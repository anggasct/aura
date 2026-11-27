package codeexec

import (
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/configkit"
)

func TestCodeLimitsReadEveryConfiguredBound(t *testing.T) {
	configured := Config{
		Timeout:          configkit.Duration(7 * time.Second),
		BuildTimeout:     configkit.Duration(21 * time.Second),
		MaxSourceBytes:   11,
		MaxStdinBytes:    12,
		MaxStdoutBytes:   13,
		MaxStderrBytes:   14,
		MaxOutputFiles:   15,
		MaxArtifactBytes: 16,
		MaxMemoryBytes:   17,
		MaxProcesses:     18,
		MaxRunDirBytes:   19,
	}
	want := Limits{
		Timeout: 7 * time.Second, BuildTimeout: 21 * time.Second, MaxSourceBytes: 11, MaxStdinBytes: 12, MaxStdoutBytes: 13, MaxStderrBytes: 14,
		MaxMemoryBytes: 17, MaxProcesses: 18, MaxRunDirBytes: 19, MaxFileBytes: 16, MaxOutputFiles: 15,
	}
	if got := configured.Limits(); got != want {
		t.Fatalf("Limits() = %+v, want %+v", got, want)
	}
}

func TestCodeLimitsKeepAFileInsideTheRunDirectory(t *testing.T) {
	configured := Default()
	configured.MaxArtifactBytes = configured.MaxRunDirBytes * 2
	if got := configured.Limits(); got.MaxFileBytes != int64(configured.MaxRunDirBytes) {
		t.Fatalf("file limit = %d, want it capped at the run directory limit %d", got.MaxFileBytes, configured.MaxRunDirBytes)
	}
}

func TestConfigValidateRejectsInvalidBounds(t *testing.T) {
	var nilCfg *Config
	if err := nilCfg.Validate(); err == nil {
		t.Fatal("nil config validated without error")
	}
	if got := nilCfg.Limits(); got != (Limits{}) {
		t.Fatalf("nil Limits() = %+v, want zero", got)
	}

	defaults := Default()
	if err := defaults.Validate(); err != nil {
		t.Fatalf("Default().Validate(): %v", err)
	}

	cases := []struct {
		name     string
		mutate   func(*Config)
		wantPath string
	}{
		{"timeout below min", func(c *Config) { c.Timeout = configkit.Duration(MinTimeout - 1) }, "timeout"},
		{"timeout above max", func(c *Config) { c.Timeout = configkit.Duration(MaxTimeout + 1) }, "timeout"},
		{"build timeout below min", func(c *Config) { c.BuildTimeout = configkit.Duration(MinBuildTimeout - 1) }, "build_timeout"},
		{"build timeout above max", func(c *Config) { c.BuildTimeout = configkit.Duration(MaxBuildTimeout + 1) }, "build_timeout"},
		{"source zero", func(c *Config) { c.MaxSourceBytes = 0 }, "max_source_bytes"},
		{"source above max", func(c *Config) { c.MaxSourceBytes = MaxSourceBytes + 1 }, "max_source_bytes"},
		{"stdin zero", func(c *Config) { c.MaxStdinBytes = 0 }, "max_stdin_bytes"},
		{"stdout above max", func(c *Config) { c.MaxStdoutBytes = MaxOutputBytes + 1 }, "max_stdout_bytes"},
		{"stderr zero", func(c *Config) { c.MaxStderrBytes = 0 }, "max_stderr_bytes"},
		{"memory below min", func(c *Config) { c.MaxMemoryBytes = MinMemoryBytes - 1 }, "max_memory_bytes"},
		{"output files zero", func(c *Config) { c.MaxOutputFiles = 0 }, "max_output_files"},
		{"output files above max", func(c *Config) { c.MaxOutputFiles = MaxOutputFiles + 1 }, "max_output_files"},
		{"processes zero", func(c *Config) { c.MaxProcesses = 0 }, "max_processes"},
		{"processes above max", func(c *Config) { c.MaxProcesses = MaxProcesses + 1 }, "max_processes"},
		{"artifact exceeds run dir", func(c *Config) {
			c.MaxArtifactBytes = 2 << 20
			c.MaxRunDirBytes = 1 << 20
		}, "max_artifact_bytes"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			tc.mutate(&cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("Validate() accepted %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantPath) {
				t.Fatalf("Validate() error = %v, want path %q", err, tc.wantPath)
			}
		})
	}
}
