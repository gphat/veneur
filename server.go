package veneur

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/DataDog/datadog-go/statsd"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/s3/s3iface"
	"github.com/getsentry/raven-go"
	"github.com/sirupsen/logrus"
	"github.com/zenazn/goji/bind"
	"github.com/zenazn/goji/graceful"

	"github.com/pkg/profile"

	"github.com/stripe/veneur/plugins"
	localfilep "github.com/stripe/veneur/plugins/localfile"
	s3p "github.com/stripe/veneur/plugins/s3"
	"github.com/stripe/veneur/protocol"
	"github.com/stripe/veneur/samplers"
	"github.com/stripe/veneur/sinks"
	"github.com/stripe/veneur/sinks/datadog"
	"github.com/stripe/veneur/sinks/kafka"
	"github.com/stripe/veneur/sinks/lightstep"
	"github.com/stripe/veneur/sinks/signalfx"
	"github.com/stripe/veneur/sinks/ssfmetrics"
	"github.com/stripe/veneur/sinks/xray"
	"github.com/stripe/veneur/ssf"
	"github.com/stripe/veneur/trace"
	"github.com/stripe/veneur/trace/metrics"
)

// VERSION stores the current veneur version.
// It must be a var so it can be set at link time.
var VERSION = defaultLinkValue

var BUILD_DATE = defaultLinkValue

const defaultLinkValue = "dirty"

// REDACTED is used to replace values that we don't want to leak into loglines (e.g., credentials)
const REDACTED = "REDACTED"

var profileStartOnce = sync.Once{}

var log = logrus.StandardLogger()

var tracer = trace.GlobalTracer

const defaultTCPReadTimeout = 10 * time.Minute

// A Server is the actual veneur instance that will be run.
type Server struct {
	Workers              []*Worker
	EventWorker          *EventWorker
	SpanChan             chan *ssf.SSFSpan
	SpanWorker           *SpanWorker
	SpanWorkerGoroutines int

	Sentry *raven.Client

	Hostname  string
	Tags      []string
	TagsAsMap map[string]string

	HTTPClient *http.Client

	HTTPAddr string

	ForwardAddr string

	StatsdListenAddrs []net.Addr
	SSFListenAddrs    []net.Addr
	RcvbufBytes       int

	interval            time.Duration
	synchronizeInterval bool
	numReaders          int
	metricMaxLength     int
	traceMaxLengthBytes int

	tlsConfig      *tls.Config
	tcpReadTimeout time.Duration

	// closed when the server is shutting down gracefully
	shutdown chan struct{}

	HistogramPercentiles []float64

	plugins   []plugins.Plugin
	pluginMtx sync.Mutex

	enableProfiling bool

	HistogramAggregates samplers.HistogramAggregates

	spanSinks   []sinks.SpanSink
	metricSinks []sinks.MetricSink

	TraceClient *trace.Client
}

// SetLogger sets the default logger in veneur to the passed value.
func SetLogger(logger *logrus.Logger) {
	log = logger
}

