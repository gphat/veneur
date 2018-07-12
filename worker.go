package veneur

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/DataDog/datadog-go/statsd"
	"github.com/sirupsen/logrus"
	"github.com/stripe/veneur/protocol"
	"github.com/stripe/veneur/samplers"
	"github.com/stripe/veneur/samplers/metricpb"
	"github.com/stripe/veneur/sinks"
	"github.com/stripe/veneur/ssf"
	"github.com/stripe/veneur/trace"
	"github.com/stripe/veneur/trace/metrics"
)

const counterTypeName = "counter"
const gaugeTypeName = "gauge"
const histogramTypeName = "histogram"
const setTypeName = "set"
const timerTypeName = "timer"
const statusTypeName = "status"

// Worker is the doodad that does work.
type Worker struct {
	id               int
	PacketChan       chan samplers.UDPMetric
	ImportChan       chan []samplers.JSONMetric
	ImportMetricChan chan []*metricpb.Metric
	QuitChan         chan struct{}
	processed        int64
	imported         int64
	traceClient      *trace.Client
	logger           *logrus.Logger
	wm               WorkerMetrics
	stats            *statsd.Client
}

// IngestUDP on a Worker feeds the metric into the worker's PacketChan.
func (w *Worker) IngestUDP(metric samplers.UDPMetric) {
	w.PacketChan <- metric
}

func (w *Worker) IngestMetrics(ms []*metricpb.Metric) {
	w.ImportMetricChan <- ms
}

// WorkerMetrics is just a plain struct bundling together the flushed contents of a worker
type WorkerMetrics struct {
	// we do not want to key on the metric's Digest here, because those could
	// collide, and then we'd have to implement a hashtable on top of go maps,
	// which would be silly
	counters       map[samplers.MetricKey]*samplers.Counter
	counterMutex   sync.RWMutex
	gauges         map[samplers.MetricKey]*samplers.Gauge
	gaugeMutex     sync.RWMutex
	histograms     map[samplers.MetricKey]*samplers.Histo
	histogramMutex sync.RWMutex
	sets           map[samplers.MetricKey]*samplers.Set
	setMutex       sync.RWMutex
	timers         map[samplers.MetricKey]*samplers.Histo
	timerMutex     sync.RWMutex

	// this is for counters which are globally aggregated
	globalCounters     map[samplers.MetricKey]*samplers.Counter
	globalCounterMutex sync.RWMutex
	// and gauges which are global
	globalGauges     map[samplers.MetricKey]*samplers.Gauge
	globalGaugeMutex sync.RWMutex

	// these are used for metrics that shouldn't be forwarded
	localHistograms       map[samplers.MetricKey]*samplers.Histo
	localHistogramMutex   sync.RWMutex
	localSets             map[samplers.MetricKey]*samplers.Set
	localSetMutex         sync.RWMutex
	localTimers           map[samplers.MetricKey]*samplers.Histo
	localTimerMutex       sync.RWMutex
	localStatusChecks     map[samplers.MetricKey]*samplers.StatusCheck
	localStatusCheckMutex sync.RWMutex
}

// NewWorkerMetrics initializes a WorkerMetrics struct
func NewWorkerMetrics() WorkerMetrics {
	return WorkerMetrics{
		counters:              map[samplers.MetricKey]*samplers.Counter{},
		counterMutex:          sync.RWMutex{},
		globalCounters:        map[samplers.MetricKey]*samplers.Counter{},
		globalCounterMutex:    sync.RWMutex{},
		globalGauges:          map[samplers.MetricKey]*samplers.Gauge{},
		globalGaugeMutex:      sync.RWMutex{},
		gauges:                map[samplers.MetricKey]*samplers.Gauge{},
		gaugeMutex:            sync.RWMutex{},
		histograms:            map[samplers.MetricKey]*samplers.Histo{},
		histogramMutex:        sync.RWMutex{},
		sets:                  map[samplers.MetricKey]*samplers.Set{},
		setMutex:              sync.RWMutex{},
		timers:                map[samplers.MetricKey]*samplers.Histo{},
		timerMutex:            sync.RWMutex{},
		localHistograms:       map[samplers.MetricKey]*samplers.Histo{},
		localHistogramMutex:   sync.RWMutex{},
		localSets:             map[samplers.MetricKey]*samplers.Set{},
		localSetMutex:         sync.RWMutex{},
		localTimers:           map[samplers.MetricKey]*samplers.Histo{},
		localTimerMutex:       sync.RWMutex{},
		localStatusChecks:     map[samplers.MetricKey]*samplers.StatusCheck{},
		localStatusCheckMutex: sync.RWMutex{},
	}
}

