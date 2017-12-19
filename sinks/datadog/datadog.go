package datadog

import (
	"container/ring"
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/DataDog/datadog-go/statsd"
	"github.com/sirupsen/logrus"
	vhttp "github.com/stripe/veneur/http"
	"github.com/stripe/veneur/protocol"
	"github.com/stripe/veneur/samplers"
	"github.com/stripe/veneur/ssf"
	"github.com/stripe/veneur/trace"
)

const DatadogResourceKey = "resource"
const datadogNameKey = "name"
const totalSpansFlushedMetricKey = "worker.spans_flushed_total"

type DatadogMetricSink struct {
	HTTPClient      *http.Client
	APIKey          string
	DDHostname      string
	hostname        string
	flushMaxPerBody int
	statsd          *statsd.Client
	tags            []string
	interval        float64
	traceClient     *trace.Client
	log             *logrus.Logger
}

// DDMetric is a data structure that represents the JSON that Datadog
// wants when posting to the API
type DDMetric struct {
	Name       string        `json:"metric"`
	Value      [1][2]float64 `json:"points"`
	Tags       []string      `json:"tags,omitempty"`
	MetricType string        `json:"type"`
	Hostname   string        `json:"host,omitempty"`
	DeviceName string        `json:"device_name,omitempty"`
	Interval   int32         `json:"interval,omitempty"`
}

// NewDatadogMetricSink creates a new Datadog sink for trace spans.
func NewDatadogMetricSink(interval float64, flushMaxPerBody int, hostname string, tags []string, ddHostname string, apiKey string, httpClient *http.Client, stats *statsd.Client, log *logrus.Logger) (*DatadogMetricSink, error) {
	return &DatadogMetricSink{
		HTTPClient:      httpClient,
		APIKey:          apiKey,
		DDHostname:      ddHostname,
		statsd:          stats,
		interval:        interval,
		flushMaxPerBody: flushMaxPerBody,
		hostname:        hostname,
		tags:            tags,
		log:             log,
	}, nil
}

// Name returns the name of this sink.
func (dd *DatadogMetricSink) Name() string {
	return "datadog"
}

// Start sets the sink up.
func (dd *DatadogMetricSink) Start(cl *trace.Client) error {
	dd.traceClient = cl
	return nil
}

func (dd *DatadogMetricSink) Flush(ctx context.Context, interMetrics []samplers.InterMetric) error {
	span, _ := trace.StartSpanFromContext(ctx, "")
	defer span.ClientFinish(dd.traceClient)

	metrics := dd.finalizeMetrics(interMetrics)

	// break the metrics into chunks of approximately equal size, such that
	// each chunk is less than the limit
	// we compute the chunks using rounding-up integer division
	workers := ((len(metrics) - 1) / dd.flushMaxPerBody) + 1
	chunkSize := ((len(metrics) - 1) / workers) + 1
	dd.log.WithField("workers", workers).Debug("Worker count chosen")
	dd.log.WithField("chunkSize", chunkSize).Debug("Chunk size chosen")
	var wg sync.WaitGroup
	flushStart := time.Now()
	for i := 0; i < workers; i++ {
		chunk := metrics[i*chunkSize:]
		if i < workers-1 {
			// trim to chunk size unless this is the last one
			chunk = chunk[:chunkSize]
		}
		wg.Add(1)
		go dd.flushPart(span.Attach(ctx), chunk, &wg)
	}
	wg.Wait()
	dd.statsd.TimeInMilliseconds("flush.total_duration_ns", float64(time.Since(flushStart).Nanoseconds()), []string{"part:post"}, 1.0)

	dd.log.WithField("metrics", len(metrics)).Info("Completed flush to Datadog")
	return nil
}

func (dd *DatadogMetricSink) FlushEventsChecks(ctx context.Context, events []samplers.UDPEvent, checks []samplers.UDPServiceCheck) {
	span, _ := trace.StartSpanFromContext(ctx, "")
	defer span.ClientFinish(dd.traceClient)

	// fill in the default hostname for packets that didn't set it
	for i := range events {
		if events[i].Hostname == "" {
			events[i].Hostname = dd.hostname
		}
		events[i].Tags = append(events[i].Tags, dd.tags...)
	}
	for i := range checks {
		if checks[i].Hostname == "" {
			checks[i].Hostname = dd.hostname
		}
		checks[i].Tags = append(checks[i].Tags, dd.tags...)
	}

	if len(events) != 0 {
		// this endpoint is not documented at all, its existence is only known from
		// the official dd-agent
		// we don't actually pass all the body keys that dd-agent passes here... but
		// it still works
		err := vhttp.PostHelper(context.TODO(), dd.HTTPClient, dd.statsd, dd.traceClient, http.MethodPost, fmt.Sprintf("%s/intake?api_key=%s", dd.DDHostname, dd.APIKey), map[string]map[string][]samplers.UDPEvent{
			"events": {
				"api": events,
			},
		}, "flush_events", true, dd.log)
		if err == nil {
			dd.log.WithField("events", len(events)).Info("Completed flushing events to Datadog")
		} else {
			dd.log.WithFields(logrus.Fields{
				"events":        len(events),
				logrus.ErrorKey: err}).Warn("Error flushing events to Datadog")
		}
	}

	if len(checks) != 0 {
		// this endpoint is not documented to take an array... but it does
		// another curious constraint of this endpoint is that it does not
		// support "Content-Encoding: deflate"
		err := vhttp.PostHelper(context.TODO(), dd.HTTPClient, dd.statsd, dd.traceClient, http.MethodPost, fmt.Sprintf("%s/api/v1/check_run?api_key=%s", dd.DDHostname, dd.APIKey), checks, "flush_checks", false, dd.log)
		if err == nil {
			dd.log.WithField("checks", len(checks)).Info("Completed flushing service checks to Datadog")
		} else {
			dd.log.WithFields(logrus.Fields{
				"checks":        len(checks),
				logrus.ErrorKey: err}).Warn("Error flushing checks to Datadog")
		}
	}
}