// NewFromConfig creates a new veneur server from a configuration
// specification and sets up the passed logger according to the
// configuration.
func NewFromConfig(logger *logrus.Logger, conf Config) (*Server, error) {
	ret := &Server{}

	ret.Hostname = conf.Hostname
	ret.Tags = conf.Tags

	mappedTags := make(map[string]string)
	for _, tag := range ret.Tags {
		splt := strings.SplitN(tag, ":", 2)
		if len(splt) < 2 {
			mappedTags[splt[0]] = ""
		} else {
			mappedTags[splt[0]] = splt[1]
		}
	}

	ret.synchronizeInterval = conf.SynchronizeWithInterval

	ret.TagsAsMap = mappedTags
	ret.HistogramPercentiles = conf.Percentiles
	ret.HistogramAggregates.Value = 0
	for _, agg := range conf.Aggregates {
		ret.HistogramAggregates.Value += samplers.AggregatesLookup[agg]
	}
	ret.HistogramAggregates.Count = len(conf.Aggregates)

	var err error
	ret.interval, err = conf.ParseInterval()
	if err != nil {
		return ret, err
	}

	transport := &http.Transport{
		IdleConnTimeout: ret.interval * 2, // If we're idle more than one interval something is up
	}

	ret.HTTPClient = &http.Client{
		// make sure that POSTs to datadog do not overflow the flush interval
		Timeout:   ret.interval * 9 / 10,
		Transport: transport,
	}

	stats, err := statsd.NewBuffered(conf.StatsAddress, 4096)
	if err != nil {
		return ret, err
	}
	stats.Namespace = "veneur."

	ret.SpanChan = make(chan *ssf.SSFSpan, conf.SpanChannelCapacity)
	ret.TraceClient, err = trace.NewChannelClient(ret.SpanChan,
		trace.ReportStatistics(stats, 1*time.Second, []string{"ssf_format:internal"}),
	)
	if err != nil {
		return ret, err
	}

	// nil is a valid sentry client that noops all methods, if there is no DSN
	// we can just leave it as nil
	if conf.SentryDsn != "" {
		ret.Sentry, err = raven.New(conf.SentryDsn)
		if err != nil {
			return ret, err
		}
	}

	if conf.Debug {
		logger.SetLevel(logrus.DebugLevel)
	}

	if conf.EnableProfiling {
		ret.enableProfiling = true
	}

	// This is a check to ensure that we don't repeatedly add a hook
	// to the "global" log instance on repeated calls to `NewFromConfig`
	// such as those made in testing. By skipping this we avoid a race
	// condition in logrus discussed here:
	// https://github.com/sirupsen/logrus/issues/295
	if _, ok := logger.Hooks[logrus.FatalLevel]; !ok {
		logger.AddHook(sentryHook{
			c:        ret.Sentry,
			hostname: ret.Hostname,
			lv: []logrus.Level{
				logrus.ErrorLevel,
				logrus.FatalLevel,
				logrus.PanicLevel,
			},
		})
	}

	// After log hooks are configured, if any further errors are
	// found during setup we should use logger.Fatal or
	// logger.Panic, because that will give us breakage monitoring
	// through Sentry.
	numWorkers := 1
	if conf.NumWorkers > 1 {
		numWorkers = conf.NumWorkers
	}
	logger.WithField("number", numWorkers).Info("Preparing workers")
	// Allocate the slice, we'll fill it with workers later.
	ret.Workers = make([]*Worker, numWorkers)
	ret.numReaders = conf.NumReaders

	// Use the pre-allocated Workers slice to know how many to start.
	for i := range ret.Workers {
		ret.Workers[i] = NewWorker(i+1, ret.TraceClient, log)
		// do not close over loop index
		go func(w *Worker) {
			defer func() {
				ConsumePanic(ret.Sentry, ret.TraceClient, ret.Hostname, recover())
			}()
			w.Work()
		}(ret.Workers[i])
	}

	ret.EventWorker = NewEventWorker(ret.TraceClient)

	// Set up a span sink that extracts metrics from SSF spans and
	// reports them via the metric workers:
	processors := make([]ssfmetrics.Processor, len(ret.Workers))
	for i, w := range ret.Workers {
		processors[i] = w
	}
	metricSink, err := ssfmetrics.NewMetricExtractionSink(processors, conf.IndicatorSpanTimerName, ret.TraceClient, log)
	if err != nil {
		return ret, err
	}
	ret.spanSinks = append(ret.spanSinks, metricSink)

	for _, addrStr := range conf.StatsdListenAddresses {
		addr, err := protocol.ResolveAddr(addrStr)
		if err != nil {
			return ret, err
		}
		ret.StatsdListenAddrs = append(ret.StatsdListenAddrs, addr)
	}
	for _, addrStr := range conf.SsfListenAddresses {
		addr, err := protocol.ResolveAddr(addrStr)
		if err != nil {
			return ret, err
		}
		ret.SSFListenAddrs = append(ret.SSFListenAddrs, addr)
	}

	ret.metricMaxLength = conf.MetricMaxLength
	ret.traceMaxLengthBytes = conf.TraceMaxLengthBytes
	ret.RcvbufBytes = conf.ReadBufferSizeBytes
	ret.HTTPAddr = conf.HTTPAddress
	ret.ForwardAddr = conf.ForwardAddress

	if conf.TLSKey != "" {
		if conf.TLSCertificate == "" {
			err = errors.New("tls_key is set; must set tls_certificate")
			logger.WithError(err).Error("Improper TLS configuration")
			return ret, err
		}

		// load the TLS key and certificate
		var cert tls.Certificate
		cert, err = tls.X509KeyPair([]byte(conf.TLSCertificate), []byte(conf.TLSKey))
		if err != nil {
			logger.WithError(err).Error("Improper TLS configuration")
			return ret, err
		}

		clientAuthMode := tls.NoClientCert
		var clientCAs *x509.CertPool
		if conf.TLSAuthorityCertificate != "" {
			// load the authority; require clients to present certificated signed by this authority
			clientAuthMode = tls.RequireAndVerifyClientCert
			clientCAs = x509.NewCertPool()
			ok := clientCAs.AppendCertsFromPEM([]byte(conf.TLSAuthorityCertificate))
			if !ok {
				err = errors.New("tls_authority_certificate: Could not load any certificates")
				logger.WithError(err).Error("Improper TLS configuration")
				return ret, err
			}
		}

		ret.tlsConfig = &tls.Config{
			Certificates: []tls.Certificate{cert},
			ClientAuth:   clientAuthMode,
			ClientCAs:    clientCAs,
		}
	}

	if conf.SignalfxAPIKey != "" {
		fallback := signalfx.NewClient(conf.SignalfxEndpointBase, conf.SignalfxAPIKey)
		byTagClients := map[string]signalfx.DPClient{}
		for _, perTag := range conf.SignalfxPerTagAPIKeys {
			byTagClients[perTag.Name] = signalfx.NewClient(conf.SignalfxEndpointBase, perTag.APIKey)
		}
		sfxSink, err := signalfx.NewSignalFxSink(conf.SignalfxHostnameTag, conf.Hostname, ret.TagsAsMap, log, fallback, conf.SignalfxVaryKeyBy, byTagClients)
		if err != nil {
			return ret, err
		}
		ret.metricSinks = append(ret.metricSinks, sfxSink)
	}
	if conf.DatadogAPIKey != "" && conf.DatadogAPIHostname != "" {
		ddSink, err := datadog.NewDatadogMetricSink(
			ret.interval.Seconds(), conf.DatadogFlushMaxPerBody, conf.Hostname, ret.Tags,
			conf.DatadogAPIHostname, conf.DatadogAPIKey, ret.HTTPClient, log,
		)
		if err != nil {
			return ret, err
		}
		ret.metricSinks = append(ret.metricSinks, ddSink)
	}

	// Configure tracing sinks
	if len(conf.SsfListenAddresses) > 0 {

		trace.Enable()

		// configure Datadog as a Span sink
		if conf.DatadogAPIKey != "" && conf.DatadogTraceAPIAddress != "" {
			ddSink, err := datadog.NewDatadogSpanSink(
				conf.DatadogTraceAPIAddress, conf.DatadogSpanBufferSize,
				ret.HTTPClient, ret.TagsAsMap, log,
			)
			if err != nil {
				return ret, err
			}

			ret.spanSinks = append(ret.spanSinks, ddSink)
			logger.Info("Configured Datadog span sink")
		}

		if conf.XrayAddress != "" {
			if conf.XraySamplePercentage == 0 {
				log.Warn("XRay sample percentage is 0, no segments will be sent.")
			}
			xraySink, err := xray.NewXRaySpanSink(conf.XrayAddress, conf.XraySamplePercentage, ret.TagsAsMap, log)
			if err != nil {
				return ret, err
			}
			ret.spanSinks = append(ret.spanSinks, xraySink)

			logger.Info("Configured X-Ray span sink")
		}

		// configure Lightstep as a Span Sink
		if conf.LightstepAccessToken != "" {

			var lsSink sinks.SpanSink
			lsSink, err = lightstep.NewLightStepSpanSink(
				conf.LightstepCollectorHost, conf.LightstepReconnectPeriod,
				conf.LightstepMaximumSpans, conf.LightstepNumClients,
				conf.LightstepAccessToken, ret.TagsAsMap, log,
			)
			if err != nil {
				return ret, err
			}
			ret.spanSinks = append(ret.spanSinks, lsSink)

			logger.Info("Configured Lightstep span sink")
		}
		// Set up as many span workers as we need:
		ret.SpanWorkerGoroutines = 1
		if conf.NumSpanWorkers > 0 {
			ret.SpanWorkerGoroutines = conf.NumSpanWorkers
		}
	}

	if conf.KafkaBroker != "" {
		if conf.KafkaMetricTopic != "" || conf.KafkaCheckTopic != "" || conf.KafkaEventTopic != "" {
			kSink, err := kafka.NewKafkaMetricSink(
				log, ret.TraceClient, conf.KafkaBroker, conf.KafkaCheckTopic, conf.KafkaEventTopic,
				conf.KafkaMetricTopic, conf.KafkaMetricRequireAcks,
				conf.KafkaPartitioner, conf.KafkaRetryMax,
				conf.KafkaMetricBufferBytes, conf.KafkaMetricBufferMessages,
				conf.KafkaMetricBufferFrequency,
			)
			if err != nil {
				return ret, err
			}

			ret.metricSinks = append(ret.metricSinks, kSink)

			logger.Info("Configured Kafka metric sink")
		} else {
			logger.Warn("Kafka metric sink skipped due to missing metric, check and event topic")
		}

		if conf.KafkaSpanTopic != "" {
			sink, err := kafka.NewKafkaSpanSink(log, ret.TraceClient, conf.KafkaBroker, conf.KafkaSpanTopic,
				conf.KafkaPartitioner, conf.KafkaMetricRequireAcks, conf.KafkaRetryMax,
				conf.KafkaSpanBufferBytes, conf.KafkaSpanBufferMesages,
				conf.KafkaSpanBufferFrequency, conf.KafkaSpanSerializationFormat,
				conf.KafkaSpanSampleTag, conf.KafkaSpanSampleRatePercent,
			)
			if err != nil {
				return ret, err
			}

			ret.spanSinks = append(ret.spanSinks, sink)
			logger.Info("Configured Kafka span sink")
		} else {
			logger.Warn("Kafka span sink skipped due to missing span topic")
		}
	}

	// After all sinks are initialized, set the list of tags to exclude
	setSinkExcludedTags(conf.TagsExclude, ret.metricSinks)

	var svc s3iface.S3API
	awsID := conf.AwsAccessKeyID
	awsSecret := conf.AwsSecretAccessKey

	if len(awsID) > 0 && len(awsSecret) > 0 {
		sess, err := session.NewSession(&aws.Config{
			Region:      aws.String(conf.AwsRegion),
			Credentials: credentials.NewStaticCredentials(awsID, awsSecret, ""),
		})

		if err != nil {
			logger.Info("error getting AWS session: %s", err)
			svc = nil
		} else {
			logger.Info("Successfully created AWS session")
			svc = s3.New(sess)

			plugin := &s3p.S3Plugin{
				Logger:   log,
				Svc:      svc,
				S3Bucket: conf.AwsS3Bucket,
				Hostname: ret.Hostname,
			}
			ret.registerPlugin(plugin)
		}
	} else {
		logger.Info("AWS credentials not found")
	}

	if svc == nil {
		logger.Info("S3 archives are disabled")
	} else {
		logger.Info("S3 archives are enabled")
	}

	if conf.FlushFile != "" {
		localFilePlugin := &localfilep.Plugin{
			FilePath: conf.FlushFile,
			Logger:   log,
		}
		ret.registerPlugin(localFilePlugin)
		logger.Info(fmt.Sprintf("Local file logging to %s", conf.FlushFile))
	}

	// closed in Shutdown; Same approach and http.Shutdown
	ret.shutdown = make(chan struct{})

	// Don't emit keys into logs now that we're done with them.
	conf.SentryDsn = REDACTED
	conf.TLSKey = REDACTED
	conf.DatadogAPIKey = REDACTED
	conf.SignalfxAPIKey = REDACTED
	conf.LightstepAccessToken = REDACTED
	conf.AwsAccessKeyID = REDACTED
	conf.AwsSecretAccessKey = REDACTED

	logger.WithField("config", conf).Debug("Initialized server")

	return ret, err
}

