package veneur

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

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
	mutex            *sync.Mutex
	traceClient      *trace.Client
	logger           *logrus.Logger
	wm               WorkerMetrics
	stats            *statsd.Client
	tagKeysToStrip   []string
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
	counters   map[samplers.MetricKey]*samplers.Counter
	gauges     map[samplers.MetricKey]*samplers.Gauge
	histograms map[samplers.MetricKey]*samplers.Histo
	sets       map[samplers.MetricKey]*samplers.Set
	timers     map[samplers.MetricKey]*samplers.Histo

	// this is for counters which are globally aggregated
	globalCounters map[samplers.MetricKey]*samplers.Counter
	// and gauges which are global
	globalGauges map[samplers.MetricKey]*samplers.Gauge

	// these are used for metrics that shouldn't be forwarded
	localHistograms   map[samplers.MetricKey]*samplers.Histo
	localSets         map[samplers.MetricKey]*samplers.Set
	localTimers       map[samplers.MetricKey]*samplers.Histo
	localStatusChecks map[samplers.MetricKey]*samplers.StatusCheck
}

// NewWorkerMetrics initializes a WorkerMetrics struct
func NewWorkerMetrics() WorkerMetrics {
	return WorkerMetrics{
		counters:          map[samplers.MetricKey]*samplers.Counter{},
		globalCounters:    map[samplers.MetricKey]*samplers.Counter{},
		globalGauges:      map[samplers.MetricKey]*samplers.Gauge{},
		gauges:            map[samplers.MetricKey]*samplers.Gauge{},
		histograms:        map[samplers.MetricKey]*samplers.Histo{},
		sets:              map[samplers.MetricKey]*samplers.Set{},
		timers:            map[samplers.MetricKey]*samplers.Histo{},
		localHistograms:   map[samplers.MetricKey]*samplers.Histo{},
		localSets:         map[samplers.MetricKey]*samplers.Set{},
		localTimers:       map[samplers.MetricKey]*samplers.Histo{},
		localStatusChecks: map[samplers.MetricKey]*samplers.StatusCheck{},
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
			if _, present = wm.globalCounters[mk]; !present {
				wm.globalCounters[mk] = samplers.NewCounter(mk.Name, tags)
			}
		} else {
			if _, present = wm.counters[mk]; !present {
				wm.counters[mk] = samplers.NewCounter(mk.Name, tags)
			}
		}
	case gaugeTypeName:
		if Scope == samplers.GlobalOnly {
			if _, present = wm.globalGauges[mk]; !present {
				wm.globalGauges[mk] = samplers.NewGauge(mk.Name, tags)
			}
		} else {
			if _, present = wm.gauges[mk]; !present {
				wm.gauges[mk] = samplers.NewGauge(mk.Name, tags)
			}
		}
	case histogramTypeName:
		if Scope == samplers.LocalOnly {
			if _, present = wm.localHistograms[mk]; !present {
				wm.localHistograms[mk] = samplers.NewHist(mk.Name, tags)
			}
		} else {
			if _, present = wm.histograms[mk]; !present {
				wm.histograms[mk] = samplers.NewHist(mk.Name, tags)
			}
		}
	case setTypeName:
		if Scope == samplers.LocalOnly {
			if _, present = wm.localSets[mk]; !present {
				wm.localSets[mk] = samplers.NewSet(mk.Name, tags)
			}
		} else {
			if _, present = wm.sets[mk]; !present {
				wm.sets[mk] = samplers.NewSet(mk.Name, tags)
			}
		}
	case timerTypeName:
		if Scope == samplers.LocalOnly {
			if _, present = wm.localTimers[mk]; !present {
				wm.localTimers[mk] = samplers.NewHist(mk.Name, tags)
			}
		} else {
			if _, present = wm.timers[mk]; !present {
				wm.timers[mk] = samplers.NewHist(mk.Name, tags)
			}
		}
	case statusTypeName:
		if _, present = wm.localStatusChecks[mk]; !present {
			wm.localStatusChecks[mk] = samplers.NewStatusCheck(mk.Name, tags)
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
func NewWorker(id int, cl *trace.Client, logger *logrus.Logger, stats *statsd.Client, tagKeysToStrip []string) *Worker {
	var tagKeys []string
	for _, t := range tagKeysToStrip {
		tagKeys = append(tagKeys, fmt.Sprintf("%s:", t))
	}

	return &Worker{
		id:               id,
		PacketChan:       make(chan samplers.UDPMetric, 32),
		ImportChan:       make(chan []samplers.JSONMetric, 32),
		ImportMetricChan: make(chan []*metricpb.Metric, 32),
		QuitChan:         make(chan struct{}),
		processed:        0,
		imported:         0,
		mutex:            &sync.Mutex{},
		traceClient:      cl,
		logger:           logger,
		wm:               NewWorkerMetrics(),
		stats:            stats,
		tagKeysToStrip:   tagKeys,
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

// MetricsProcessedCount is a convenince method for testing
// that allows us to fetch the Worker's processed count
// in a non-racey way.
func (w *Worker) MetricsProcessedCount() int64 {
	w.mutex.Lock()
	defer w.mutex.Unlock()
	return w.processed
}

// ProcessMetric takes a Metric and samples it
//
// This is standalone to facilitate testing
func (w *Worker) ProcessMetric(m *samplers.UDPMetric) {
	w.mutex.Lock()
	defer w.mutex.Unlock()
	w.processed++
	w.wm.Upsert(m.MetricKey, m.Scope, m.Tags)

	switch m.Type {
	case counterTypeName:
		if m.Scope == samplers.GlobalOnly {
			w.wm.globalCounters[m.MetricKey].Sample(m.Value.(float64), m.SampleRate)
		} else {
			w.wm.counters[m.MetricKey].Sample(m.Value.(float64), m.SampleRate)
		}
	case gaugeTypeName:
		if m.Scope == samplers.GlobalOnly {
			w.wm.globalGauges[m.MetricKey].Sample(m.Value.(float64), m.SampleRate)
		} else {
			w.wm.gauges[m.MetricKey].Sample(m.Value.(float64), m.SampleRate)
		}
	case histogramTypeName:
		if m.Scope == samplers.LocalOnly {
			w.wm.localHistograms[m.MetricKey].Sample(m.Value.(float64), m.SampleRate)
		} else {
			w.wm.histograms[m.MetricKey].Sample(m.Value.(float64), m.SampleRate)
		}
	case setTypeName:
		if m.Scope == samplers.LocalOnly {
			w.wm.localSets[m.MetricKey].Sample(m.Value.(string), m.SampleRate)
		} else {
			w.wm.sets[m.MetricKey].Sample(m.Value.(string), m.SampleRate)
		}
	case timerTypeName:
		if m.Scope == samplers.LocalOnly {
			w.wm.localTimers[m.MetricKey].Sample(m.Value.(float64), m.SampleRate)
		} else {
			w.wm.timers[m.MetricKey].Sample(m.Value.(float64), m.SampleRate)
		}
	case statusTypeName:
		v := float64(m.Value.(ssf.SSFSample_Status))
		w.wm.localStatusChecks[m.MetricKey].Sample(v, m.SampleRate, m.Message, m.HostName)
	default:
		log.WithField("type", m.Type).Error("Unknown metric type for processing")
	}
}

// ImportMetric receives a metric from another veneur instance
func (w *Worker) ImportMetric(other samplers.JSONMetric) {
	w.mutex.Lock()
	defer w.mutex.Unlock()

	// Configuration may specify some tag keys that should be stripped from
	// imports. Check the incoming tags and nuke any that are present
	finalTags := other.Tags[:0]
	for _, t := range other.Tags {
		var found bool
		for _, strip := range w.tagKeysToStrip {
			if strings.HasPrefix(t, strip) {
				// This one matches, flag it as such
				found = true
				// exit this for loop, no reason to keep checking
				break
			}
		}
		// Found tags don't get skipped.
		if found != true {
			finalTags = append(finalTags, t)
		}
	}

	// we don't increment the processed metric counter here, it was already
	// counted by the original veneur that sent this to us
	w.imported++
	if other.Type == counterTypeName || other.Type == gaugeTypeName {
		// this is an odd special case -- counters that are imported are global
		w.wm.Upsert(other.MetricKey, samplers.GlobalOnly, finalTags)
	} else {
		w.wm.Upsert(other.MetricKey, samplers.MixedScope, finalTags)
	}

	switch other.Type {
	case counterTypeName:
		if err := w.wm.globalCounters[other.MetricKey].Combine(other.Value); err != nil {
			log.WithError(err).Error("Could not merge counters")
		}
	case gaugeTypeName:
		if err := w.wm.globalGauges[other.MetricKey].Combine(other.Value); err != nil {
			log.WithError(err).Error("Could not merge gauges")
		}
	case setTypeName:
		if err := w.wm.sets[other.MetricKey].Combine(other.Value); err != nil {
			log.WithError(err).Error("Could not merge sets")
		}
	case histogramTypeName:
		if err := w.wm.histograms[other.MetricKey].Combine(other.Value); err != nil {
			log.WithError(err).Error("Could not merge histograms")
		}
	case timerTypeName:
		if err := w.wm.timers[other.MetricKey].Combine(other.Value); err != nil {
			log.WithError(err).Error("Could not merge timers")
		}
	default:
		log.WithField("type", other.Type).Error("Unknown metric type for importing")
	}
}

// ImportMetricGRPC receives a metric from another veneur instance over gRPC
func (w *Worker) ImportMetricGRPC(other *metricpb.Metric) (err error) {
	w.mutex.Lock()
	defer w.mutex.Unlock()

	key := samplers.NewMetricKeyFromMetric(other)

	scope := samplers.MixedScope
	if other.Type == metricpb.Type_Counter || other.Type == metricpb.Type_Gauge {
		scope = samplers.GlobalOnly
	}

	// Configuration may specify some tag keys that should be stripped from
	// imports. Check the incoming tags and nuke any that are present
	finalTags := other.Tags[:0]
	for _, t := range other.Tags {
		var found bool
		for _, strip := range w.tagKeysToStrip {
			if strings.HasPrefix(t, strip) {
				// This one matches, flag it as such
				found = true
				// exit this for loop, no reason to keep checking
				break
			}
		}
		// Found tags don't get skipped.
		if found != true {
			finalTags = append(finalTags, t)
		}
	}

	w.wm.Upsert(key, scope, finalTags)
	w.imported++

	switch v := other.GetValue().(type) {
	case *metricpb.Metric_Counter:
		w.wm.globalCounters[key].Merge(v.Counter)
	case *metricpb.Metric_Gauge:
		w.wm.globalGauges[key].Merge(v.Gauge)
	case *metricpb.Metric_Set:
		if merr := w.wm.sets[key].Merge(v.Set); merr != nil {
			err = fmt.Errorf("could not merge a set: %v", err)
		}
	case *metricpb.Metric_Histogram:
		switch other.Type {
		case metricpb.Type_Histogram:
			w.wm.histograms[key].Merge(v.Histogram)
		case metricpb.Type_Timer:
			w.wm.timers[key].Merge(v.Histogram)
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
	// This is a critical spot. The worker can't process metrics while this
	// mutex is held! So we try and minimize it by copying the maps of values
	// and assigning new ones.
	wm := NewWorkerMetrics()
	w.mutex.Lock()
	ret := w.wm
	processed := w.processed
	imported := w.imported

	w.wm = wm
	w.processed = 0
	w.imported = 0
	w.mutex.Unlock()

	// Track how much time each worker takes to flush.
	w.stats.Timing(
		"flush.worker_duration_ns",
		time.Since(start),
		nil,
		1.0,
	)
	w.stats.Count("worker.metrics_processed_total", processed, []string{}, 1.0)
	w.stats.Count("worker.metrics_imported_total", imported, []string{}, 1.0)

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
