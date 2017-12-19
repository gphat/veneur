package veneur

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/pprof"
	"os"
	"strconv"
	"sync"
	"syscall"
	"time"

	"golang.org/x/net/context"

	"github.com/DataDog/datadog-go/statsd"
	raven "github.com/getsentry/raven-go"
	"github.com/hashicorp/consul/api"
	"github.com/pkg/profile"
	"github.com/sirupsen/logrus"
	vhttp "github.com/stripe/veneur/http"
	"github.com/stripe/veneur/samplers"
	"github.com/stripe/veneur/trace"
	"github.com/zenazn/goji/bind"
	"github.com/zenazn/goji/graceful"
	"stathat.com/c/consistent"

	"goji.io"
	"goji.io/pat"
)

type Proxy struct {
	Sentry                 *raven.Client
	Hostname               string
	ForwardDestinations    *consistent.Consistent
	TraceDestinations      *consistent.Consistent
	Discoverer             Discoverer
	ConsulForwardService   string
	ConsulTraceService     string
	ConsulInterval         time.Duration
	ForwardDestinationsMtx sync.Mutex
	TraceDestinationsMtx   sync.Mutex
	HTTPAddr               string
	HTTPClient             *http.Client
	Statsd                 *statsd.Client
	AcceptingForwards      bool
	AcceptingTraces        bool

	usingConsul     bool
	enableProfiling bool
	traceClient     *trace.Client
}

func NewProxyFromConfig(conf ProxyConfig) (p Proxy, err error) {

	hostname, err := os.Hostname()
	if err != nil {
		log.WithError(err).Error("Error finding hostname")
		return
	}
	p.Hostname = hostname

	log.AddHook(sentryHook{
		c:        p.Sentry,
		hostname: hostname,
		lv: []logrus.Level{
			logrus.ErrorLevel,
			logrus.FatalLevel,
			logrus.PanicLevel,
		},
	})

	p.HTTPAddr = conf.HTTPAddress
	// TODO Timeout?
	p.HTTPClient = &http.Client{}

	p.Statsd, err = statsd.NewBuffered(conf.StatsAddress, 1024)
	if err != nil {
		log.WithError(err).Error("Failed to create stats instance")
		return
	}
	p.Statsd.Namespace = "veneur_proxy."

	p.enableProfiling = conf.EnableProfiling

	p.ConsulForwardService = conf.ConsulForwardServiceName
	p.ConsulTraceService = conf.ConsulTraceServiceName

	if p.ConsulForwardService != "" || conf.ForwardAddress != "" {
		p.AcceptingForwards = true
	}
	if p.ConsulTraceService != "" || conf.TraceAddress != "" {
		p.AcceptingTraces = true
	}

	// We need a convenient way to know if we're even using Consul later
	if p.ConsulForwardService != "" || p.ConsulTraceService != "" {
		p.usingConsul = true
	}

	p.ForwardDestinations = consistent.New()
	p.TraceDestinations = consistent.New()

	// We got a static forward address, stick it in the destination!
	if p.ConsulForwardService == "" && conf.ForwardAddress != "" {
		p.ForwardDestinations.Add(conf.ForwardAddress)
	}
	if p.ConsulTraceService == "" && conf.TraceAddress != "" {
		p.TraceDestinations.Add(conf.TraceAddress)
	}

	if !p.AcceptingForwards && !p.AcceptingTraces {
		err = errors.New("refusing to start with no Consul service names or static addresses in config")
		log.WithError(err).WithFields(logrus.Fields{
			"consul_forward_service_name": p.ConsulForwardService,
			"consul_trace_service_name":   p.ConsulTraceService,
			"forward_address":             conf.ForwardAddress,
			"trace_address":               conf.TraceAddress,
		}).Error("Oops")
		return
	}

	if p.usingConsul {
		p.ConsulInterval, err = time.ParseDuration(conf.ConsulRefreshInterval)
		if err != nil {
			log.WithError(err).Error("Error parsing Consul refresh interval")
			return
		}
		log.WithField("interval", conf.ConsulRefreshInterval).Info("Will use Consul for service discovery")
	}
	p.traceClient = trace.DefaultClient

	// TODO Size of replicas in config?
	//ret.ForwardDestinations.NumberOfReplicas = ???

	if conf.Debug {
		log.SetLevel(logrus.DebugLevel)
	}

	log.WithField("config", conf).Debug("Initialized server")

	return
}