// Start spins up the Server to do actual work, firing off goroutines for
// various workers and utilities.
func (s *Server) Start() {
	log.WithField("version", VERSION).Info("Starting server")

	// Set up the processors for spans:

	// Use the pre-allocated Workers slice to know how many to start.
	s.SpanWorker = NewSpanWorker(s.spanSinks, s.TraceClient, s.SpanChan)

	go func() {
		log.Info("Starting Event worker")
		defer func() {
			ConsumePanic(s.Sentry, s.TraceClient, s.Hostname, recover())
		}()
		s.EventWorker.Work()
	}()

	log.WithField("n", s.SpanWorkerGoroutines).Info("Starting span workers")
	for i := 0; i < s.SpanWorkerGoroutines; i++ {
		go func() {
			defer func() {
				ConsumePanic(s.Sentry, s.TraceClient, s.Hostname, recover())
			}()
			s.SpanWorker.Work()
		}()
	}

	statsdPool := &sync.Pool{
		// We +1 this so we an "detect" when someone sends us too long of a metric!
		New: func() interface{} {
			return make([]byte, s.metricMaxLength+1)
		},
	}

	tracePool := &sync.Pool{
		New: func() interface{} {
			return make([]byte, s.traceMaxLengthBytes)
		},
	}

	for _, sink := range s.spanSinks {
		logrus.WithField("sink", sink.Name()).Info("Starting span sink")
		if err := sink.Start(s.TraceClient); err != nil {
			logrus.WithError(err).WithField("sink", sink).Panic("Error starting span sink")
		}
	}

	for _, sink := range s.metricSinks {
		logrus.WithField("sink", sink.Name()).Info("Starting metric sink")
		if err := sink.Start(s.TraceClient); err != nil {
			logrus.WithError(err).WithField("sink", sink).Fatal("Error starting metric sink")
		}
	}

	// Read Metrics Forever!
	concreteAddrs := make([]net.Addr, 0, len(s.StatsdListenAddrs))
	for _, addr := range s.StatsdListenAddrs {
		concreteAddrs = append(concreteAddrs, StartStatsd(s, addr, statsdPool))
	}
	s.StatsdListenAddrs = concreteAddrs

	// Read Traces Forever!
	if len(s.SSFListenAddrs) > 0 {
		concreteAddrs := make([]net.Addr, 0, len(s.StatsdListenAddrs))
		for _, addr := range s.SSFListenAddrs {
			concreteAddrs = append(concreteAddrs, StartSSF(s, addr, tracePool))
		}
		s.SSFListenAddrs = concreteAddrs
	} else {
		logrus.Info("Tracing sockets are not configured - not reading trace socket")
	}

	// Flush every Interval forever!
	go func() {
		defer func() {
			ConsumePanic(s.Sentry, s.TraceClient, s.Hostname, recover())
		}()

		if s.synchronizeInterval {
			// We want to align our ticker to a multiple of its duration for
			// convenience of bucketing.
			<-time.After(CalculateTickDelay(s.interval, time.Now()))
		}

		// We aligned the ticker to our interval above. It's worth noting that just
		// because we aligned once we're not guaranteed to be perfect on each
		// subsequent tick. This code is small, however, and should service the
		// incoming tick signal fast enough that the amount we are "off" is
		// negligible.
		ticker := time.NewTicker(s.interval)
		for {
			select {
			case <-s.shutdown:
				// stop flushing on graceful shutdown
				ticker.Stop()
				return
			case <-ticker.C:
				s.Flush(context.TODO())
			}
		}
	}()
}

