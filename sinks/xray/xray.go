package xray

import (
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"math"
	"net"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stripe/veneur/protocol"
	"github.com/stripe/veneur/sinks"
	"github.com/stripe/veneur/ssf"
	"github.com/stripe/veneur/trace"
	"github.com/stripe/veneur/trace/metrics"
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
	daemonAddr      string
	traceClient     *trace.Client
	conn            *net.UDPConn
	sampleThreshold uint32
	commonTags      map[string]string
	log             *logrus.Logger
	spansHandled    int64
}

var _ sinks.SpanSink = &XRaySpanSink{}

// NewXRaySpanSink creates a new instance of a XRaySpanSink.
func NewXRaySpanSink(daemonAddr string, sampleRatePercentage int, commonTags map[string]string, log *logrus.Logger) (*XRaySpanSink, error) {

	log.WithFields(logrus.Fields{
		"Address": daemonAddr,
	}).Info("Creating X-Ray client")

	var sampleThreshold uint32
	if sampleRatePercentage <= 0 || sampleRatePercentage > 100 {
		return nil, errors.New("Span sample rate percentage must be greater than 0%% and less than or equal to 100%%")
	}

	// Set the sample threshold to (sample rate) * (maximum value of uint32), so that
	// we can store it as a uint32 instead of a float64 and compare apples-to-apples
	// with the output of our hashing algorithm.
	sampleThreshold = uint32(sampleRatePercentage * math.MaxUint32 / 100)

	return &XRaySpanSink{
		daemonAddr:      daemonAddr,
		sampleThreshold: sampleThreshold,
		commonTags:      commonTags,
		log:             log,
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

	sampleCheckValue := []byte(strconv.FormatInt(ssfSpan.TraceId, 10))
	hashKey := crc32.ChecksumIEEE(sampleCheckValue)
	if hashKey > x.sampleThreshold {
		return nil
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
	metrics.ReportOne(x.traceClient,
		ssf.Count(sinks.MetricKeyTotalSpansFlushed, float32(atomic.LoadInt64(&x.spansHandled)), map[string]string{"sink": x.Name()}),
	)

	x.log.WithField("total_spans", atomic.LoadInt64(&x.spansHandled)).Debug("Checkpointing flushed spans for X-Ray")
	atomic.SwapInt64(&x.spansHandled, 0)
}
