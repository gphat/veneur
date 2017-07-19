package ssf

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSpan(t *testing.T) {

	counter := SSFSample{
		Name:   "foo.queries_executed",
		Metric: SSFSample_COUNTER,
		Value:  1.0,
		Tags: map[string]string{
			"table_name": "customers",
		},
	}
	span := SSFSpan{
		TraceId:        1234,
		Id:             1235,
		StartTimestamp: 0,
		EndTimestamp:   100,
		Metrics:        []*SSFSample{&counter},
	}

	assert.Equal(t, int64(1234), span.TraceId, "Trace ID matches")
	assert.Equal(t, 1, len(span.Metrics), "Number of metrics is correct")
}

func TestSamplesOnly(t *testing.T) {

	counter := SSFSample{
		Name:   "foo.queries_executed",
		Metric: SSFSample_COUNTER,
		Value:  1.0,
		Tags: map[string]string{
			"table_name": "customers",
		},
	}
	span := SSFSpan{
		Metrics: []*SSFSample{&counter},
	}

	assert.Equal(t, 1, len(span.Metrics), "Proper number of metrics")
	assert.Equal(t, 1, len(span.Metrics[0].Tags), "Proper number of tags")
}