// HandleMetricPacket processes each packet that is sent to the server, and sends to an
// appropriate worker (EventWorker or Worker).
func (s *Server) HandleMetricPacket(packet []byte) error {
	// This is a very performance-sensitive function
	// and packets may be dropped if it gets slowed down.
	// Keep that in mind when modifying!

	if len(packet) == 0 {
		// a lot of clients send packets that accidentally have a trailing
		// newline, it's easier to just let them be
		return nil
	}
	samples := &ssf.Samples{}
	defer metrics.Report(s.TraceClient, samples)

	if bytes.HasPrefix(packet, []byte{'_', 'e', '{'}) {
		event, err := samplers.ParseEvent(packet)
		if err != nil {
			log.WithFields(logrus.Fields{
				logrus.ErrorKey: err,
				"packet":        string(packet),
			}).Warn("Could not parse packet")
			samples.Add(ssf.Count("packet.error_total", 1, map[string]string{"packet_type": "event", "reason": "parse"}))
			return err
		}
		s.EventWorker.EventChan <- *event
	} else if bytes.HasPrefix(packet, []byte{'_', 's', 'c'}) {
		svcheck, err := samplers.ParseServiceCheck(packet)
		if err != nil {
			log.WithFields(logrus.Fields{
				logrus.ErrorKey: err,
				"packet":        string(packet),
			}).Warn("Could not parse packet")
			samples.Add(ssf.Count("packet.error_total", 1, map[string]string{"packet_type": "service_check", "reason": "parse"}))
			return err
		}
		s.EventWorker.ServiceCheckChan <- *svcheck
	} else {
		metric, err := samplers.ParseMetric(packet)
		if err != nil {
			log.WithFields(logrus.Fields{
				logrus.ErrorKey: err,
				"packet":        string(packet),
			}).Warn("Could not parse packet")
			samples.Add(ssf.Count("packet.error_total", 1, map[string]string{"packet_type": "metric", "reason": "parse"}))
			return err
		}
		s.Workers[metric.Digest%uint32(len(s.Workers))].PacketChan <- *metric
	}
	return nil
}