// Upsert creates an entry on the WorkerMetrics struct for the given metrickey (if one does not already exist)
// and updates the existing entry (if one already exists).
// Returns true if the metric entry was created and false otherwise.
func (wm WorkerMetrics) Upsert(mk samplers.MetricKey, Scope samplers.MetricScope, tags []string) bool {
	present := false
	switch mk.Type {
	case counterTypeName:
		if Scope == samplers.GlobalOnly {
			wm.globalCounterMutex.RLock()
			_, present = wm.globalCounters[mk]
			wm.globalCounterMutex.RUnlock()
			if !present {
				wm.globalCounterMutex.Lock()
				wm.globalCounters[mk] = samplers.NewCounter(mk.Name, tags)
				wm.globalCounterMutex.Unlock()
			}
		} else {
			wm.counterMutex.RLock()
			_, present = wm.counters[mk]
			wm.counterMutex.RUnlock()
			if !present {
				wm.counterMutex.Lock()
				wm.counters[mk] = samplers.NewCounter(mk.Name, tags)
				wm.counterMutex.Unlock()
			}
		}
	case gaugeTypeName:
		if Scope == samplers.GlobalOnly {
			wm.globalGaugeMutex.RLock()
			_, present = wm.globalGauges[mk]
			wm.globalGaugeMutex.RUnlock()
			if !present {
				wm.globalGaugeMutex.Lock()
				wm.globalGauges[mk] = samplers.NewGauge(mk.Name, tags)
				wm.globalGaugeMutex.Unlock()
			}
		} else {
			wm.gaugeMutex.RLock()
			_, present = wm.gauges[mk]
			wm.gaugeMutex.RUnlock()
			if !present {
				wm.gaugeMutex.Lock()
				wm.gauges[mk] = samplers.NewGauge(mk.Name, tags)
				wm.gaugeMutex.Unlock()
			}
		}
	case histogramTypeName:
		if Scope == samplers.LocalOnly {
			wm.localHistogramMutex.RLock()
			_, present = wm.localHistograms[mk]
			wm.localHistogramMutex.RUnlock()
			if !present {
				wm.localHistogramMutex.Lock()
				wm.localHistograms[mk] = samplers.NewHist(mk.Name, tags)
				wm.localHistogramMutex.Unlock()
			}
		} else {
			wm.histogramMutex.RLock()
			_, present = wm.histograms[mk]
			wm.histogramMutex.RUnlock()
			if !present {
				wm.histogramMutex.Lock()
				wm.histograms[mk] = samplers.NewHist(mk.Name, tags)
				wm.histogramMutex.Unlock()
			}
		}
	case setTypeName:
		if Scope == samplers.LocalOnly {
			wm.localSetMutex.RLock()
			_, present = wm.localSets[mk]
			wm.localSetMutex.RUnlock()
			if !present {
				wm.localSetMutex.Lock()
				wm.localSets[mk] = samplers.NewSet(mk.Name, tags)
				wm.localSetMutex.Unlock()
			}
		} else {
			wm.setMutex.RLock()
			_, present = wm.sets[mk]
			wm.setMutex.RUnlock()
			if !present {
				wm.setMutex.Lock()
				wm.sets[mk] = samplers.NewSet(mk.Name, tags)
				wm.setMutex.Unlock()
			}
		}
	case timerTypeName:
		if Scope == samplers.LocalOnly {
			wm.localTimerMutex.RLock()
			_, present = wm.localTimers[mk]
			wm.localTimerMutex.RUnlock()
			if !present {
				wm.localTimerMutex.Lock()
				wm.localTimers[mk] = samplers.NewHist(mk.Name, tags)
				wm.localTimerMutex.Unlock()
			}
		} else {
			wm.timerMutex.RLock()
			_, present = wm.timers[mk]
			wm.timerMutex.RUnlock()
			if !present {
				wm.timerMutex.Lock()
				wm.timers[mk] = samplers.NewHist(mk.Name, tags)
				wm.timerMutex.Unlock()
			}
		}
	case statusTypeName:
		wm.localStatusCheckMutex.RLock()
		_, present = wm.localStatusChecks[mk]
		wm.localStatusCheckMutex.RUnlock()
		if !present {
			wm.localStatusCheckMutex.Lock()
			wm.localStatusChecks[mk] = samplers.NewStatusCheck(mk.Name, tags)
			wm.localStatusCheckMutex.Unlock()
		}
		// no need to raise errors on unknown types
		// the caller will probably end up doing that themselves
	}
	return !present
}

