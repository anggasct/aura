package config

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestLoad_InterpreterDefaults(t *testing.T) {
	res, err := Load(writeTempConfig(t, "version: 1\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := Interpreter{
		Timeout:          Duration(30 * time.Second),
		MaxSourceBytes:   262144,
		MaxStdinBytes:    1 << 20,
		MaxStdoutBytes:   65536,
		MaxStderrBytes:   65536,
		MaxOutputFiles:   16,
		MaxArtifactBytes: 33554432,
		MaxMemoryBytes:   512 << 20,
		MaxProcesses:     64,
		MaxRunDirBytes:   256 << 20,
	}
	if res.Config.Interpreter != want {
		t.Errorf("Interpreter = %+v, want %+v", res.Config.Interpreter, want)
	}
	if res.Config.Interpreter != DefaultInterpreter() {
		t.Errorf("loaded defaults differ from DefaultInterpreter()")
	}
}

func TestLoad_InterpreterAppliesEveryKey(t *testing.T) {
	content := `version: 1
interpreter:
  timeout: 2m
  max_source_bytes: 1KiB
  max_stdin_bytes: 2048
  max_stdout_bytes: 4KiB
  max_stderr_bytes: 8KiB
  max_output_files: 3
  max_artifact_bytes: 1MiB
  max_memory_bytes: 128MiB
  max_processes: 9
  max_run_dir_bytes: 2MiB
`
	res, err := Load(writeTempConfig(t, content))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := Interpreter{
		Timeout: Duration(2 * time.Minute), MaxSourceBytes: 1 << 10, MaxStdinBytes: 2048, MaxStdoutBytes: 4 << 10,
		MaxStderrBytes: 8 << 10, MaxOutputFiles: 3, MaxArtifactBytes: 1 << 20, MaxMemoryBytes: 128 << 20,
		MaxProcesses: 9, MaxRunDirBytes: 2 << 20,
	}
	if res.Config.Interpreter != want {
		t.Errorf("Interpreter = %+v, want %+v", res.Config.Interpreter, want)
	}
}

func TestLoad_InterpreterBoundsAreRejected(t *testing.T) {
	byteKeys := []struct {
		key   string
		max   ByteSize
		min   ByteSize
		other string
	}{
		{"max_source_bytes", MaxInterpreterSourceBytes, 1, ""},
		{"max_stdin_bytes", MaxInterpreterStdinBytes, 1, ""},
		{"max_stdout_bytes", MaxInterpreterOutputBytes, 1, ""},
		{"max_stderr_bytes", MaxInterpreterOutputBytes, 1, ""},
		{"max_artifact_bytes", MaxInterpreterArtifactBytes, 1, "max_run_dir_bytes: 16GiB"},
		{"max_memory_bytes", MaxInterpreterMemoryBytes, MinInterpreterMemoryBytes, ""},
		{"max_run_dir_bytes", MaxInterpreterRunDirBytes, 1, "max_artifact_bytes: 1"},
	}
	type bound struct {
		name    string
		content string
		valid   bool
	}
	var cases []bound
	for _, k := range byteKeys {
		cases = append(cases,
			bound{k.key + " at the minimum", fmt.Sprintf("%s: %d\n  %s", k.key, k.min, k.other), true},
			bound{k.key + " at the maximum", fmt.Sprintf("%s: %d\n  %s", k.key, k.max, k.other), true},
			bound{k.key + " below the minimum", fmt.Sprintf("%s: %d", k.key, k.min-1), false},
			bound{k.key + " above the maximum", fmt.Sprintf("%s: %d", k.key, k.max+1), false},
			bound{k.key + " explicit zero", k.key + ": 0", false},
			bound{k.key + " negative", k.key + ": -1", false},
		)
	}
	cases = append(cases,
		bound{"timeout at the minimum", "timeout: 1s", true},
		bound{"timeout at the maximum", "timeout: 10m", true},
		bound{"timeout below the minimum", "timeout: 999ms", false},
		bound{"timeout above the maximum", "timeout: 10m1s", false},
		bound{"timeout zero", "timeout: 0s", false},
		bound{"timeout negative", "timeout: -5s", false},
		bound{"max_output_files at the minimum", "max_output_files: 1", true},
		bound{"max_output_files at the maximum", fmt.Sprintf("max_output_files: %d", MaxInterpreterOutputFiles), true},
		bound{"max_output_files zero", "max_output_files: 0", false},
		bound{"max_output_files above the maximum", fmt.Sprintf("max_output_files: %d", MaxInterpreterOutputFiles+1), false},
		bound{"max_processes at the minimum", "max_processes: 1", true},
		bound{"max_processes at the maximum", fmt.Sprintf("max_processes: %d", MaxInterpreterProcesses), true},
		bound{"max_processes zero", "max_processes: 0", false},
		bound{"max_processes negative", "max_processes: -1", false},
		bound{"max_processes above the maximum", fmt.Sprintf("max_processes: %d", MaxInterpreterProcesses+1), false},
		bound{"artifact larger than the run directory", "max_artifact_bytes: 2MiB\n  max_run_dir_bytes: 1MiB", false},
		bound{"artifact equal to the run directory", "max_artifact_bytes: 1MiB\n  max_run_dir_bytes: 1MiB", true},
	)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeTempConfig(t, "version: 1\ninterpreter:\n  "+tc.content+"\n"))
			if tc.valid {
				if err != nil {
					t.Fatalf("Load: %v", err)
				}
				return
			}
			wantCode(t, err, ErrorCodeConfigInvalid)
		})
	}
}

