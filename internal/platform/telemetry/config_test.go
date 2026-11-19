package telemetry

import (
	"strings"
	"testing"
)

func TestTelemetryConfigValidate(t *testing.T) {
	const distinctive = "exporter-canary-7f3a"
	cases := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{"exporter unsupported", func(c *Config) { c.Exporter = distinctive }, "exporter: config_invalid: must be one of none, stdout, otlp_grpc, otlp_http"},
		{"sample ratio below zero", func(c *Config) { c.SampleRatio = -0.1 }, "sample_ratio: config_invalid: -0.100000 is out of range (0-1)"},
		{"sample ratio above one", func(c *Config) { c.SampleRatio = 1.5 }, "sample_ratio: config_invalid: 1.500000 is out of range (0-1)"},
		{"queue size negative", func(c *Config) { c.QueueSize = -1 }, "queue_size: config_invalid: must not be negative"},
		{"export timeout negative", func(c *Config) { c.ExportTimeout = -1 }, "export_timeout: config_invalid: must not be negative"},
		{"otlp grpc without endpoint", func(c *Config) { c.Exporter = ExporterOTLPGRPC }, `endpoint: config_invalid: is required for exporter "otlp_grpc"`},
		{"otlp http without endpoint", func(c *Config) { c.Exporter = ExporterOTLPHTTP }, `endpoint: config_invalid: is required for exporter "otlp_http"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			tc.mutate(&cfg)
			err := cfg.Validate()
			if err == nil || err.Error() != tc.want {
				t.Fatalf("Validate() = %v, want %q", err, tc.want)
			}
			if strings.Contains(err.Error(), distinctive) {
				t.Errorf("Validate() = %v, which echoes the exporter value", err)
			}
		})
	}
	accepted := map[string]func(*Config){
		"empty exporter":     func(c *Config) { c.Exporter = "" },
		"stdout exporter":    func(c *Config) { c.Exporter = ExporterStdout },
		"otlp with endpoint": func(c *Config) { c.Exporter = ExporterOTLPGRPC; c.Endpoint = "localhost:4317" },
	}
	for name, mutate := range accepted {
		t.Run(name, func(t *testing.T) {
			cfg := Default()
			mutate(&cfg)
			if err := cfg.Validate(); err != nil {
				t.Errorf("Validate() = %v, want nil", err)
			}
		})
	}
	if err := Default().Validate(); err != nil {
		t.Errorf("Default().Validate() = %v, want nil", err)
	}
}