// ForwardableMetrics converts all metrics that should be forwarded to
// metricpb.Metric (protobuf-compatible).
func (wm WorkerMetrics) ForwardableMetrics(cl *trace.Client) []*metricpb.Metric {
	bufLen := len(wm.histograms) + len(wm.sets) + len(wm.timers) +
		len(wm.globalCounters) + len(wm.globalGauges)

	metrics := make([]*metricpb.Metric, 0, bufLen)
	for _, count := range wm.globalCounters {
		metrics = wm.appendExportedMetric(metrics, count, metricpb.Type_Counter, cl)
	}
	for _, gauge := range wm.globalGauges {
		metrics = wm.appendExportedMetric(metrics, gauge, metricpb.Type_Gauge, cl)
	}
	for _, histo := range wm.histograms {
		metrics = wm.appendExportedMetric(metrics, histo, metricpb.Type_Histogram, cl)
	}
	for _, set := range wm.sets {
		metrics = wm.appendExportedMetric(metrics, set, metricpb.Type_Set, cl)
	}
	for _, timer := range wm.timers {
		metrics = wm.appendExportedMetric(metrics, timer, metricpb.Type_Timer, cl)
	}

	return metrics
}

// A type implemented by all valid samplers
type metricExporter interface {
	GetName() string
	Metric() (*metricpb.Metric, error)
}

// appendExportedMetric appends the exported version of the input metric, with
// the inputted type.  If the export fails, the original slice is returned
// and an error is logged.
func (wm WorkerMetrics) appendExportedMetric(res []*metricpb.Metric, exp metricExporter, mType metricpb.Type, cl *trace.Client) []*metricpb.Metric {
	m, err := exp.Metric()
	if err != nil {
		log.WithFields(logrus.Fields{
			logrus.ErrorKey: err,
			"type":          mType,
			"name":          exp.GetName(),
		}).Error("Could not export metric")
		metrics.ReportOne(cl,
			ssf.Count("worker_metrics.export_metric.errors", 1, map[string]string{
				"type": mType.String(),
			}),
		)
		return res
	}

	m.Type = mType
	return append(res, m)
}

// NewWorker creates, and returns a new Worker object.
func NewWorker(id int, cl *trace.Client, logger *logrus.Logger, stats *statsd.Client) *Worker {
	return &Worker{
		id:               id,
		PacketChan:       make(chan samplers.UDPMetric, 32),
		ImportChan:       make(chan []samplers.JSONMetric, 32),
		ImportMetricChan: make(chan []*metricpb.Metric, 32),
		QuitChan:         make(chan struct{}),
		processed:        0,
		imported:         0,
		traceClient:      cl,
		logger:           logger,
		wm:               NewWorkerMetrics(),
		stats:            stats,
	}
}