func (dd *DatadogMetricSink) finalizeMetrics(metrics []samplers.InterMetric) []DDMetric {
	ddMetrics := make([]DDMetric, len(metrics))
	for i, m := range metrics {
		// Defensively copy tags since we're gonna mutate it
		tags := make([]string, len(dd.tags))
		copy(tags, dd.tags)

		metricType := ""
		value := m.Value

		switch m.Type {
		case samplers.CounterMetric:
			// We convert counters into rates for Datadog
			metricType = "rate"
			value = m.Value / dd.interval
		case samplers.GaugeMetric:
			metricType = "gauge"
		default:
			dd.log.WithField("metric_type", m.Type).Warn("Encountered an unknown metric type")
			continue
		}

		ddMetric := DDMetric{
			Name: m.Name,
			Value: [1][2]float64{
				[2]float64{
					float64(m.Timestamp), value,
				},
			},
			Tags:       tags,
			MetricType: metricType,
			Interval:   int32(dd.interval),
		}

		// Let's look for "magic tags" that override metric fields host and device.
		for _, tag := range m.Tags {
			// This overrides hostname
			if strings.HasPrefix(tag, "host:") {
				// Override the hostname with the tag, trimming off the prefix.
				ddMetric.Hostname = tag[5:]
			} else if strings.HasPrefix(tag, "device:") {
				// Same as above, but device this time
				ddMetric.DeviceName = tag[7:]
			} else {
				// Add it, no reason to exclude it.
				ddMetric.Tags = append(ddMetric.Tags, tag)
			}
		}
		if ddMetric.Hostname == "" {
			// No magic tag, set the hostname
			ddMetric.Hostname = dd.hostname
		}
		ddMetrics[i] = ddMetric
	}

	return ddMetrics
}

func (dd *DatadogMetricSink) flushPart(ctx context.Context, metricSlice []DDMetric, wg *sync.WaitGroup) {
	defer wg.Done()
	vhttp.PostHelper(ctx, dd.HTTPClient, dd.statsd, dd.traceClient, http.MethodPost, fmt.Sprintf("%s/api/v1/series?api_key=%s", dd.DDHostname, dd.APIKey), map[string][]DDMetric{
		"series": metricSlice,
	}, "flush", true, dd.log)
}

// DatadogSpan represents a trace span as JSON for the
// Datadog tracing API.
type DatadogSpan struct {
	Duration int64              `json:"duration"`
	Error    int64              `json:"error"`
	Meta     map[string]string  `json:"meta"`
	Metrics  map[string]float64 `json:"metrics"`
	Name     string             `json:"name"`
	ParentID int64              `json:"parent_id,omitempty"`
	Resource string             `json:"resource,omitempty"`
	Service  string             `json:"service"`
	SpanID   int64              `json:"span_id"`
	Start    int64              `json:"start"`
	TraceID  int64              `json:"trace_id"`
	Type     string             `json:"type"`
}

// DatadogSpanSink is a sink for sending spans to a Datadog trace agent.
type DatadogSpanSink struct {
	HTTPClient   *http.Client
	buffer       *ring.Ring
	bufferSize   int
	mutex        *sync.Mutex
	stats        *statsd.Client
	commonTags   map[string]string
	traceAddress string
	traceClient  *trace.Client
	log          *logrus.Logger
}

// NewDatadogSpanSink creates a new Datadog sink for trace spans.
func NewDatadogSpanSink(address string, bufferSize int, stats *statsd.Client, httpClient *http.Client, commonTags map[string]string, log *logrus.Logger) (*DatadogSpanSink, error) {
	return &DatadogSpanSink{
		HTTPClient:   httpClient,
		bufferSize:   bufferSize,
		buffer:       ring.New(bufferSize),
		mutex:        &sync.Mutex{},
		stats:        stats,
		commonTags:   commonTags,
		traceAddress: address,
		log:          log,
	}, nil
}

// Name returns the name of this sink.
func (dd *DatadogSpanSink) Name() string {
	return "datadog"
}

// Start performs final adjustments on the sink.
func (dd *DatadogSpanSink) Start(cl *trace.Client) error {
	dd.traceClient = cl
	return nil
}

