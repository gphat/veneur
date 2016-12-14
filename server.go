package veneur

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"syscall"
	"time"

	"github.com/DataDog/datadog-go/statsd"
	"github.com/Sirupsen/logrus"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/s3/s3iface"
	"github.com/getsentry/raven-go"
	"github.com/golang/protobuf/proto"
	"github.com/stripe/veneur/ssf"
	"github.com/zenazn/goji/bind"
	"github.com/zenazn/goji/graceful"

	"github.com/pkg/profile"

	"github.com/stripe/veneur/plugins"
	localfilep "github.com/stripe/veneur/plugins/localfile"
	s3p "github.com/stripe/veneur/plugins/s3"
	"github.com/stripe/veneur/samplers"
	"github.com/stripe/veneur/trace"
)

// VERSION stores the current veneur version.
// It must be a var so it can be set at link time.
var VERSION = "dirty"

// REDACTED should be a constant since we use it enough.
const REDACTED = "REDACTED"

var profileStartOnce = sync.Once{}

var log = logrus.New()

// A Server is the actual veneur instance that will be run.
type Server struct {
	Workers     []*Worker
	EventWorker *EventWorker
	TraceWorker *TraceWorker

	statsd *statsd.Client
	sentry *raven.Client

	Hostname string
	Tags     []string

	DDHostname     string
	DDAPIKey       string
	DDTraceAddress string
	HTTPClient     *http.Client

	HTTPAddr    string
	ForwardAddr string
	UDPAddr     *net.UDPAddr
	TraceAddr   *net.UDPAddr
	RcvbufBytes int

	HistogramPercentiles []float64

	plugins   []plugins.Plugin
	pluginMtx sync.Mutex

	enableProfiling bool

	HistogramAggregates samplers.HistogramAggregates
}