// Start fires up the various goroutines that run on behalf of the server.
// This is separated from the constructor for testing convenience.
func (p *Proxy) Start() {
	log.WithField("version", VERSION).Info("Starting server")

	config := api.DefaultConfig()
	// Use the same HTTP Client we're using for other things, so we can leverage
	// it for testing.
	config.HttpClient = p.HTTPClient
	disc, consulErr := NewConsul(config)
	if consulErr != nil {
		log.WithError(consulErr).Error("Error creating Consul discoverer")
		return
	}
	p.Discoverer = disc

	if p.AcceptingForwards && p.ConsulForwardService != "" {
		p.RefreshDestinations(p.ConsulForwardService, p.ForwardDestinations, &p.ForwardDestinationsMtx)
		if len(p.ForwardDestinations.Members()) == 0 {
			log.WithField("serviceName", p.ConsulForwardService).Fatal("Refusing to start with zero destinations for forwarding.")
		}
	}

	if p.AcceptingTraces && p.ConsulTraceService != "" {
		p.RefreshDestinations(p.ConsulTraceService, p.TraceDestinations, &p.TraceDestinationsMtx)
		if len(p.ForwardDestinations.Members()) == 0 {
			log.WithField("serviceName", p.ConsulTraceService).Fatal("Refusing to start with zero destinations for tracing.")
		}
	}

	if p.usingConsul {
		go func() {
			defer func() {
				ConsumePanic(p.Sentry, p.Statsd, p.Hostname, recover())
			}()
			ticker := time.NewTicker(p.ConsulInterval)
			for range ticker.C {
				if p.AcceptingForwards && p.ConsulForwardService != "" {
					p.RefreshDestinations(p.ConsulForwardService, p.ForwardDestinations, &p.ForwardDestinationsMtx)
				}
				if p.AcceptingTraces && p.ConsulTraceService != "" {
					p.RefreshDestinations(p.ConsulTraceService, p.TraceDestinations, &p.TraceDestinationsMtx)
				}
			}
		}()
	}
}

// HTTPServe starts the HTTP server and listens perpetually until it encounters an unrecoverable error.
func (p *Proxy) HTTPServe() {
	var prf interface {
		Stop()
	}

	// We want to make sure the profile is stopped
	// exactly once (and only once), even if the
	// shutdown pre-hook does not run (which it may not)
	profileStopOnce := sync.Once{}

	if p.enableProfiling {
		profileStartOnce.Do(func() {
			prf = profile.Start()
		})

		defer func() {
			profileStopOnce.Do(prf.Stop)
		}()
	}
	httpSocket := bind.Socket(p.HTTPAddr)
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
	log.WithField("address", p.HTTPAddr).Info("HTTP server listening")
	bind.Ready()

	if err := graceful.Serve(httpSocket, p.Handler()); err != nil {
		log.WithError(err).Error("HTTP server shut down due to error")
	}

	graceful.Shutdown()
}

// RefreshDestinations updates the server's list of valid destinations
// for flushing. This should be called periodically to ensure we have
// the latest data.
func (p *Proxy) RefreshDestinations(serviceName string, ring *consistent.Consistent, mtx *sync.Mutex) {

	start := time.Now()
	destinations, err := p.Discoverer.GetDestinationsForService(serviceName)
	p.Statsd.TimeInMilliseconds("discoverer.update_duration_ns", float64(time.Since(start).Nanoseconds()), []string{fmt.Sprintf("service:%s", serviceName)}, 1.0)
	if err != nil || len(destinations) == 0 {
		log.WithError(err).WithField("service", serviceName).Error("Discoverer returned an error, destinations may be stale!")
		p.Statsd.Incr("discoverer.errors", []string{fmt.Sprintf("service:%s", serviceName)}, 1.0)
		// Return since we got no hosts. We don't want to zero out the list. This
		// should result in us leaving the "last good" values in the ring.
		return
	}

	// At the last moment, lock the mutex and defer so we unlock after setting.
	// We do this after we've fetched info so we don't hold the lock during long
	// queries, timeouts or errors. The flusher can lock the mutex and prevent us
	// from updating at the same time.
	mtx.Lock()
	defer mtx.Unlock()
	ring.Set(destinations)
	p.Statsd.Gauge("discoverer.destination_number", float64(len(destinations)), []string{fmt.Sprintf("service:%s", serviceName)}, 1.0)
}