// Work will start the worker listening for metrics to process or import.
// It will not return until the worker is sent a message to terminate using Stop()
func (w *Worker) Work() {
	for {
		select {
		case m := <-w.PacketChan:
			w.ProcessMetric(&m)
		case m := <-w.ImportChan:
			for _, j := range m {
				w.ImportMetric(j)
			}
		case ms := <-w.ImportMetricChan:
			for _, m := range ms {
				w.ImportMetricGRPC(m)
			}
		case <-w.QuitChan:
			// We have been asked to stop.
			log.WithField("worker", w.id).Error("Stopping")
			return
		}
	}
}

func (w *Worker) MetricsProcessedCount() int64 {
	return atomic.LoadInt64(&w.processed)
}

// ProcessMetric takes a Metric and samples it
//
// This is standalone to facilitate testing
func (w *Worker) ProcessMetric(m *samplers.UDPMetric) {
	atomic.AddInt64(&w.processed, 1)
	w.wm.Upsert(m.MetricKey, m.Scope, m.Tags)

	switch m.Type {
	case counterTypeName:
		if m.Scope == samplers.GlobalOnly {
			w.wm.globalCounterMutex.RLock()
			w.wm.globalCounters[m.MetricKey].Sample(m.Value.(float64), m.SampleRate)
			w.wm.globalCounterMutex.RUnlock()
		} else {
			w.wm.counterMutex.RLock()
			w.wm.counters[m.MetricKey].Sample(m.Value.(float64), m.SampleRate)
			w.wm.counterMutex.RUnlock()
		}
	case gaugeTypeName:
		if m.Scope == samplers.GlobalOnly {
			w.wm.globalGaugeMutex.RLock()
			w.wm.globalGauges[m.MetricKey].Sample(m.Value.(float64), m.SampleRate)
			w.wm.globalGaugeMutex.RUnlock()
		} else {
			w.wm.gaugeMutex.RLock()
			w.wm.gauges[m.MetricKey].Sample(m.Value.(float64), m.SampleRate)
			w.wm.gaugeMutex.RUnlock()
		}
	case histogramTypeName:
		if m.Scope == samplers.LocalOnly {
			w.wm.localHistogramMutex.RLock()
			w.wm.localHistograms[m.MetricKey].Sample(m.Value.(float64), m.SampleRate)
			w.wm.localHistogramMutex.RUnlock()
		} else {
			w.wm.histogramMutex.RLock()
			w.wm.histograms[m.MetricKey].Sample(m.Value.(float64), m.SampleRate)
			w.wm.histogramMutex.RUnlock()
		}
	case setTypeName:
		if m.Scope == samplers.LocalOnly {
			w.wm.localSetMutex.RLock()
			w.wm.localSets[m.MetricKey].Sample(m.Value.(string), m.SampleRate)
			w.wm.localSetMutex.RUnlock()
		} else {
			w.wm.setMutex.RLock()
			w.wm.sets[m.MetricKey].Sample(m.Value.(string), m.SampleRate)
			w.wm.setMutex.RUnlock()
		}
	case timerTypeName:
		if m.Scope == samplers.LocalOnly {
			w.wm.localTimerMutex.RLock()
			w.wm.localTimers[m.MetricKey].Sample(m.Value.(float64), m.SampleRate)
			w.wm.localTimerMutex.RUnlock()
		} else {
			w.wm.timerMutex.RLock()
			w.wm.timers[m.MetricKey].Sample(m.Value.(float64), m.SampleRate)
			w.wm.timerMutex.RUnlock()
		}
	case statusTypeName:
		v := float64(m.Value.(ssf.SSFSample_Status))
		w.wm.localStatusCheckMutex.RLock()
		w.wm.localStatusChecks[m.MetricKey].Sample(v, m.SampleRate, m.Message, m.HostName)
		w.wm.localStatusCheckMutex.RUnlock()
	default:
		log.WithField("type", m.Type).Error("Unknown metric type for processing")
	}
}

