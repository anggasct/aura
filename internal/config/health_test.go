package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeHealthConfig(t *testing.T, healthYAML string) LoadResult {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	full := "version: 1\n" + healthYAML
	if err := os.WriteFile(path, []byte(full), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	result, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return result
}

func TestLoadHealthDefaultsApplied(t *testing.T) {
	result := writeHealthConfig(t, "")
	h := result.Config.Health
	if h.Listen != "127.0.0.1:8281" {
		t.Errorf("Listen = %q", h.Listen)
	}
	if time.Duration(h.CheckInterval) != 60*time.Second || time.Duration(h.CheckTimeout) != 5*time.Second {
		t.Errorf("interval/timeout = %v/%v", h.CheckInterval, h.CheckTimeout)
	}
	if h.DiskWarningPercent != 15 || h.DiskCriticalPercent != 8 {
		t.Errorf("disk percents = %d/%d", h.DiskWarningPercent, h.DiskCriticalPercent)
	}
	if h.DiskCriticalFloorBytes != ByteSize(512<<20) {
		t.Errorf("floor = %v", h.DiskCriticalFloorBytes)
	}
	if time.Duration(h.BackupMaxAge) != 24*time.Hour {
		t.Errorf("backup max age = %v", h.BackupMaxAge)
	}
}

func TestLoadHealthAcceptsLoopbackListen(t *testing.T) {
	cases := map[string]string{
		"ipv4 loopback": "health:\n  listen: \"127.0.0.1:9000\"\n",
		"ipv6 loopback": "health:\n  listen: \"[::1]:9000\"\n",
		"localhost":     "health:\n  listen: \"localhost:9000\"\n",
	}
	for name, yaml := range cases {
		t.Run(name, func(t *testing.T) {
			result := writeHealthConfig(t, yaml)
			if result.Config.Health.Listen == "" {
				t.Fatal("listen not loaded")
			}
		})
	}
}

// Probe exposure beyond loopback is the admin surface's job, never this
// listener's.
func TestLoadHealthRejectsNonLoopbackListen(t *testing.T) {
	for _, listen := range []string{"0.0.0.0:8281", "192.168.1.10:8281", "example.com:8281", "8281", "127.0.0.1:notaport"} {
		dir := t.TempDir()
		path := filepath.Join(dir, "config.yaml")
		yaml := "version: 1\nhealth:\n  listen: \"" + listen + "\"\n"
		if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
			t.Fatalf("write config: %v", err)
		}
		_, err := Load(path)
		if _, ok := CodeOf(err); err == nil || !ok {
			t.Errorf("Load(listen=%q) err = %v, want config_invalid", listen, err)
		}
	}
}

func TestValidateHealthBounds(t *testing.T) {
	valid := Default().Health
	cases := []struct {
		name   string
		mutate func(*Health)
	}{
		{"interval zero", func(h *Health) { h.CheckInterval = 0 }},
		{"timeout exceeds interval", func(h *Health) { h.CheckTimeout = valid.CheckInterval * 2 }},
		{"warning percent zero", func(h *Health) { h.DiskWarningPercent = 0 }},
		{"critical above warning", func(h *Health) { h.DiskCriticalPercent = valid.DiskWarningPercent }},
		{"floor zero", func(h *Health) { h.DiskCriticalFloorBytes = 0 }},
		{"backup age zero", func(h *Health) { h.BackupMaxAge = 0 }},
		{"restore age negative", func(h *Health) { h.RestoreVerificationMaxAge = -1 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			healthCfg := valid
			tc.mutate(&healthCfg)
			err := validateHealth(healthCfg)
			if code, ok := CodeOf(err); !ok || code != ErrorCodeConfigInvalid {
				t.Errorf("validateHealth err = %v, want config_invalid", err)
			}
		})
	}
	if err := validateHealth(valid); err != nil {
		t.Errorf("valid health config rejected: %v", err)
	}
}

func TestLoadHealthShapeRejectsWrongTypes(t *testing.T) {
	yaml := "version: 1\nhealth:\n  check_interval: 60\n"
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("numeric duration must be rejected at the YAML shape pass")
	}
}

func TestLoadHealthEnvOverride(t *testing.T) {
	t.Setenv("AURA_HEALTH_CHECK_TIMEOUT", "9s")
	result := writeHealthConfig(t, "")
	if got := result.Config.Health.CheckTimeout; time.Duration(got) != 9*time.Second {
		t.Errorf("CheckTimeout = %v, want 9s from env", got)
	}
}
