package toolsbuiltin

import (
	"testing"
	"time"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/tools/codeexec"
)

func TestCodeLimitsReadEveryConfiguredBound(t *testing.T) {
	configured := config.Interpreter{
		Timeout:          config.Duration(7 * time.Second),
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
	want := codeexec.Limits{
		Timeout: 7 * time.Second, MaxSourceBytes: 11, MaxStdinBytes: 12, MaxStdoutBytes: 13, MaxStderrBytes: 14,
		MaxMemoryBytes: 17, MaxProcesses: 18, MaxRunDirBytes: 19, MaxFileBytes: 16,
	}
	if got := codeLimits(&configured); got != want {
		t.Fatalf("codeLimits = %+v, want %+v", got, want)
	}
}

func TestCodeLimitsKeepAFileInsideTheRunDirectory(t *testing.T) {
	configured := config.DefaultInterpreter()
	configured.MaxArtifactBytes = configured.MaxRunDirBytes * 2
	if got := codeLimits(&configured); got.MaxFileBytes != int64(configured.MaxRunDirBytes) {
		t.Fatalf("file limit = %d, want it capped at the run directory limit %d", got.MaxFileBytes, configured.MaxRunDirBytes)
	}
}

func TestNewCodeRunnerRequiresItsOptions(t *testing.T) {
	limits := config.DefaultInterpreter()
	cases := []struct {
		name    string
		options *CodeOptions
	}{
		{"nil options", nil},
		{"missing limits", &CodeOptions{RunRoot: "/run"}},
		{"missing store", &CodeOptions{RunRoot: "/run", Limits: &limits}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner, err := NewCodeRunner(tc.options)
			if err == nil || runner != nil {
				t.Fatalf("NewCodeRunner = %v, %v; want an error and no runner", runner, err)
			}
		})
	}
}
