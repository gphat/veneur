package xray

import (
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"math"
	"net"
	"regexp"
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
	Namespace   string            `json:"namespace"`
	Error       bool              `json:"error"`
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
	spansDropped    int64
	spansHandled    int64
	nameRegex       *regexp.Regexp
}

var _ sinks.SpanSink = &XRaySpanSink{}

// NewXRaySpanSink creates a new instance of a XRaySpanSink.
func NewXRaySpanSink(daemonAddr string, sampleRatePercentage int, commonTags map[string]string, log *logrus.Logger) (*XRaySpanSink, error) {

	log.WithFields(logrus.Fields{
		"Address": daemonAddr,
	}).Info("Creating X-Ray client")

	var sampleThreshold uint32
	if sampleRatePercentage <= 0 || sampleRatePercentage > 100 {
		return nil, errors.New("Span sample rate percentage must be greater than 0% and less than or equal to 100%")
	}

	// Set the sample threshold to (sample rate) * (maximum value of uint32), so that
	// we can store it as a uint32 instead of a float64 and compare apples-to-apples
	// with the output of our hashing algorithm.
	sampleThreshold = uint32(sampleRatePercentage * math.MaxUint32 / 100)

	// Build a regex for cleaning names based on valid characters from:
	// https://docs.aws.amazon.com/xray/latest/devguide/xray-api-segmentdocuments.html#api-segmentdocuments-fields
	reg, err := regexp.Compile("[^a-zA-Z0-9_\\.\\:\\/\\%\\&#=+\\-\\@\\s\\\\]+")
	if err != nil {
		return nil, err
	}

	return &XRaySpanSink{
		daemonAddr:      daemonAddr,
		sampleThreshold: sampleThreshold,
		commonTags:      commonTags,
		log:             log,
		nameRegex:       reg,
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
		atomic.AddInt64(&x.spansDropped, 1)
		return nil
	}

	annos := map[string]string{}
	for k, v := range x.commonTags {
		annos[k] = v
	}

	name := string(x.nameRegex.ReplaceAll([]byte(ssfSpan.Service), []byte("_")))
	if len(name) > 200 {
		name = name[:200]
	}

	// The fields below are defined here:
	// https://docs.aws.amazon.com/xray/latest/devguide/xray-api-segmentdocuments.html#api-segmentdocuments-fields
	segment := XRaySegment{
		// ID is a 64-bit hex
		ID: fmt.Sprintf("%016x", ssfSpan.Id),
		// Trace ID is version-startTimeUnixAs8CharHex-traceIdAs24CharHex
		TraceID:     fmt.Sprintf("1-%08x-%024x", ssfSpan.StartTimestamp/1e9, ssfSpan.TraceId),
		Name:        name,
		StartTime:   float64(float64(ssfSpan.StartTimestamp) / float64(time.Second)),
		EndTime:     float64(float64(ssfSpan.EndTimestamp) / float64(time.Second)),
		Annotations: annos,
		Namespace:   "remote",
		Error:       ssfSpan.Error,
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
	metrics.ReportBatch(x.traceClient, []*ssf.SSFSample{
		ssf.Count(sinks.MetricKeyTotalSpansFlushed, float32(atomic.LoadInt64(&x.spansHandled)), map[string]string{"sink": x.Name()}),
		ssf.Count(sinks.MetricKeyTotalSpansDropped, float32(atomic.LoadInt64(&x.spansDropped)), map[string]string{"sink": x.Name()}),
	})
	atomic.SwapInt64(&x.spansHandled, 0)
	atomic.SwapInt64(&x.spansDropped, 0)

	x.log.WithField("total_spans", atomic.LoadInt64(&x.spansHandled)).Info("Checkpointing flushed spans for X-Ray")
}