// HandleTracePacket accepts an incoming packet as bytes and sends it to the
// appropriate worker.
func (s *Server) HandleTracePacket(packet []byte) {
	samples := &ssf.Samples{}
	defer metrics.Report(s.TraceClient, samples)

	samples.Add(ssf.Count("ssf.received_total", 1, nil))
	// Unlike metrics, protobuf shouldn't have an issue with 0-length packets
	if len(packet) == 0 {
		samples.Add(ssf.Count("ssf.error_total", 1, map[string]string{"ssf_format": "packet", "packet_type": "unknown", "reason": "zerolength"}))
		log.Warn("received zero-length trace packet")
		return
	}

	samples.Add(ssf.Histogram("ssf.packet_size", float32(len(packet)), nil))

	span, err := protocol.ParseSSF(packet)
	if err != nil {
		samples.Add(ssf.Count("ssf.error_total", 1, map[string]string{"ssf_format": "packet", "packet_type": "ssf_metric", "reason": err.Error()}))
		log.WithError(err).Warn("ParseSSF")
		return
	}
	s.handleSSF(span, map[string]string{"ssf_format": "packet"})
}

func (s *Server) handleSSF(span *ssf.SSFSpan, baseTags map[string]string) {
	tags := map[string]string{}
	for k, v := range baseTags {
		tags[k] = v
	}
	tags["service"] = span.Service
	defer metrics.ReportBatch(s.TraceClient, []*ssf.SSFSample{
		ssf.Count("ssf.spans.received_total", 1, tags),
		ssf.Histogram("ssf.spans.tags_per_span", float32(len(span.Tags)), tags),
	})

	s.SpanChan <- span
}

