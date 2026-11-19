package telemetry

import (
	"fmt"
	"time"

	"github.com/anggasct/aura/internal/platform/configkit"
	"github.com/anggasct/aura/internal/platform/secret"
)

type Config struct {
	Exporter      string             `json:"exporter"`
	Endpoint      string             `json:"endpoint"`
	CredentialRef secret.Ref         `json:"credential_ref"`
	SampleRatio   float64            `json:"sample_ratio"`
	QueueSize     int                `json:"queue_size"`
	ExportTimeout configkit.Duration `json:"export_timeout"`
}

func Default() Config {
	return Config{
		Exporter:      "none",
		SampleRatio:   0.10,
		QueueSize:     2048,
		ExportTimeout: configkit.Duration(5 * time.Second),
	}
}

func (c Config) Validate() error {
	switch c.Exporter {
	case "", "none", "stdout", "otlp_grpc", "otlp_http":
	default:
		return configkit.Invalid("exporter", "must be one of none, stdout, otlp_grpc, otlp_http")
	}
	if c.SampleRatio < 0 || c.SampleRatio > 1 {
		return configkit.Invalid("sample_ratio", fmt.Sprintf("%f is out of range (0-1)", c.SampleRatio))
	}
	if c.QueueSize < 0 {
		return configkit.Invalid("queue_size", "must not be negative")
	}
	if c.ExportTimeout < 0 {
		return configkit.Invalid("export_timeout", "must not be negative")
	}
	if (c.Exporter == "otlp_grpc" || c.Exporter == "otlp_http") && c.Endpoint == "" {
		return configkit.Invalid("endpoint", fmt.Sprintf("is required for exporter %q", c.Exporter))
	}
	return nil
}