// ImportMetric receives a metric from another veneur instance
func (w *Worker) ImportMetric(other samplers.JSONMetric) {
	// we don't increment the processed metric counter here, it was already
	// counted by the original veneur that sent this to us
	atomic.AddInt64(&w.imported, 1)
	if other.Type == counterTypeName || other.Type == gaugeTypeName {
		// this is an odd special case -- counters that are imported are global
		w.wm.Upsert(other.MetricKey, samplers.GlobalOnly, other.Tags)
	} else {
		w.wm.Upsert(other.MetricKey, samplers.MixedScope, other.Tags)
	}

	switch other.Type {
	case counterTypeName:
		w.wm.globalCounterMutex.RLock()
		defer w.wm.globalCounterMutex.RUnlock()
		if err := w.wm.globalCounters[other.MetricKey].Combine(other.Value); err != nil {
			log.WithError(err).Error("Could not merge counters")
		}
	case gaugeTypeName:
		w.wm.globalGaugeMutex.RLock()
		defer w.wm.globalGaugeMutex.RUnlock()
		if err := w.wm.globalGauges[other.MetricKey].Combine(other.Value); err != nil {
			log.WithError(err).Error("Could not merge gauges")
		}
	case setTypeName:
		w.wm.setMutex.RLock()
		defer w.wm.setMutex.RUnlock()
		if err := w.wm.sets[other.MetricKey].Combine(other.Value); err != nil {
			log.WithError(err).Error("Could not merge sets")
		}
	case histogramTypeName:
		w.wm.histogramMutex.RLock()
		defer w.wm.histogramMutex.RUnlock()
		if err := w.wm.histograms[other.MetricKey].Combine(other.Value); err != nil {
			log.WithError(err).Error("Could not merge histograms")
		}
	case timerTypeName:
		w.wm.timerMutex.RLock()
		w.wm.timerMutex.RUnlock()
		if err := w.wm.timers[other.MetricKey].Combine(other.Value); err != nil {
			log.WithError(err).Error("Could not merge timers")
		}
	default:
		log.WithField("type", other.Type).Error("Unknown metric type for importing")
	}
}

// ImportMetricGRPC receives a metric from another veneur instance over gRPC
func (w *Worker) ImportMetricGRPC(other *metricpb.Metric) (err error) {
	key := samplers.NewMetricKeyFromMetric(other)

	scope := samplers.MixedScope
	if other.Type == metricpb.Type_Counter || other.Type == metricpb.Type_Gauge {
		scope = samplers.GlobalOnly
	}

	w.wm.Upsert(key, scope, other.Tags)
	atomic.AddInt64(&w.imported, 1)

	switch v := other.GetValue().(type) {
	case *metricpb.Metric_Counter:
		w.wm.globalCounterMutex.RLock()
		w.wm.globalCounters[key].Merge(v.Counter)
		w.wm.globalCounterMutex.RUnlock()
	case *metricpb.Metric_Gauge:
		w.wm.globalGaugeMutex.RLock()
		w.wm.globalGauges[key].Merge(v.Gauge)
		w.wm.globalGaugeMutex.RUnlock()
	case *metricpb.Metric_Set:
		w.wm.setMutex.RLock()
		defer w.wm.setMutex.RUnlock()
		if merr := w.wm.sets[key].Merge(v.Set); merr != nil {
			err = fmt.Errorf("could not merge a set: %v", err)
		}
	case *metricpb.Metric_Histogram:
		switch other.Type {
		case metricpb.Type_Histogram:
			w.wm.histogramMutex.RLock()
			w.wm.histograms[key].Merge(v.Histogram)
			w.wm.histogramMutex.RUnlock()
		case metricpb.Type_Timer:
			w.wm.timerMutex.RLock()
			w.wm.timers[key].Merge(v.Histogram)
			w.wm.timerMutex.RUnlock()
		}
	case nil:
		err = errors.New("Can't import a metric with a nil value")
	default:
		err = fmt.Errorf("Unknown metric type for importing: %T", v)
	}

	if err != nil {
		log.WithError(err).WithFields(logrus.Fields{
			"type":     other.Type,
			"name":     other.Name,
			"protocol": "grpc",
		}).Error("Failed to import a metric")
	}

	return err
}

