package veneur

type Config struct {
	Aggregates          []string  `yaml:"aggregates"`
	APIHostname         string    `yaml:"api_hostname"`
	AwsAccessKeyID      string    `yaml:"aws_access_key_id"`
	AwsRegion           string    `yaml:"aws_region"`
	AwsS3Bucket         string    `yaml:"aws_s3_bucket"`
	AwsSecretAccessKey  string    `yaml:"aws_secret_access_key"`
	Debug               bool      `yaml:"debug"`
	EnableProfiling     bool      `yaml:"enable_profiling"`
	FlushFile           string    `yaml:"flush_file"`
	FlushMaxPerBody     int       `yaml:"flush_max_per_body"`
	ForwardAddress      string    `yaml:"forward_address"`
	Hostname            string    `yaml:"hostname"`
	HTTPAddress         string    `yaml:"http_address"`
	Interval            string    `yaml:"interval"`
	Key                 string    `yaml:"key"`
	MetricMaxLength     int       `yaml:"metric_max_length"`
	NumReaders          int       `yaml:"num_readers"`
	NumWorkers          int       `yaml:"num_workers"`
	Percentiles         []float64 `yaml:"percentiles"`
	ReadBufferSizeBytes int       `yaml:"read_buffer_size_bytes"`
	SentryDsn           string    `yaml:"sentry_dsn"`
	StatsAddress        string    `yaml:"stats_address"`
	Tags                []string  `yaml:"tags"`
	TraceAddress        string    `yaml:"trace_address"`
	TraceAPIAddress     string    `yaml:"trace_api_address"`
	TraceMaxLengthBytes int       `yaml:"trace_max_length_bytes"`
	UdpAddress          string    `yaml:"udp_address"`
}