// ReadMetricSocket listens for available packets to handle.
func (s *Server) ReadMetricSocket(serverConn net.PacketConn, packetPool *sync.Pool) {
	for {
		buf := packetPool.Get().([]byte)
		n, _, err := serverConn.ReadFrom(buf)
		if err != nil {
			log.WithError(err).Error("Error reading from UDP metrics socket")
			continue
		}
		if n > s.metricMaxLength {
			metrics.ReportOne(s.TraceClient, ssf.Count("packet.error_total", 1, map[string]string{"packet_type": "unknown", "reason": "toolong"}))
			continue
		}

		// statsd allows multiple packets to be joined by newlines and sent as
		// one larger packet
		// note that spurious newlines are not allowed in this format, it has
		// to be exactly one newline between each packet, with no leading or
		// trailing newlines
		splitPacket := samplers.NewSplitBytes(buf[:n], '\n')
		for splitPacket.Next() {
			s.HandleMetricPacket(splitPacket.Chunk())
		}

		// the Metric struct created by HandleMetricPacket has no byte slices in it,
		// only strings
		// therefore there are no outstanding references to this byte slice, we
		// can return it to the pool
		packetPool.Put(buf)
	}
}

// ReadSSFPacketSocket reads SSF packets off a packet connection.
func (s *Server) ReadSSFPacketSocket(serverConn net.PacketConn, packetPool *sync.Pool) {
	// TODO This is duplicated from ReadMetricSocket and feels like it could be it's
	// own function?
	p := packetPool.Get().([]byte)
	if len(p) == 0 {
		log.WithField("len", len(p)).Fatal(
			"packetPool making empty slices: trace_max_length_bytes must be >= 0")
	}
	packetPool.Put(p)

	for {
		buf := packetPool.Get().([]byte)
		n, _, err := serverConn.ReadFrom(buf)
		if err != nil {
			// In tests, the probably-best way to
			// terminate this reader is to issue a shutdown and close the listening
			// socket, which returns an error, so let's handle it here:
			select {
			case <-s.shutdown:
				log.WithError(err).Info("Ignoring ReadFrom error while shutting down")
				return
			default:
				log.WithError(err).Error("Error reading from UDP trace socket")
				continue
			}
		}

		s.HandleTracePacket(buf[:n])
		packetPool.Put(buf)
	}
}

