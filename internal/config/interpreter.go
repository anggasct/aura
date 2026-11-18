package config

import (
	"errors"
	"fmt"
	"time"

	"github.com/anggasct/aura/internal/platform/configkit"
	"github.com/anggasct/aura/internal/platform/tools"
)

const (
	MinInterpreterTimeout = time.Second
	MaxInterpreterTimeout = 10 * time.Minute

	MinInterpreterBuildTimeout = 5 * time.Second
	MaxInterpreterBuildTimeout = 10 * time.Minute

	MaxInterpreterSourceBytes   = configkit.ByteSize(tools.MaxCodeSourceBytes)
	MaxInterpreterStdinBytes    = configkit.ByteSize(tools.MaxCodeStdinBytes)
	MaxInterpreterOutputBytes   = configkit.ByteSize(1 << 20)
	MaxInterpreterOutputFiles   = tools.MaxCodeOutputFiles
	MaxInterpreterArtifactBytes = configkit.ByteSize(1 << 30)
	MinInterpreterMemoryBytes   = configkit.ByteSize(64 << 20)
	MaxInterpreterMemoryBytes   = configkit.ByteSize(16 << 30)
	MaxInterpreterProcesses     = 4096
	MaxInterpreterRunDirBytes   = configkit.ByteSize(16 << 30)
)

type Interpreter struct {
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

func DefaultInterpreter() Interpreter {
	return Interpreter{
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

func validateInterpreter(interpreter *Interpreter) error {
	var problems []error
	if time.Duration(interpreter.Timeout) < MinInterpreterTimeout || time.Duration(interpreter.Timeout) > MaxInterpreterTimeout {
		problems = append(problems, configkit.Invalid("interpreter.timeout", fmt.Sprintf("must be between %s and %s", MinInterpreterTimeout, MaxInterpreterTimeout)))
	}
	if time.Duration(interpreter.BuildTimeout) < MinInterpreterBuildTimeout || time.Duration(interpreter.BuildTimeout) > MaxInterpreterBuildTimeout {
		problems = append(problems, configkit.Invalid("interpreter.build_timeout", fmt.Sprintf("must be between %s and %s", MinInterpreterBuildTimeout, MaxInterpreterBuildTimeout)))
	}
	for _, limit := range []struct {
		name  string
		value configkit.ByteSize
		min   configkit.ByteSize
		max   configkit.ByteSize
	}{
		{"max_source_bytes", interpreter.MaxSourceBytes, 1, MaxInterpreterSourceBytes},
		{"max_stdin_bytes", interpreter.MaxStdinBytes, 1, MaxInterpreterStdinBytes},
		{"max_stdout_bytes", interpreter.MaxStdoutBytes, 1, MaxInterpreterOutputBytes},
		{"max_stderr_bytes", interpreter.MaxStderrBytes, 1, MaxInterpreterOutputBytes},
		{"max_artifact_bytes", interpreter.MaxArtifactBytes, 1, MaxInterpreterArtifactBytes},
		{"max_memory_bytes", interpreter.MaxMemoryBytes, MinInterpreterMemoryBytes, MaxInterpreterMemoryBytes},
		{"max_run_dir_bytes", interpreter.MaxRunDirBytes, 1, MaxInterpreterRunDirBytes},
	} {
		if limit.value < limit.min || limit.value > limit.max {
			problems = append(problems, configkit.Invalid("interpreter."+limit.name, fmt.Sprintf("must be between %d and %d bytes", limit.min, limit.max)))
		}
	}
	if interpreter.MaxOutputFiles < 1 || interpreter.MaxOutputFiles > MaxInterpreterOutputFiles {
		problems = append(problems, configkit.Invalid("interpreter.max_output_files", fmt.Sprintf("must be between 1 and %d", MaxInterpreterOutputFiles)))
	}
	if interpreter.MaxProcesses < 1 || interpreter.MaxProcesses > MaxInterpreterProcesses {
		problems = append(problems, configkit.Invalid("interpreter.max_processes", fmt.Sprintf("must be between 1 and %d", MaxInterpreterProcesses)))
	}
	if interpreter.MaxArtifactBytes > interpreter.MaxRunDirBytes {
		problems = append(problems, configkit.Invalid("interpreter.max_artifact_bytes", "must not exceed interpreter.max_run_dir_bytes"))
	}
	return errors.Join(problems...)
}
