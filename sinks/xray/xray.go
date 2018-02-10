package xray

import (
	"encoding/json"
	"fmt"
	"net"
	"sync/atomic"
	"time"

	"github.com/DataDog/datadog-go/statsd"
	"github.com/sirupsen/logrus"
	"github.com/stripe/veneur/protocol"
	"github.com/stripe/veneur/sinks"
	"github.com/stripe/veneur/ssf"
	"github.com/stripe/veneur/trace"
)

const subsegmentType = "subsegment"

var segmentHeader = []byte(`{"format": "json", "version": 1}` + "\n")

// XRaySegment is a trace segment for X-Ray as defined by:
// https://docs.aws.amazon.com/xray/latest/devguide/xray-api-segmentdocuments.html
type XRaySegment struct {
	Name        string            `json:"name"`
	ID          string            `json:"id"`
	TraceID     string            `json:"trace_id"`
	ParentID    string            `json:"parent_id,omitempty"`
	StartTime   float64           `json:"start_time"`
	EndTime     float64           `json:"end_time"`
	SegmentType string            `json:"type,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

// XRaySpanSink is a sink for spans to be sent to AWS X-Ray.
type XRaySpanSink struct {
	daemonAddr   string
	stats        *statsd.Client
	traceClient  *trace.Client
	conn         *net.UDPConn
	commonTags   map[string]string
	log          *logrus.Logger
	spansHandled int64
}

var _ sinks.SpanSink = &XRaySpanSink{}

// NewXRaySpanSink creates a new instance of a XRaySpanSink.
func NewXRaySpanSink(daemonAddr string, stats *statsd.Client, commonTags map[string]string, log *logrus.Logger) (*XRaySpanSink, error) {

	log.WithFields(logrus.Fields{
		"Address": daemonAddr,
	}).Info("Creating X-Ray client")

	return &XRaySpanSink{
		daemonAddr: daemonAddr,
		stats:      stats,
		commonTags: commonTags,
		log:        log,
	}, nil
}

// Start the sink
func (x *XRaySpanSink) Start(cl *trace.Client) error {
	x.traceClient = cl

	xrayDaemon, err := net.ResolveUDPAddr("udp", x.daemonAddr)
	if err != nil {
		return err
	}
	conn, err := net.DialUDP("udp", nil, xrayDaemon)
	if err != nil {
		return err
	}
	x.conn = conn

	return nil
}

// Name returns this sink's name.
func (x *XRaySpanSink) Name() string {
	return "xray"
}

// Ingest takes in a span and passed it along to the X-Ray client after
// some sanity checks and improvements are made.
func (x *XRaySpanSink) Ingest(ssfSpan *ssf.SSFSpan) error {
	if err := protocol.ValidateTrace(ssfSpan); err != nil {
		return err
	}

	annos := map[string]string{}
	for k, v := range x.commonTags {
		annos[k] = v
	}

	segment := XRaySegment{
		ID:          fmt.Sprintf("%016x", ssfSpan.Id),
		TraceID:     fmt.Sprintf("1-%08x-%024x", ssfSpan.StartTimestamp/1e9, ssfSpan.TraceId),
		Name:        ssfSpan.Service,
		StartTime:   float64(float64(ssfSpan.StartTimestamp) / float64(time.Second)),
		EndTime:     float64(float64(ssfSpan.EndTimestamp) / float64(time.Second)),
		Annotations: annos,
	}
	if ssfSpan.ParentId != 0 {
		segment.ParentID = fmt.Sprintf("%016x", ssfSpan.ParentId)
		segment.SegmentType = subsegmentType
	}
	b, err := json.Marshal(segment)
	if err != nil {
		x.log.WithError(err).Error("Error marshaling segment")
		return err
	}
	// Send the segment
	_, err = x.conn.Write(append(segmentHeader, b...))
	if err != nil {
		x.log.WithError(err).Error("Error sending segment")
		return err
	}

	atomic.AddInt64(&x.spansHandled, 1)
	return nil
}

// Flush doesn't need to do anything, so we emit metrics
// instead.
func (x *XRaySpanSink) Flush() {

	x.stats.Count(sinks.MetricKeyTotalSpansFlushed, atomic.LoadInt64(&x.spansHandled), []string{fmt.Sprintf("sink:%s", x.Name())}, 1)
	x.log.WithField("total_spans", atomic.LoadInt64(&x.spansHandled)).Debug("Checkpointing flushed spans for X-Ray")
	atomic.SwapInt64(&x.spansHandled, 0)
}
