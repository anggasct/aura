package config

import (
	"errors"
	"fmt"
	"time"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/anggasct/aura/internal/tools"
)

const (
	MinInterpreterTimeout = time.Second
	MaxInterpreterTimeout = 10 * time.Minute

	MaxInterpreterSourceBytes   = ByteSize(tools.MaxCodeSourceBytes)
	MaxInterpreterStdinBytes    = ByteSize(tools.MaxCodeStdinBytes)
	MaxInterpreterOutputBytes   = ByteSize(1 << 20)
	MaxInterpreterOutputFiles   = 256
	MaxInterpreterArtifactBytes = ByteSize(1 << 30)
	MinInterpreterMemoryBytes   = ByteSize(64 << 20)
	MaxInterpreterMemoryBytes   = ByteSize(16 << 30)
	MaxInterpreterProcesses     = 4096
	MaxInterpreterRunDirBytes   = ByteSize(16 << 30)
)

type Interpreter struct {
	Timeout          Duration `koanf:"timeout" yaml:"timeout"`
	MaxSourceBytes   ByteSize `koanf:"max_source_bytes" yaml:"max_source_bytes"`
	MaxStdinBytes    ByteSize `koanf:"max_stdin_bytes" yaml:"max_stdin_bytes"`
	MaxStdoutBytes   ByteSize `koanf:"max_stdout_bytes" yaml:"max_stdout_bytes"`
	MaxStderrBytes   ByteSize `koanf:"max_stderr_bytes" yaml:"max_stderr_bytes"`
	MaxOutputFiles   int      `koanf:"max_output_files" yaml:"max_output_files"`
	MaxArtifactBytes ByteSize `koanf:"max_artifact_bytes" yaml:"max_artifact_bytes"`
	MaxMemoryBytes   ByteSize `koanf:"max_memory_bytes" yaml:"max_memory_bytes"`
	MaxProcesses     int      `koanf:"max_processes" yaml:"max_processes"`
	MaxRunDirBytes   ByteSize `koanf:"max_run_dir_bytes" yaml:"max_run_dir_bytes"`
}

func DefaultInterpreter() Interpreter {
	return Interpreter{
		Timeout:          Duration(30 * time.Second),
		MaxSourceBytes:   ByteSize(256 << 10),
		MaxStdinBytes:    ByteSize(1 << 20),
		MaxStdoutBytes:   ByteSize(64 << 10),
		MaxStderrBytes:   ByteSize(64 << 10),
		MaxOutputFiles:   16,
		MaxArtifactBytes: ByteSize(32 << 20),
		MaxMemoryBytes:   ByteSize(512 << 20),
		MaxProcesses:     64,
		MaxRunDirBytes:   ByteSize(256 << 20),
	}
}

func interpreterAbsent(doc *yamlv3.Node, key string) bool {
	return !configValuePresent(doc, "interpreter", key) && !envValuePresent("interpreter."+key)
}

