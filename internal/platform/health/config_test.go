package health

import (
	"testing"

	"github.com/anggasct/aura/internal/platform/configkit"
	"github.com/anggasct/aura/internal/platform/fault"
)

func TestValidateHealthBounds(t *testing.T) {
	valid := Default()
	cases := []struct {
		name   string
		mutate func(*Config)
	}{
		{"interval zero", func(h *Config) { h.CheckInterval = 0 }},
		{"timeout exceeds interval", func(h *Config) { h.CheckTimeout = valid.CheckInterval * 2 }},
		{"warning percent zero", func(h *Config) { h.DiskWarningPercent = 0 }},
		{"critical above warning", func(h *Config) { h.DiskCriticalPercent = valid.DiskWarningPercent }},
		{"floor zero", func(h *Config) { h.DiskCriticalFloorBytes = 0 }},
		{"backup age zero", func(h *Config) { h.BackupMaxAge = 0 }},
		{"restore age negative", func(h *Config) { h.RestoreVerificationMaxAge = -1 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			healthCfg := valid
			tc.mutate(&healthCfg)
			err := healthCfg.Validate()
			if !fault.HasCode(err, configkit.ErrorCodeInvalid) {
				t.Errorf("validateHealth err = %v, want config_invalid", err)
			}
		})
	}
	if err := valid.Validate(); err != nil {
		t.Errorf("valid health config rejected: %v", err)
	}
}