// ReadSSFStreamSocket reads a streaming connection in framed wire format
// off a streaming socket. See package
// github.com/stripe/veneur/protocol for details.
func (s *Server) ReadSSFStreamSocket(serverConn net.Conn) {
	defer func() {
		serverConn.Close()
	}()
	tags := map[string]string{"ssf_format": "framed"}
	for {
		msg, err := protocol.ReadSSF(serverConn)
		if err != nil {
			if err == io.EOF {
				// Client hangup, close this
				metrics.ReportOne(s.TraceClient, ssf.Count("frames.disconnects", 1, nil))
				return
			}
			if protocol.IsFramingError(err) {
				log.WithError(err).
					WithField("remote", serverConn.RemoteAddr()).
					Info("Frame error reading from SSF connection. Closing.")
				sample := ssf.Count("ssf.error_total", 1, tags)
				sample.Tags["packet_type"] = "unknown"
				sample.Tags["reason"] = "framing"
				metrics.ReportOne(s.TraceClient, sample)
				return
			}
			// Non-frame errors means we can continue reading:
			log.WithError(err).
				WithField("remote", serverConn.RemoteAddr()).
				Error("Error processing an SSF frame")
			sample := ssf.Count("ssf.error_total", 1, tags)
			sample.Tags["packet_type"] = "unknown"
			sample.Tags["reason"] = "processing"
			metrics.ReportOne(s.TraceClient, sample)
			continue
		}
		metrics.ReportBatch(s.TraceClient,
			ssf.RandomlySample(0.01, ssf.Count("ssf.received_total", 1, tags)))
		s.handleSSF(msg, tags)
	}
}

func (s *Server) handleTCPGoroutine(conn net.Conn) {
	defer func() {
		ConsumePanic(s.Sentry, s.TraceClient, s.Hostname, recover())
	}()

	defer func() {
		log.WithField("peer", conn.RemoteAddr()).Debug("Closing TCP connection")
		err := conn.Close()
		metrics.ReportOne(s.TraceClient, ssf.Count("tcp.disconnects", 1, nil))
		if err != nil {
			// most often "write: broken pipe"; not really an error
			log.WithFields(logrus.Fields{
				logrus.ErrorKey: err,
				"peer":          conn.RemoteAddr(),
			}).Info("TCP close failed")
		}
	}()
	metrics.ReportOne(s.TraceClient, ssf.Count("tcp.connects", 1, nil))

	// time out idle connections to prevent leaking memory/goroutines
	timeout := defaultTCPReadTimeout
	if s.tcpReadTimeout != 0 {
		timeout = s.tcpReadTimeout
	}

	if tlsConn, ok := conn.(*tls.Conn); ok {
		// complete the handshake to verify the certificate
		conn.SetReadDeadline(time.Now().Add(timeout))
		err := tlsConn.Handshake()
		if err != nil {
			// usually io.EOF or "read: connection reset by peer"; not really errors
			// it can also be caused by certificate authentication problems
			metrics.ReportOne(s.TraceClient, ssf.Count("tcp.tls_handshake_failures", 1, nil))
			log.WithFields(logrus.Fields{
				logrus.ErrorKey: err,
				"peer":          conn.RemoteAddr(),
			}).Info("TLS Handshake failed")

			return
		}

		state := tlsConn.ConnectionState()
		var clientCert pkix.RDNSequence
		if len(state.PeerCertificates) > 0 {
			clientCert = state.PeerCertificates[0].Subject.ToRDNSequence()
		}
		log.WithFields(logrus.Fields{
			"peer":        conn.RemoteAddr(),
			"client_cert": clientCert,
		}).Debug("Starting TLS connection")
	} else {
		log.WithFields(logrus.Fields{
			"peer": conn.RemoteAddr(),
		}).Debug("Starting TCP connection")
	}

	// Scanner is nearly the same performance as a custom implementation
	buf := bufio.NewScanner(conn)

	scanWithDeadline := func() bool {
		conn.SetReadDeadline(time.Now().Add(timeout))
		return buf.Scan()
	}
	for scanWithDeadline() {
		// treat each line as a separate packet
		err := s.HandleMetricPacket(buf.Bytes())
		if err != nil {
			// don't consume bad data from a client indefinitely
			// HandleMetricPacket logs the err and packet, and increments error counters
			log.WithField("peer", conn.RemoteAddr()).Warn(
				"Error parsing packet; closing TCP connection")
			return
		}
	}
	if buf.Err() != nil {
		// usually "read: connection reset by peer" or "i/o timeout"
		log.WithFields(logrus.Fields{
			logrus.ErrorKey: buf.Err(),
			"peer":          conn.RemoteAddr(),
		}).Info("Error reading from TCP client")
	}
}

