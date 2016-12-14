package localfile

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/Sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stripe/veneur/samplers"
)

type badWriter struct{}

func (w *badWriter) Write(b []byte) (n int, err error) {
	return 0, fmt.Errorf("This writer fails when you try to write!")
}

func TestName(t *testing.T) {
	plugin := Plugin{FilePath: "doesntexist.txt", Logger: logrus.New()}
	assert.Equal(t, plugin.Name(), "localfile")
}

func TestAppendToWriter(t *testing.T) {
	b := &bytes.Buffer{}

	metrics := []samplers.DDMetric{
		samplers.DDMetric{
			Name: "a.b.c.max",
			Value: [1][2]float64{[2]float64{1476119058,
				100}},
			Tags: []string{"foo:bar",
				"baz:quz"},
			MetricType: "gauge",
			Hostname:   "globalstats",
			DeviceName: "food",
			Interval:   0,
		},
	}

	err := appendToWriter(b, metrics, metrics[0].Hostname)
	assert.NoError(t, err)
	assert.NotEqual(t, b.Len(), 0)
}

func TestHandlesErrorsInAppendToWriter(t *testing.T) {
	b := &badWriter{}

	err := appendToWriter(b, []samplers.DDMetric{
		samplers.DDMetric{
			Name:       "sketchy.metric",
			Value:      [1][2]float64{[2]float64{1476119058, 100}},
			Tags:       []string{"skepticism:high"},
			MetricType: "gauge",
			Hostname:   "globblestoots",
			DeviceName: "¬_¬",
			Interval:   -1,
		},
	}, "globblestoots")

	assert.Error(t, err)
}

func TestWritesToDevNull(t *testing.T) {
	plugin := Plugin{FilePath: "/dev/null", Logger: logrus.New()}
	err := plugin.Flush([]samplers.DDMetric{
		samplers.DDMetric{
			Name:       "sketchy.metric",
			Value:      [1][2]float64{[2]float64{1476119058, 100}},
			Tags:       []string{"skepticism:high"},
			MetricType: "gauge",
			Hostname:   "globblestoots",
			DeviceName: "¬_¬",
			Interval:   -1,
		},
	}, "globblestoots")
	assert.NoError(t, err)
}

func TestFailsWritingToInvalidPath(t *testing.T) {
	plugin := Plugin{FilePath: "", Logger: logrus.New()}
	err := plugin.Flush([]samplers.DDMetric{
		samplers.DDMetric{
			Name:       "sketchy.metric",
			Value:      [1][2]float64{[2]float64{1476119058, 100}},
			Tags:       []string{"skepticism:high"},
			MetricType: "gauge",
			Hostname:   "globblestoots",
			DeviceName: "¬_¬",
			Interval:   -1,
		},
	}, "globblestoots")
	assert.Error(t, err)
}