// Ingest takes the span and adds it to the ringbuffer.
func (dd *DatadogSpanSink) Ingest(span *ssf.SSFSpan) error {
	if err := protocol.ValidateTrace(span); err != nil {
		return err
	}
	dd.mutex.Lock()
	defer dd.mutex.Unlock()

	dd.buffer.Value = span
	dd.buffer = dd.buffer.Next()
	return nil
}

// Flush signals the sink to send it's spans to their destination. For this
// sync it means we'll be making an HTTP request to send them along. We assume
// it's beneficial to performance to defer these until the normal 10s flush.
func (dd *DatadogSpanSink) Flush() {
	dd.mutex.Lock()

	ssfSpans := make([]*ssf.SSFSpan, 0, dd.buffer.Len())

	dd.buffer.Do(func(t interface{}) {
		const tooEarly = 1497
		const tooLate = 1497629343000000

		if t != nil {
			ssfSpan, ok := t.(*ssf.SSFSpan)
			if !ok {
				dd.log.Error("Got an unknown object in tracing ring!")
				// We'll just skip this one so we don't poison pill or anything.
				return
			}

			var timeErr string
			if ssfSpan.StartTimestamp < tooEarly {
				timeErr = "type:tooEarly"
			}
			if ssfSpan.StartTimestamp > tooLate {
				timeErr = "type:tooLate"
			}
			if timeErr != "" {
				dd.stats.Incr("worker.trace.sink.timestamp_error", []string{timeErr}, 1) // TODO tag as dd?
			}

			if ssfSpan.Tags == nil {
				ssfSpan.Tags = make(map[string]string)
			}

			// Add common tags from veneur's config
			// this will overwrite tags already present on the span
			for k, v := range dd.commonTags {
				ssfSpan.Tags[k] = v
			}
			ssfSpans = append(ssfSpans, ssfSpan)
		}
	})

	// Reset the ring.
	dd.buffer = ring.New(dd.bufferSize)

	// We're done manipulating stuff, let Ingest loose again.
	dd.mutex.Unlock()

	serviceCount := make(map[string]int64)
	// Datadog wants the spans for each trace in an array, so make a map.
	traceMap := map[int64][]*DatadogSpan{}
	// Convert the SSFSpans into Datadog Spans
	for _, span := range ssfSpans {
		// -1 is a canonical way of passing in invalid info in Go
		// so we should support that too
		parentID := span.ParentId

		// check if this is the root span
		if parentID <= 0 {
			// we need parentId to be zero for json:omitempty to work
			parentID = 0
		}

		resource := span.Tags[DatadogResourceKey]
		if resource == "" {
			resource = "unknown"
		}

		tags := map[string]string{}
		// Get the span's existing tags
		for k, v := range span.Tags {
			tags[k] = v
		}

		name := span.Name
		if name == "" {
			name = "unknown"
		}

		delete(tags, DatadogResourceKey)

		var errorCode int64
		if span.Error {
			errorCode = 2
		}

		ddspan := &DatadogSpan{
			TraceID:  span.TraceId,
			SpanID:   span.Id,
			ParentID: parentID,
			Service:  span.Service,
			Name:     name,
			Resource: resource,
			Start:    span.StartTimestamp,
			Duration: span.EndTimestamp - span.StartTimestamp,
			// TODO don't hardcode
			Type:  "web",
			Error: errorCode,
			Meta:  tags,
		}
		serviceCount[span.Service]++
		if _, ok := traceMap[span.TraceId]; !ok {
			traceMap[span.TraceId] = []*DatadogSpan{}
		}
		traceMap[span.TraceId] = append(traceMap[span.TraceId], ddspan)
	}
	// Smush the spans into a two-dimensionall array now that they are grouped by trace id.
	finalTraces := make([][]*DatadogSpan, len(traceMap))
	idx := 0
	for _, val := range traceMap {
		finalTraces[idx] = val
		idx++
	}

	if len(finalTraces) != 0 {
		// this endpoint is not documented to take an array... but it does
		// another curious constraint of this endpoint is that it does not
		// support "Content-Encoding: deflate"

		err := vhttp.PostHelper(context.TODO(), dd.HTTPClient, dd.stats, dd.traceClient, http.MethodPut, fmt.Sprintf("%s/v0.3/traces", dd.traceAddress), finalTraces, "flush_traces", false, dd.log)

		if err == nil {
			dd.log.WithField("traces", len(finalTraces)).Info("Completed flushing traces to Datadog")
			dd.stats.Count(totalSpansFlushedMetricKey, int64(len(ssfSpans)), []string{"sink:datadog"}, 1)
			// TODO: Per service counters?
		} else {
			dd.log.WithFields(logrus.Fields{
				"traces":        len(finalTraces),
				logrus.ErrorKey: err}).Warn("Error flushing traces to Datadog")
		}
		for service, count := range serviceCount {
			dd.stats.Count(totalSpansFlushedMetricKey, count, []string{"sink:datadog", fmt.Sprintf("service:%s", service)}, 1)
		}
	} else {
		dd.log.Info("No traces to flush to Datadog, skipping.")
	}
}