// ReadTCPSocket listens on Server.TCPAddr for new connections, starting a goroutine for each.
func (s *Server) ReadTCPSocket(listener net.Listener) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			select {
			case <-s.shutdown:
				// occurs when cleanly shutting down the server e.g. in tests; ignore errors
				log.WithError(err).Info("Ignoring Accept error while shutting down")
				return
			default:
				log.WithError(err).Fatal("TCP accept failed")
			}
		}

		go s.handleTCPGoroutine(conn)
	}
}

// HTTPServe starts the HTTP server and listens perpetually until it encounters an unrecoverable error.
func (s *Server) HTTPServe() {
	var prf interface {
		Stop()
	}

	// We want to make sure the profile is stopped
	// exactly once (and only once), even if the
	// shutdown pre-hook does not run (which it may not)
	profileStopOnce := sync.Once{}

	if s.enableProfiling {
		profileStartOnce.Do(func() {
			prf = profile.Start()
		})

		defer func() {
			profileStopOnce.Do(prf.Stop)
		}()
	}
	httpSocket := bind.Socket(s.HTTPAddr)
	graceful.Timeout(10 * time.Second)
	graceful.PreHook(func() {

		if prf != nil {
			profileStopOnce.Do(prf.Stop)
		}

		log.Info("Terminating HTTP listener")
	})

	// Ensure that the server responds to SIGUSR2 even
	// when *not* running under einhorn.
	graceful.AddSignal(syscall.SIGUSR2, syscall.SIGHUP)
	graceful.HandleSignals()
	log.WithField("address", s.HTTPAddr).Info("HTTP server listening")
	bind.Ready()

	if err := graceful.Serve(httpSocket, s.Handler()); err != nil {
		log.WithError(err).Error("HTTP server shut down due to error")
	}

	graceful.Shutdown()
}

// Shutdown signals the server to shut down after closing all
// current connections.
func (s *Server) Shutdown() {
	// TODO(aditya) shut down workers and socket readers
	log.Info("Shutting down server gracefully")
	close(s.shutdown)
	graceful.Shutdown()
}

// IsLocal indicates whether veneur is running as a local instance
// (forwarding non-local data to a global veneur instance) or is running as a global
// instance (sending all data directly to the final destination).
func (s *Server) IsLocal() bool {
	return s.ForwardAddr != ""
}

// registerPlugin registers a plugin for use
// on the veneur server. It is blocking
// and not threadsafe.
func (s *Server) registerPlugin(p plugins.Plugin) {
	s.pluginMtx.Lock()
	defer s.pluginMtx.Unlock()
	s.plugins = append(s.plugins, p)
}

func (s *Server) getPlugins() []plugins.Plugin {
	s.pluginMtx.Lock()
	plugins := make([]plugins.Plugin, len(s.plugins))
	copy(plugins, s.plugins)
	s.pluginMtx.Unlock()
	return plugins
}

// CalculateTickDelay takes the provided time, `Truncate`s it a rounded-down
// multiple of `interval`, then adds `interval` back to find the "next" tick.
func CalculateTickDelay(interval time.Duration, t time.Time) time.Duration {
	return t.Truncate(interval).Add(interval).Sub(t)
}

// Set the list of tags to exclude on each sink
func setSinkExcludedTags(excludeRules []string, metricSinks []sinks.MetricSink) {
	type excludableSink interface {
		SetExcludedTags([]string)
	}

	for _, sink := range metricSinks {
		if s, ok := sink.(excludableSink); ok {
			excludedTags := generateExcludedTags(excludeRules, sink.Name())
			log.WithFields(logrus.Fields{
				"sink":         sink.Name(),
				"excludedTags": excludedTags,
			}).Debug("Setting excluded tags on sink")
			s.SetExcludedTags(excludedTags)
		}
	}
}

func generateExcludedTags(excludeRules []string, sinkName string) []string {
	excludedTags := make([]string, 0, len(excludeRules))
	for _, rule := range excludeRules {
		splt := strings.Split(rule, "|")
		if len(splt) == 1 {
			excludedTags = append(excludedTags, splt[0])
			continue
		}
		for _, name := range splt[1:] {
			if name == sinkName {
				excludedTags = append(excludedTags, splt[0])
			}
		}
	}
	return excludedTags
}
