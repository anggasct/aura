package codeexec

import (
	"errors"
	"fmt"
	"time"

	"github.com/anggasct/aura/internal/platform/configkit"
)

const (
	MinTimeout = time.Second
	MaxTimeout = 10 * time.Minute

	MinBuildTimeout = 5 * time.Second
	MaxBuildTimeout = 10 * time.Minute

	MaxSourceBytes   = configkit.ByteSize(MaxCodeSourceBytes)
	MaxStdinBytes    = configkit.ByteSize(MaxCodeStdinBytes)
	MaxOutputBytes   = configkit.ByteSize(1 << 20)
	MaxOutputFiles   = MaxCodeOutputFiles
	MaxArtifactBytes = configkit.ByteSize(1 << 30)
	MinMemoryBytes   = configkit.ByteSize(64 << 20)
	MaxMemoryBytes   = configkit.ByteSize(16 << 30)
	MaxProcesses     = 4096
	MaxRunDirBytes   = configkit.ByteSize(16 << 30)
)

type Config struct {
	Timeout          configkit.Duration `json:"timeout"`
	BuildTimeout     configkit.Duration `json:"build_timeout"`
	MaxSourceBytes   configkit.ByteSize `json:"max_source_bytes"`
	MaxStdinBytes    configkit.ByteSize `json:"max_stdin_bytes"`
	MaxStdoutBytes   configkit.ByteSize `json:"max_stdout_bytes"`
	MaxStderrBytes   configkit.ByteSize `json:"max_stderr_bytes"`
	MaxOutputFiles   int                `json:"max_output_files"`
	MaxArtifactBytes configkit.ByteSize `json:"max_artifact_bytes"`
	MaxMemoryBytes   configkit.ByteSize `json:"max_memory_bytes"`
	MaxProcesses     int                `json:"max_processes"`
	MaxRunDirBytes   configkit.ByteSize `json:"max_run_dir_bytes"`
}

func Default() Config {
	return Config{
		Timeout:          configkit.Duration(30 * time.Second),
		BuildTimeout:     configkit.Duration(60 * time.Second),
		MaxSourceBytes:   configkit.ByteSize(256 << 10),
		MaxStdinBytes:    configkit.ByteSize(1 << 20),
		MaxStdoutBytes:   configkit.ByteSize(64 << 10),
		MaxStderrBytes:   configkit.ByteSize(64 << 10),
		MaxOutputFiles:   16,
		MaxArtifactBytes: configkit.ByteSize(32 << 20),
		MaxMemoryBytes:   configkit.ByteSize(512 << 20),
		MaxProcesses:     64,
		MaxRunDirBytes:   configkit.ByteSize(256 << 20),
	}
}

func (c *Config) Validate() error {
	if c == nil {
		return configkit.Invalid("", "must not be nil")
	}
	var problems []error
	if time.Duration(c.Timeout) < MinTimeout || time.Duration(c.Timeout) > MaxTimeout {
		problems = append(problems, configkit.Invalid("timeout", fmt.Sprintf("must be between %s and %s", MinTimeout, MaxTimeout)))
	}
	if time.Duration(c.BuildTimeout) < MinBuildTimeout || time.Duration(c.BuildTimeout) > MaxBuildTimeout {
		problems = append(problems, configkit.Invalid("build_timeout", fmt.Sprintf("must be between %s and %s", MinBuildTimeout, MaxBuildTimeout)))
	}
	for _, limit := range []struct {
		name  string
		value configkit.ByteSize
		min   configkit.ByteSize
		max   configkit.ByteSize
	}{
		{"max_source_bytes", c.MaxSourceBytes, 1, MaxSourceBytes},
		{"max_stdin_bytes", c.MaxStdinBytes, 1, MaxStdinBytes},
		{"max_stdout_bytes", c.MaxStdoutBytes, 1, MaxOutputBytes},
		{"max_stderr_bytes", c.MaxStderrBytes, 1, MaxOutputBytes},
		{"max_artifact_bytes", c.MaxArtifactBytes, 1, MaxArtifactBytes},
		{"max_memory_bytes", c.MaxMemoryBytes, MinMemoryBytes, MaxMemoryBytes},
		{"max_run_dir_bytes", c.MaxRunDirBytes, 1, MaxRunDirBytes},
	} {
		if limit.value < limit.min || limit.value > limit.max {
			problems = append(problems, configkit.Invalid(limit.name, fmt.Sprintf("must be between %d and %d bytes", limit.min, limit.max)))
		}
	}
	if c.MaxOutputFiles < 1 || c.MaxOutputFiles > MaxOutputFiles {
		problems = append(problems, configkit.Invalid("max_output_files", fmt.Sprintf("must be between 1 and %d", MaxOutputFiles)))
	}
	if c.MaxProcesses < 1 || c.MaxProcesses > MaxProcesses {
		problems = append(problems, configkit.Invalid("max_processes", fmt.Sprintf("must be between 1 and %d", MaxProcesses)))
	}
	if c.MaxArtifactBytes > c.MaxRunDirBytes {
		problems = append(problems, configkit.Invalid("max_artifact_bytes", "must not exceed interpreter.max_run_dir_bytes"))
	}
	return errors.Join(problems...)
}

func (c *Config) Limits() Limits {
	if c == nil {
		return Limits{}
	}
	return Limits{
		Timeout:        time.Duration(c.Timeout),
		BuildTimeout:   time.Duration(c.BuildTimeout),
		MaxSourceBytes: int64(c.MaxSourceBytes),
		MaxStdinBytes:  int64(c.MaxStdinBytes),
		MaxStdoutBytes: int64(c.MaxStdoutBytes),
		MaxStderrBytes: int64(c.MaxStderrBytes),
		MaxMemoryBytes: int64(c.MaxMemoryBytes),
		MaxProcesses:   int64(c.MaxProcesses),
		MaxRunDirBytes: int64(c.MaxRunDirBytes),
		MaxFileBytes:   int64(min(c.MaxArtifactBytes, c.MaxRunDirBytes)),
		MaxOutputFiles: int64(c.MaxOutputFiles),
	}
}