// Handler returns the Handler responsible for routing request processing.
func (p *Proxy) Handler() http.Handler {
	mux := goji.NewMux()

	mux.HandleFuncC(pat.Get("/healthcheck"), func(c context.Context, w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok\n"))
	})

	mux.Handle(pat.Post("/import"), handleProxy(p))

	mux.Handle(pat.Get("/debug/pprof/cmdline"), http.HandlerFunc(pprof.Cmdline))
	mux.Handle(pat.Get("/debug/pprof/profile"), http.HandlerFunc(pprof.Profile))
	mux.Handle(pat.Get("/debug/pprof/symbol"), http.HandlerFunc(pprof.Symbol))
	mux.Handle(pat.Get("/debug/pprof/trace"), http.HandlerFunc(pprof.Trace))
	// TODO match without trailing slash as well
	mux.Handle(pat.Get("/debug/pprof/*"), http.HandlerFunc(pprof.Index))

	return mux
}

func (p *Proxy) ProxyTraces(ctx context.Context, traces []DatadogTraceSpan) {
	span, _ := trace.StartSpanFromContext(ctx, "veneur.opentracing.proxy.proxy_traces")
	defer span.ClientFinish(p.traceClient)

	tracesByDestination := make(map[string][]*DatadogTraceSpan)
	for _, h := range p.TraceDestinations.Members() {
		tracesByDestination[h] = make([]*DatadogTraceSpan, 0)
	}

	for _, t := range traces {
		dest, _ := p.TraceDestinations.Get(strconv.FormatInt(t.TraceID, 10))
		tracesByDestination[dest] = append(tracesByDestination[dest], &t)
	}

	for dest, batch := range tracesByDestination {

		if len(batch) != 0 {

			// this endpoint is not documented to take an array... but it does
			// another curious constraint of this endpoint is that it does not
			// support "Content-Encoding: deflate"
			err := vhttp.PostHelper(span.Attach(ctx), p.HTTPClient, p.Statsd, p.traceClient, http.MethodPost, fmt.Sprintf("%s/spans", dest), batch, "flush_traces", false, log)

			if err == nil {
				log.WithFields(logrus.Fields{
					"traces":      len(batch),
					"destination": dest,
				}).Info("Completed flushing traces to Datadog")
			} else {
				log.WithFields(
					logrus.Fields{
						"traces":        len(batch),
						logrus.ErrorKey: err}).Error("Error flushing traces to Datadog")
			}
		} else {
			log.WithField("destination", dest).Info("No traces to flush, skipping.")
		}
	}
}

// ProxyMetrics takes a sliceof JSONMetrics and breaks them up into multiple
// HTTP requests by MetricKey using the hash ring.
func (p *Proxy) ProxyMetrics(ctx context.Context, jsonMetrics []samplers.JSONMetric) {
	span, _ := trace.StartSpanFromContext(ctx, "veneur.opentracing.proxy.proxy_metrics")
	defer span.ClientFinish(p.traceClient)

	jsonMetricsByDestination := make(map[string][]samplers.JSONMetric)
	for _, h := range p.ForwardDestinations.Members() {
		jsonMetricsByDestination[h] = make([]samplers.JSONMetric, 0)
	}

	for _, jm := range jsonMetrics {
		dest, _ := p.ForwardDestinations.Get(jm.MetricKey.String())
		jsonMetricsByDestination[dest] = append(jsonMetricsByDestination[dest], jm)
	}

	// nb The response has already been returned at this point, because we
	wg := sync.WaitGroup{}
	wg.Add(len(jsonMetricsByDestination)) // Make our waitgroup the size of our destinations

	for dest, batch := range jsonMetricsByDestination {
		go p.doPost(&wg, dest, batch)
	}
	wg.Wait() // Wait for all the above goroutines to complete
	p.Statsd.TimeInMilliseconds("proxy.duration_ns", float64(time.Since(span.Start).Nanoseconds()), nil, 1.0)
}

func (p *Proxy) doPost(wg *sync.WaitGroup, destination string, batch []samplers.JSONMetric) {
	defer wg.Done()

	batchSize := len(batch)
	if batchSize < 1 {
		return
	}

	err := vhttp.PostHelper(context.TODO(), p.HTTPClient, p.Statsd, p.traceClient, http.MethodPost, fmt.Sprintf("%s/import", destination), batch, "forward", true, log)
	if err == nil {
		log.WithField("metrics", batchSize).Debug("Completed forward to upstream Veneur")
	} else {
		p.Statsd.Count("forward.error_total", 1, []string{"cause:post"}, 1.0)
		log.WithError(err).Warn("Failed to POST metrics to destination")
	}
	p.Statsd.Gauge("metrics_by_destination", float64(batchSize), []string{fmt.Sprintf("destination:%s", destination)}, 1.0)
}

// Shutdown signals the server to shut down after closing all
// current connections.
func (p *Proxy) Shutdown() {
	log.Info("Shutting down server gracefully")
	graceful.Shutdown()
}