func TestLoad_InterpreterReportsEveryProblem(t *testing.T) {
	_, err := Load(writeTempConfig(t, "version: 1\ninterpreter:\n  timeout: 0s\n  max_processes: 0\n  max_output_files: 0\n"))
	wantCode(t, err, ErrorCodeConfigInvalid)
	for _, key := range []string{"interpreter.timeout", "interpreter.max_processes", "interpreter.max_output_files"} {
		if !strings.Contains(err.Error(), key) {
			t.Errorf("error omits %s: %v", key, err)
		}
	}
}

func TestLoad_InterpreterShapeAndKeysAreStrict(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    string
	}{
		{"quoted integer", "interpreter:\n  max_processes: \"64\"\n", "interpreter.max_processes must be an integer"},
		{"float count", "interpreter:\n  max_output_files: 1.5\n", "interpreter.max_output_files must be an integer"},
		{"numeric duration", "interpreter:\n  timeout: 30\n", "interpreter.timeout must be a duration string"},
		{"boolean byte size", "interpreter:\n  max_memory_bytes: true\n", "interpreter.max_memory_bytes must be a byte size"},
		{"list instead of a mapping", "interpreter: [1]\n", "interpreter must be a mapping"},
		{"unknown key", "interpreter:\n  max_threads: 4\n", `unknown key "interpreter.max_threads"`},
		{"retired profiles key", "profiles:\n  interpreter-python-linux:\n    enabled: true\n", `unknown key "profiles"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeTempConfig(t, "version: 1\n"+tc.content))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestLoad_InterpreterEnvOverride(t *testing.T) {
	t.Setenv("AURA_INTERPRETER_MAX_PROCESSES", "12")
	t.Setenv("AURA_INTERPRETER_MAX_MEMORY_BYTES", "256MiB")
	t.Setenv("AURA_INTERPRETER_TIMEOUT", "45s")
	res, err := Load(writeTempConfig(t, "version: 1\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := res.Config.Interpreter
	if got.MaxProcesses != 12 || got.MaxMemoryBytes != 256<<20 || got.Timeout != Duration(45*time.Second) {
		t.Errorf("Interpreter = %+v", got)
	}
}

func TestLoad_InterpreterEnvOverrideIsValidated(t *testing.T) {
	t.Setenv("AURA_INTERPRETER_MAX_PROCESSES", "0")
	_, err := Load(writeTempConfig(t, "version: 1\n"))
	wantCode(t, err, ErrorCodeConfigInvalid)
}
