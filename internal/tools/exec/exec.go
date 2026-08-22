package exec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/anggasct/aura/internal/approval"
	"github.com/anggasct/aura/internal/sandbox"
	"github.com/anggasct/aura/internal/toolbroker"
)

type Options struct {
	Workspace      string
	Timeout        time.Duration
	MaxStdoutBytes int64
	MaxStderrBytes int64
	Environment    []string
}

type arguments struct {
	Command []string `json:"command"`
	Shell   bool     `json:"shell"`
}

type result struct {
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	ExitCode   int    `json:"exit_code"`
	Duration   int64  `json:"duration_ms"`
	Terminated bool   `json:"terminated"`
	Truncated  bool   `json:"truncated"`
}

func New(options Options) (toolbroker.Adapter, error) {
	if !filepath.IsAbs(options.Workspace) {
		return nil, errors.New("exec: workspace must be an absolute path")
	}
	if options.Timeout <= 0 {
		return nil, errors.New("exec: timeout must be positive")
	}
	if options.MaxStdoutBytes <= 0 || options.MaxStderrBytes <= 0 {
		return nil, errors.New("exec: output limits must be positive")
	}
	environment := slices.Clone(options.Environment)
	if len(environment) == 0 {
		path := os.Getenv("PATH")
		if path == "" {
			path = "/usr/bin:/bin"
		}
		environment = []string{"PATH=" + path}
	}
	for _, entry := range environment {
		if !validEnvironmentEntry(entry) {
			return nil, errors.New("exec: invalid environment entry")
		}
	}
	return func(ctx context.Context, request *toolbroker.ToolRequest, constraints approval.Constraints) (toolbroker.ToolResult, error) {
		return run(ctx, request, constraints, options, environment)
	}, nil
}

func run(ctx context.Context, request *toolbroker.ToolRequest, constraints approval.Constraints, options Options, environment []string) (toolbroker.ToolResult, error) {
	if request == nil {
		return toolbroker.ToolResult{}, errors.New("exec: request must not be nil")
	}
	if request.ToolName != "exec" || request.ToolVersion != "v1" {
		return toolbroker.ToolResult{}, errors.New("exec: unsupported tool request")
	}
	var args arguments
	if err := json.Unmarshal(request.Arguments, &args); err != nil {
		return toolbroker.ToolResult{}, fmt.Errorf("exec: decode arguments: %w", err)
	}
	if len(args.Command) == 0 || strings.TrimSpace(args.Command[0]) == "" {
		return toolbroker.ToolResult{}, errors.New("exec: command must not be empty")
	}
	if remoteCommandLine(args.Command) {
		return toolbroker.ToolResult{}, errors.New("exec: remote command execution is not allowed")
	}
	command := args.Command[0]
	commandArgs := slices.Clone(args.Command[1:])
	if args.Shell {
		command = "/bin/sh"
		commandArgs = []string{"-c", strings.Join(args.Command, " ")}
	}

	timeout := options.Timeout
	if constraints.Timeout > 0 && constraints.Timeout < timeout {
		timeout = constraints.Timeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if !request.Deadline.IsZero() {
		deadlineCtx, deadlineCancel := context.WithDeadline(runCtx, request.Deadline)
		defer deadlineCancel()
		runCtx = deadlineCtx
	}
	maxOutput := options.MaxStdoutBytes
	if options.MaxStderrBytes > maxOutput {
		maxOutput = options.MaxStderrBytes
	}
	spec := &sandbox.Spec{
		WorkingDir:     options.Workspace,
		ReadWritePaths: []string{options.Workspace},
		AllowEnv:       environment,
		Limits: sandbox.Limits{
			Timeout:        timeout,
			MaxOutputBytes: maxOutput,
		},
	}
	started := time.Now()
	sandboxResult, err := sandbox.Run(runCtx, spec, command, commandArgs...)
	if err != nil {
		return toolbroker.ToolResult{}, err
	}
	output, err := json.Marshal(result{
		Stdout:     capOutput(sandboxResult.Stdout, options.MaxStdoutBytes),
		Stderr:     capOutput(sandboxResult.Stderr, options.MaxStderrBytes),
		ExitCode:   sandboxResult.ExitCode,
		Duration:   time.Since(started).Milliseconds(),
		Terminated: sandboxResult.Terminated,
		Truncated:  sandboxResult.Truncated,
	})
	if err != nil {
		return toolbroker.ToolResult{}, fmt.Errorf("exec: encode result: %w", err)
	}
	class := toolbroker.ResultOK
	if sandboxResult.Terminated {
		class = toolbroker.ResultDeadlineExceeded
	} else if sandboxResult.ExitCode != 0 {
		class = toolbroker.ResultExecutionFailed
	}
	return toolbroker.ToolResult{Class: class, Output: output}, nil
}

func capOutput(value string, limit int64) string {
	if limit <= 0 || int64(len(value)) <= limit {
		return value
	}
	return value[:int(limit)]
}

func remoteCommand(command string) bool {
	base := filepath.Base(command)
	switch base {
	case "ssh", "scp", "sftp", "rsh", "telnet", "nc", "netcat":
		return true
	default:
		return false
	}
}

func remoteCommandLine(command []string) bool {
	for _, token := range strings.Fields(strings.Join(command, " ")) {
		if remoteCommand(token) {
			return true
		}
	}
	return false
}

func validEnvironmentEntry(entry string) bool {
	key, _, ok := strings.Cut(entry, "=")
	if !ok || key == "" {
		return false
	}
	for i, r := range key {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r == '_' || (i > 0 && r >= '0' && r <= '9') {
			continue
		}
		return false
	}
	return true
}
