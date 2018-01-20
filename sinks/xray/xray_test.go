package xray

import (
	"testing"

	"github.com/DataDog/datadog-go/statsd"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
)

func TestConstructor(t *testing.T) {
	logger := logrus.StandardLogger()
	stats, _ := statsd.NewBuffered("localhost:1235", 1024)

	sink, err := NewXRaySpanSink("127.0.0.1:2000", stats, map[string]string{"foo": "bar"}, logger)
	assert.NoError(t, err)
	assert.Equal(t, "xray", sink.Name())
	assert.Equal(t, "bar", sink.commonTags["foo"])
	assert.Equal(t, "127.0.0.1:2000", sink.daemonAddr)
}
