package localfile

import (
	"compress/gzip"
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/stripe/veneur/plugins"
	"github.com/stripe/veneur/plugins/s3"
	"github.com/stripe/veneur/samplers"
)

var _ plugins.Plugin = &Plugin{}

// Plugin is the LocalFile plugin that we'll use in Veneur
type Plugin struct {
	FilePath string
	Logger   *logrus.Logger
}

// Delimiter defines what kind of delimiter we'll use in the CSV format -- in this case, we want TSV
const Delimiter = '\t'

// Flush the metrics from the LocalFilePlugin
func (p *Plugin) Flush(metrics []samplers.DDMetric, hostname string) error {
	f, err := os.OpenFile(p.FilePath, os.O_RDWR|os.O_APPEND|os.O_CREATE, os.ModePerm)
	defer f.Close()

	if err != nil {
		return fmt.Errorf("couldn't open %s for appending: %s", p.FilePath, err)
	}
	appendToWriter(f, metrics, hostname)
	return nil
}

func appendToWriter(appender io.Writer, metrics []samplers.DDMetric, hostname string) error {
	gzw := gzip.NewWriter(appender)
	w := csv.NewWriter(gzw)
	w.Comma = Delimiter

	partitionDate := time.Now()
	for _, metric := range metrics {
		s3.EncodeDDMetricCSV(metric, w, &partitionDate, hostname)
	}
	w.Flush()
	return w.Error()
}

// Name is the name of the LocalFilePlugin, i.e., "localfile"
func (p *Plugin) Name() string {
	return "localfile"
}