// Flush resets the worker's internal metrics and returns their contents.
func (w *Worker) Flush() WorkerMetrics {
	start := time.Now()
	// We will atomically swap out the worker's metrics to avoid any problems.
	wm := NewWorkerMetrics()
	ret := w.wm
	wmOld := unsafe.Pointer(&w.wm)
	wmNew := unsafe.Pointer(&wm)
	atomic.SwapPointer(&wmOld, wmNew)

	// Since we're not locking this globally, this might race and we might lose
	// a couple of increments elsewhere, but that seems fine.
	var processed int64
	atomic.SwapInt64(&processed, 0)
	var imported int64
	atomic.SwapInt64(&imported, 0)

	// Track how much time each worker takes to flush.
	w.stats.Timing(
		"flush.worker_duration_ns",
		time.Since(start),
		nil,
		1.0,
	)
	w.stats.Count("worker.metrics_processed_total", int64(processed), []string{}, 1.0)
	w.stats.Count("worker.metrics_imported_total", int64(imported), []string{}, 1.0)

	return ret
}

// Stop tells the worker to stop listening for work requests.
//
// Note that the worker will only stop *after* it has finished its work.
func (w *Worker) Stop() {
	close(w.QuitChan)
}

// EventWorker is similar to a Worker but it collects events and service checks instead of metrics.
type EventWorker struct {
	sampleChan  chan ssf.SSFSample
	mutex       *sync.Mutex
	samples     []ssf.SSFSample
	traceClient *trace.Client
	stats       *statsd.Client
}

// NewEventWorker creates an EventWorker ready to collect events and service checks.
func NewEventWorker(cl *trace.Client, stats *statsd.Client) *EventWorker {
	return &EventWorker{
		sampleChan:  make(chan ssf.SSFSample),
		mutex:       &sync.Mutex{},
		traceClient: cl,
		stats:       stats,
	}
}

// Work will start the EventWorker listening for events and service checks.
// This function will never return.
func (ew *EventWorker) Work() {
	for {
		select {
		case s := <-ew.sampleChan:
			ew.mutex.Lock()
			ew.samples = append(ew.samples, s)
			ew.mutex.Unlock()
		}
	}
}

// Flush returns the EventWorker's stored events and service checks and
// resets the stored contents.
func (ew *EventWorker) Flush() []ssf.SSFSample {
	start := time.Now()
	ew.mutex.Lock()

	retsamples := ew.samples
	// these slices will be allocated again at append time
	ew.samples = nil

	ew.mutex.Unlock()
	ew.stats.Count("worker.other_samples_flushed_total", int64(len(retsamples)), nil, 1.0)
	ew.stats.TimeInMilliseconds("flush.other_samples_duration_ns", float64(time.Since(start).Nanoseconds()), nil, 1.0)
	return retsamples
}

// SpanWorker is similar to a Worker but it collects events and service checks instead of metrics.
type SpanWorker struct {
	SpanChan   <-chan *ssf.SSFSpan
	sinkTags   []map[string]string
	commonTags map[string]string
	sinks      []sinks.SpanSink

	// cumulative time spent per sink, in nanoseconds
	cumulativeTimes []int64
	traceClient     *trace.Client
	statsd          *statsd.Client
	capCount        int64
}