func applyInterpreterDefaults(cfg *Config, doc *yamlv3.Node) {
	defaults := DefaultInterpreter()
	interpreter := &cfg.Interpreter
	if interpreter.Timeout == 0 && interpreterAbsent(doc, "timeout") {
		interpreter.Timeout = defaults.Timeout
	}
	if interpreter.MaxSourceBytes == 0 && interpreterAbsent(doc, "max_source_bytes") {
		interpreter.MaxSourceBytes = defaults.MaxSourceBytes
	}
	if interpreter.MaxStdinBytes == 0 && interpreterAbsent(doc, "max_stdin_bytes") {
		interpreter.MaxStdinBytes = defaults.MaxStdinBytes
	}
	if interpreter.MaxStdoutBytes == 0 && interpreterAbsent(doc, "max_stdout_bytes") {
		interpreter.MaxStdoutBytes = defaults.MaxStdoutBytes
	}
	if interpreter.MaxStderrBytes == 0 && interpreterAbsent(doc, "max_stderr_bytes") {
		interpreter.MaxStderrBytes = defaults.MaxStderrBytes
	}
	if interpreter.MaxOutputFiles == 0 && interpreterAbsent(doc, "max_output_files") {
		interpreter.MaxOutputFiles = defaults.MaxOutputFiles
	}
	if interpreter.MaxArtifactBytes == 0 && interpreterAbsent(doc, "max_artifact_bytes") {
		interpreter.MaxArtifactBytes = defaults.MaxArtifactBytes
	}
	if interpreter.MaxMemoryBytes == 0 && interpreterAbsent(doc, "max_memory_bytes") {
		interpreter.MaxMemoryBytes = defaults.MaxMemoryBytes
	}
	if interpreter.MaxProcesses == 0 && interpreterAbsent(doc, "max_processes") {
		interpreter.MaxProcesses = defaults.MaxProcesses
	}
	if interpreter.MaxRunDirBytes == 0 && interpreterAbsent(doc, "max_run_dir_bytes") {
		interpreter.MaxRunDirBytes = defaults.MaxRunDirBytes
	}
}

func validateInterpreter(interpreter *Interpreter) error {
	var problems []error
	if time.Duration(interpreter.Timeout) < MinInterpreterTimeout || time.Duration(interpreter.Timeout) > MaxInterpreterTimeout {
		problems = append(problems, fmt.Errorf("interpreter.timeout must be between %s and %s", MinInterpreterTimeout, MaxInterpreterTimeout))
	}
	for _, limit := range []struct {
		name  string
		value ByteSize
		min   ByteSize
		max   ByteSize
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
			problems = append(problems, fmt.Errorf("interpreter.%s must be between %d and %d bytes", limit.name, limit.min, limit.max))
		}
	}
	if interpreter.MaxOutputFiles < 1 || interpreter.MaxOutputFiles > MaxInterpreterOutputFiles {
		problems = append(problems, fmt.Errorf("interpreter.max_output_files must be between 1 and %d", MaxInterpreterOutputFiles))
	}
	if interpreter.MaxProcesses < 1 || interpreter.MaxProcesses > MaxInterpreterProcesses {
		problems = append(problems, fmt.Errorf("interpreter.max_processes must be between 1 and %d", MaxInterpreterProcesses))
	}
	if interpreter.MaxArtifactBytes > interpreter.MaxRunDirBytes {
		problems = append(problems, errors.New("interpreter.max_artifact_bytes must not exceed interpreter.max_run_dir_bytes"))
	}
	if err := errors.Join(problems...); err != nil {
		return &Error{Code: ErrorCodeConfigInvalid, Detail: err.Error()}
	}
	return nil
}

func validateInterpreterShapes(doc *yamlv3.Node) error {
	node := mappingValue(doc, "interpreter")
	if node == nil {
		return nil
	}
	if node.Kind != yamlv3.MappingNode {
		return fmt.Errorf("interpreter must be a mapping at line %d", node.Line)
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		key := node.Content[i].Value
		value := node.Content[i+1]
		switch key {
		case "timeout":
			if value.Kind != yamlv3.ScalarNode || value.Tag != "!!str" {
				return fmt.Errorf("interpreter.timeout must be a duration string at line %d", value.Line)
			}
		case "max_output_files", "max_processes":
			if value.Kind != yamlv3.ScalarNode || value.Tag != "!!int" {
				return fmt.Errorf("interpreter.%s must be an integer at line %d", key, value.Line)
			}
		case "max_source_bytes", "max_stdin_bytes", "max_stdout_bytes", "max_stderr_bytes", "max_artifact_bytes", "max_memory_bytes", "max_run_dir_bytes":
			if value.Kind != yamlv3.ScalarNode || (value.Tag != "!!int" && value.Tag != "!!str") {
				return fmt.Errorf("interpreter.%s must be a byte size at line %d", key, value.Line)
			}
		}
	}
	return nil
}
