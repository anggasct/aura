package fetch

import (
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/configkit"
	"github.com/anggasct/aura/internal/platform/fault"
)

func TestDefaultConfigIsValid(t *testing.T) {
	cfg := Default()
	if time.Duration(cfg.Timeout) != 30*time.Second || cfg.MaxRedirects != 5 || cfg.MaxEncodedBytes != 2097152 || cfg.MaxDecodedBytes != 8388608 {
		t.Fatalf("Default() = %+v", cfg)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Default().Validate() = %v", err)
	}
}

func TestConfigValidateNilReceiver(t *testing.T) {
	var cfg *Config
	if err := cfg.Validate(); err != nil {
		t.Fatalf("nil.Validate() = %v", err)
	}
}

func TestConfigValidateBoundaries(t *testing.T) {
	accepted := map[string]func(*Config){
		"min redirects zero": func(c *Config) { c.MaxRedirects = 0 },
		"max redirects ten":  func(c *Config) { c.MaxRedirects = 10 },
		"minimal positive": func(c *Config) {
			c.Timeout = configkit.Duration(time.Millisecond)
			c.MaxEncodedBytes = 1
			c.MaxDecodedBytes = 1
		},
	}
	for name, mutate := range accepted {
		t.Run("accepts "+name, func(t *testing.T) {
			cfg := Default()
			mutate(&cfg)
			if err := cfg.Validate(); err != nil {
				t.Fatalf("Validate() = %v, want nil", err)
			}
		})
	}

	rejected := map[string]struct {
		mutate   func(*Config)
		wantPath string
		wantText string
	}{
		"zero timeout": {
			mutate:   func(c *Config) { c.Timeout = 0 },
			wantPath: "tools.fetch.timeout",
			wantText: "must be positive",
		},
		"negative timeout": {
			mutate:   func(c *Config) { c.Timeout = -configkit.Duration(time.Second) },
			wantPath: "tools.fetch.timeout",
			wantText: "must be positive",
		},
		"negative redirects": {
			mutate:   func(c *Config) { c.MaxRedirects = -1 },
			wantPath: "tools.fetch.max_redirects",
			wantText: "must be between 0 and 10",
		},
		"redirects above ten": {
			mutate:   func(c *Config) { c.MaxRedirects = 11 },
			wantPath: "tools.fetch.max_redirects",
			wantText: "must be between 0 and 10",
		},
		"zero encoded bytes": {
			mutate:   func(c *Config) { c.MaxEncodedBytes = 0 },
			wantPath: "tools.fetch",
			wantText: "body limits must be positive",
		},
		"zero decoded bytes": {
			mutate:   func(c *Config) { c.MaxDecodedBytes = 0 },
			wantPath: "tools.fetch",
			wantText: "body limits must be positive",
		},
	}
	for name, tc := range rejected {
		t.Run("rejects "+name, func(t *testing.T) {
			cfg := Default()
			tc.mutate(&cfg)
			err := configkit.Under("tools.fetch", cfg.Validate())
			if err == nil {
				t.Fatal("Validate() succeeded, want error")
			}
			if !fault.HasCode(err, configkit.ErrorCodeInvalid) {
				t.Fatalf("err = %v, want config_invalid", err)
			}
			if !strings.Contains(err.Error(), tc.wantPath+": ") || !strings.Contains(err.Error(), tc.wantText) {
				t.Fatalf("err = %q, want path %q and detail %q", err.Error(), tc.wantPath, tc.wantText)
			}
		})
	}
}