// NewSpanWorker creates a SpanWorker ready to collect events and service checks.
func NewSpanWorker(sinks []sinks.SpanSink, cl *trace.Client, statsd *statsd.Client, spanChan <-chan *ssf.SSFSpan, commonTags map[string]string) *SpanWorker {
	tags := make([]map[string]string, len(sinks))
	for i, sink := range sinks {
		tags[i] = map[string]string{
			"sink": sink.Name(),
		}
	}

	return &SpanWorker{
		SpanChan:        spanChan,
		sinks:           sinks,
		sinkTags:        tags,
		commonTags:      commonTags,
		cumulativeTimes: make([]int64, len(sinks)),
		traceClient:     cl,
		statsd:          statsd,
	}
}

// Work will start the SpanWorker listening for spans.
// This function will never return.
func (tw *SpanWorker) Work() {
	const Timeout = 9 * time.Second
	capcmp := cap(tw.SpanChan) - 1
	for m := range tw.SpanChan {
		// If we are at or one below cap, increment the counter.
		if len(tw.SpanChan) >= capcmp {
			atomic.AddInt64(&tw.capCount, 1)
		}

		if m.Tags == nil && len(tw.commonTags) != 0 {
			m.Tags = make(map[string]string, len(tw.commonTags))
		}

		for k, v := range tw.commonTags {
			if _, has := m.Tags[k]; !has {
				m.Tags[k] = v
			}
		}

		var wg sync.WaitGroup
		for i, s := range tw.sinks {
			tags := tw.sinkTags[i]
			wg.Add(1)
			go func(i int, sink sinks.SpanSink, span *ssf.SSFSpan, wg *sync.WaitGroup) {
				defer wg.Done()

				done := make(chan struct{})
				start := time.Now()

				go func() {
					// Give each sink a change to ingest.
					err := sink.Ingest(span)
					if err != nil {
						if _, isNoTrace := err.(*protocol.InvalidTrace); !isNoTrace {
							// If a sink goes wacko and errors a lot, we stand to emit a
							// loooot of metrics towards all span workers here since
							// span ingest rates can be very high. C'est la vie.
							t := make([]string, 0, len(tags)+1)
							for k, v := range tags {
								t = append(t, k+":"+v)
							}

							t = append(t, "sink:"+sink.Name())
							tw.statsd.Incr("worker.span.ingest_error_total", t, 1.0)
						}
					}
					done <- struct{}{}
				}()

				select {
				case _ = <-done:
				case <-time.After(Timeout):
					log.WithFields(logrus.Fields{
						"sink":  sink.Name(),
						"index": i,
					}).Error("Timed out on sink ingestion")

					t := make([]string, 0, len(tags)+1)
					for k, v := range tags {
						t = append(t, k+":"+v)
					}

					t = append(t, "sink:"+sink.Name())
					tw.statsd.Incr("worker.span.ingest_timeout_total", t, 1.0)
				}
				atomic.AddInt64(&tw.cumulativeTimes[i], int64(time.Since(start)/time.Nanosecond))
			}(i, s, m, &wg)
		}
		wg.Wait()
	}
}

// Flush invokes flush on each sink.
func (tw *SpanWorker) Flush() {
	samples := &ssf.Samples{}

	// Flush and time each sink.
	for i, s := range tw.sinks {
		tags := make([]string, 0, len(tw.sinkTags[i]))
		for k, v := range tw.sinkTags[i] {
			tags = append(tags, fmt.Sprintf("%s:%s", k, v))
		}
		sinkFlushStart := time.Now()
		s.Flush()
		tw.statsd.Timing("worker.span.flush_duration_ns", time.Since(sinkFlushStart), tags, 1.0)

		// cumulative time is measured in nanoseconds
		cumulative := time.Duration(atomic.SwapInt64(&tw.cumulativeTimes[i], 0)) * time.Nanosecond
		tw.statsd.Timing(sinks.MetricKeySpanIngestDuration, cumulative, tags, 1.0)
	}

	metrics.Report(tw.traceClient, samples)
	tw.statsd.Count("worker.span.hit_chan_cap", atomic.SwapInt64(&tw.capCount, 0), nil, 1.0)
}