// NewFromConfig creates a new veneur server from a configuration specification.
func NewFromConfig(conf Config) (ret Server, err error) {
	ret.Hostname = conf.Hostname
	ret.Tags = conf.Tags
	ret.DDHostname = conf.APIHostname
	ret.DDAPIKey = conf.Key
	ret.DDTraceAddress = conf.TraceAPIAddress
	ret.HistogramPercentiles = conf.Percentiles
	if len(conf.Aggregates) == 0 {
		ret.HistogramAggregates.Value = samplers.AggregateMin + samplers.AggregateMax + samplers.AggregateCount
		ret.HistogramAggregates.Count = 3
	} else {
		ret.HistogramAggregates.Value = 0
		for _, agg := range conf.Aggregates {
			ret.HistogramAggregates.Value += samplers.AggregatesLookup[agg]
		}
		ret.HistogramAggregates.Count = len(conf.Aggregates)
	}

	interval, err := time.ParseDuration(conf.Interval)
	if err != nil {
		return
	}
	ret.HTTPClient = &http.Client{
		// make sure that POSTs to datadog do not overflow the flush interval
		Timeout: interval * 9 / 10,
		// we're fine with using the default transport and redirect behavior
	}

	ret.statsd, err = statsd.NewBuffered(conf.StatsAddress, 1024)
	if err != nil {
		return
	}
	ret.statsd.Namespace = "veneur."
	ret.statsd.Tags = append(ret.Tags, "veneurlocalonly")

	// nil is a valid sentry client that noops all methods, if there is no DSN
	// we can just leave it as nil
	if conf.SentryDsn != "" {
		ret.sentry, err = raven.New(conf.SentryDsn)
		if err != nil {
			return
		}
	}

	if conf.Debug {
		log.Level = logrus.DebugLevel
	}

	if conf.EnableProfiling {
		ret.enableProfiling = true
	}

	log.Hooks.Add(sentryHook{
		c:        ret.sentry,
		hostname: ret.Hostname,
		lv: []logrus.Level{
			logrus.ErrorLevel,
			logrus.FatalLevel,
			logrus.PanicLevel,
		},
	})
	log.WithField("version", VERSION).Info("Starting server")

	log.WithField("number", conf.NumWorkers).Info("Starting workers")
	ret.Workers = make([]*Worker, conf.NumWorkers)
	for i := range ret.Workers {
		ret.Workers[i] = NewWorker(i+1, ret.statsd, log)
		// do not close over loop index
		go func(w *Worker) {
			defer func() {
				ret.ConsumePanic(recover())
			}()
			w.Work()
		}(ret.Workers[i])
	}

	ret.EventWorker = NewEventWorker(ret.statsd)
	go func() {
		defer func() {
			ret.ConsumePanic(recover())
		}()
		ret.EventWorker.Work()
	}()

	ret.UDPAddr, err = net.ResolveUDPAddr("udp", conf.UdpAddress)
	if err != nil {
		return
	}
	ret.RcvbufBytes = conf.ReadBufferSizeBytes
	ret.HTTPAddr = conf.HTTPAddress
	ret.ForwardAddr = conf.ForwardAddress

	conf.Key = REDACTED
	conf.SentryDsn = REDACTED
	log.WithField("config", conf).Debug("Initialized server")

	if len(conf.TraceAddress) > 0 && len(conf.TraceAPIAddress) > 0 {

		ret.TraceWorker = NewTraceWorker(ret.statsd)
		go func() {
			defer func() {
				ret.ConsumePanic(recover())
			}()
			ret.TraceWorker.Work()
		}()

		ret.TraceAddr, err = net.ResolveUDPAddr("udp", conf.TraceAddress)
		log.WithField("traceaddr", ret.TraceAddr).Info("Set trace address")
		if err == nil && ret.TraceAddr == nil {
			err = errors.New("resolved nil UDP address")
		}
		if err != nil {
			return
		}
	} else {
		trace.Disabled = true
	}

	var svc s3iface.S3API
	awsID := conf.AwsAccessKeyID
	awsSecret := conf.AwsSecretAccessKey

	conf.AwsAccessKeyID = REDACTED
	conf.AwsSecretAccessKey = REDACTED

	if len(awsID) > 0 && len(awsSecret) > 0 {
		sess, err := session.NewSession(&aws.Config{
			Region:      aws.String(conf.AwsRegion),
			Credentials: credentials.NewStaticCredentials(awsID, awsSecret, ""),
		})

		if err != nil {
			log.Info("error getting AWS session: %s", err)
			svc = nil
		} else {
			log.Info("Successfully created AWS session")
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
		log.Info("AWS credentials not found")
	}

	if svc == nil {
		log.Info("S3 archives are disabled")
	} else {
		log.Info("S3 archives are enabled")
	}

	if conf.FlushFile != "" {
		localFilePlugin := &localfilep.Plugin{
			FilePath: conf.FlushFile,
			Logger:   log,
		}
		ret.registerPlugin(localFilePlugin)
		log.Info(fmt.Sprintf("Local file logging to %s", conf.FlushFile))
	}

	return
}

// HandleMetricPacket processes each packet that is sent to the server, and sends to an
// appropriate worker (EventWorker or Worker).
func (s *Server) HandleMetricPacket(packet []byte) {
	// This is a very performance-sensitive function
	// and packets may be dropped if it gets slowed down.
	// Keep that in mind when modifying!

	if len(packet) == 0 {
		// a lot of clients send packets that accidentally have a trailing
		// newline, it's easier to just let them be
		return
	}

	if bytes.HasPrefix(packet, []byte{'_', 'e', '{'}) {
		event, err := samplers.ParseEvent(packet)
		if err != nil {
			log.WithFields(logrus.Fields{
				logrus.ErrorKey: err,
				"packet":        string(packet),
			}).Error("Could not parse packet")
			s.statsd.Count("packet.error_total", 1, []string{"packet_type:event"}, 1.0)
			return
		}
		s.EventWorker.EventChan <- *event
	} else if bytes.HasPrefix(packet, []byte{'_', 's', 'c'}) {
		svcheck, err := samplers.ParseServiceCheck(packet)
		if err != nil {
			log.WithFields(logrus.Fields{
				logrus.ErrorKey: err,
				"packet":        string(packet),
			}).Error("Could not parse packet")
			s.statsd.Count("packet.error_total", 1, []string{"packet_type:service_check"}, 1.0)
			return
		}
		s.EventWorker.ServiceCheckChan <- *svcheck
	} else {
		metric, err := samplers.ParseMetric(packet)
		if err != nil {
			log.WithFields(logrus.Fields{
				logrus.ErrorKey: err,
				"packet":        string(packet),
			}).Error("Could not parse packet")
			s.statsd.Count("packet.error_total", 1, []string{"packet_type:metric"}, 1.0)
			return
		}
		s.Workers[metric.Digest%uint32(len(s.Workers))].PacketChan <- *metric
	}
}

func (s *Server) HandleTracePacket(packet []byte) {
	// Unlike metrics, protobuf shouldn't have an issue with 0-length packets
	if len(packet) == 0 {
		log.Error("received zero-length trace packet")
	}

	// Technically this could be anything, but we're only consuming trace spans
	// for now.
	newSample := &ssf.SSFSample{}
	err := proto.Unmarshal(packet, newSample)
	if err != nil {
		log.WithError(err).Error("Trace unmarshaling error")
		return
	}

	s.TraceWorker.TraceChan <- *newSample
}

// ReadMetricSocket listens for available packets to handle.
func (s *Server) ReadMetricSocket(packetPool *sync.Pool, reuseport bool) {
	// each goroutine gets its own socket
	// if the sockets support SO_REUSEPORT, then this will cause the
	// kernel to distribute datagrams across them, for better read
	// performance
	serverConn, err := NewSocket(s.UDPAddr, s.RcvbufBytes, reuseport)
	if err != nil {
		// if any goroutine fails to create the socket, we can't really
		// recover, so we just blow up
		// this probably indicates a systemic issue, eg lack of
		// SO_REUSEPORT support
		log.WithError(err).Fatal("Error listening for UDP metrics")
	}
	log.WithField("address", s.UDPAddr).Info("Listening for UDP metrics")

	for {
		buf := packetPool.Get().([]byte)
		n, _, err := serverConn.ReadFrom(buf)
		if err != nil {
			log.WithError(err).Error("Error reading from UDP metrics socket")
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

// ReadTraceSocket listens for available packets to handle.
func (s *Server) ReadTraceSocket(packetPool *sync.Pool, reuseport bool) {
	// TODO This is duplicated from ReadMetricSocket and feels like it could be it's
	// own function?

	if s.TraceAddr == nil {
		log.WithField("s.TraceAddr", s.TraceAddr).Fatal("Cannot listen on nil trace address")
	}

	serverConn, err := NewSocket(s.TraceAddr, s.RcvbufBytes, reuseport)
	if err != nil {
		// if any goroutine fails to create the socket, we can't really
		// recover, so we just blow up
		// this probably indicates a systemic issue, eg lack of
		// SO_REUSEPORT support
		log.WithError(err).Fatal("Error listening for UDP traces")
	}
	log.WithField("address", s.TraceAddr).Info("Listening for UDP traces")

	for {
		buf := packetPool.Get().([]byte)
		n, _, err := serverConn.ReadFrom(buf)
		if err != nil {
			log.WithError(err).Error("Error reading from UDP trace socket")
			continue
		}

		s.HandleTracePacket(buf[:n])
		packetPool.Put(buf)
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

func (s *Server) TracingEnabled() bool {
	return s.TraceWorker != nil
}
