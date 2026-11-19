package vision

import (
	"errors"
	"strings"
	"time"

	"github.com/anggasct/aura/internal/platform/configkit"
)

type ProviderLimits struct {
	MaxImages       int    `json:"max_images,omitempty"`
	MaxRequestBytes int64  `json:"max_request_bytes,omitempty"`
	Detail          string `json:"detail,omitempty"`
}

type Config struct {
	Enabled              bool                      `json:"enabled"`
	MaxEncodedBytes      int64                     `json:"max_encoded_bytes"`
	MaxPixels            int64                     `json:"max_pixels"`
	MaxDimension         int                       `json:"max_dimension"`
	MaxImages            int                       `json:"max_images"`
	MaxFrames            int                       `json:"max_frames"`
	DecodeTimeout        configkit.Duration        `json:"decode_timeout"`
	MaxDecodeConcurrency int                       `json:"max_decode_concurrency"`
	MaxRequestBytes      int64                     `json:"max_request_bytes"`
	StripMetadata        bool                      `json:"strip_metadata"`
	OrientNormalize      bool                      `json:"orient_normalize"`
	Detail               string                    `json:"detail"`
	TransformVersion     string                    `json:"transform_version"`
	MaxTransformMemory   configkit.ByteSize        `json:"max_transform_memory_bytes"`
	Providers            map[string]ProviderLimits `json:"providers,omitempty"`
}

func Default() Config {
	return Config{
		Enabled:              true,
		MaxEncodedBytes:      20971520,
		MaxPixels:            40000000,
		MaxDimension:         8192,
		MaxImages:            4,
		MaxFrames:            1,
		DecodeTimeout:        configkit.Duration(5 * time.Second),
		MaxDecodeConcurrency: 2,
		MaxRequestBytes:      33554432,
		StripMetadata:        true,
		OrientNormalize:      true,
		Detail:               "auto",
		TransformVersion:     "v1",
		MaxTransformMemory:   configkit.ByteSize(268435456),
		Providers:            map[string]ProviderLimits{},
	}
}

func (c *Config) Validate() error {
	var problems []error
	if c.MaxEncodedBytes <= 0 {
		problems = append(problems, configkit.Invalid("max_encoded_bytes", "must be positive"))
	}
	if c.MaxPixels <= 0 {
		problems = append(problems, configkit.Invalid("max_pixels", "must be positive"))
	}
	if c.MaxDimension <= 0 {
		problems = append(problems, configkit.Invalid("max_dimension", "must be positive"))
	}
	if c.MaxImages <= 0 {
		problems = append(problems, configkit.Invalid("max_images", "must be positive"))
	}
	if c.MaxFrames != 1 {
		problems = append(problems, configkit.Invalid("max_frames", "must be 1"))
	}
	if time.Duration(c.DecodeTimeout) <= 0 || time.Duration(c.DecodeTimeout) > 30*time.Second {
		problems = append(problems, configkit.Invalid("decode_timeout", "must be positive and at most 30s"))
	}
	if c.MaxDecodeConcurrency <= 0 {
		problems = append(problems, configkit.Invalid("max_decode_concurrency", "must be positive"))
	}
	if c.MaxRequestBytes <= 0 {
		problems = append(problems, configkit.Invalid("max_request_bytes", "must be positive"))
	}
	if c.MaxTransformMemory <= 0 {
		problems = append(problems, configkit.Invalid("max_transform_memory_bytes", "must be positive"))
	}
	if !ValidDetail(c.Detail) {
		problems = append(problems, configkit.Invalid("detail", "must be auto, low, or high"))
	}
	if strings.TrimSpace(c.TransformVersion) == "" {
		problems = append(problems, configkit.Invalid("transform_version", "must not be empty"))
	}
	return errors.Join(problems...)
}

func ValidDetail(detail string) bool {
	return detail == "auto" || detail == "low" || detail == "high"
}
