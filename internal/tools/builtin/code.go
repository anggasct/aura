package toolsbuiltin

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"time"

	"github.com/anggasct/aura/internal/approval"
	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/toolbroker"
	"github.com/anggasct/aura/internal/tools"
	"github.com/anggasct/aura/internal/tools/codeexec"
	"github.com/anggasct/aura/internal/tools/interpreter"
)

type CodeRunner interface {
	Adapter() toolbroker.Adapter
	ArgumentCheck(arguments json.RawMessage) error
}

type codeHolder struct {
	runner atomic.Pointer[CodeRunner]
}

func (h *codeHolder) adapter(ctx context.Context, request *toolbroker.ToolRequest, constraints approval.Constraints) (toolbroker.ToolResult, error) {
	runner := h.runner.Load()
	if runner == nil {
		return toolbroker.ToolResult{}, toolbroker.NotPerformed("capability_unavailable", toolbroker.Errorf(toolbroker.ResultCapabilityUnavailable, "code execution is not available"))
	}
	return (*runner).Adapter()(ctx, request, constraints)
}

func (h *codeHolder) check(arguments json.RawMessage) error {
	runner := h.runner.Load()
	if runner == nil {
		return nil
	}
	return (*runner).ArgumentCheck(arguments)
}

func (e *Executor) SetCodeRunner(runner CodeRunner) {
	if e == nil || e.code == nil || runner == nil {
		return
	}
	e.code.runner.Store(&runner)
}

func codeKey() string {
	return tools.CodeExecuteToolName + "@" + tools.Version
}

type CodeOptions struct {
	Store    *interpreter.Store
	Verified *interpreter.VerifiedSet
	RunRoot  string
	Limits   *config.Interpreter
}

func NewCodeRunner(options *CodeOptions) (*codeexec.Runner, error) {
	if options == nil || options.Limits == nil {
		return nil, errors.New("code runner options and limits are required")
	}
	return codeexec.NewRunner(&codeexec.Options{
		Store: options.Store, Verified: options.Verified, RunRoot: options.RunRoot, Limits: codeLimits(options.Limits),
	})
}

func codeLimits(limits *config.Interpreter) codeexec.Limits {
	return codeexec.Limits{
		Timeout:        time.Duration(limits.Timeout),
		MaxSourceBytes: int64(limits.MaxSourceBytes),
		MaxStdinBytes:  int64(limits.MaxStdinBytes),
		MaxStdoutBytes: int64(limits.MaxStdoutBytes),
		MaxStderrBytes: int64(limits.MaxStderrBytes),
		MaxMemoryBytes: int64(limits.MaxMemoryBytes),
		MaxProcesses:   int64(limits.MaxProcesses),
		MaxRunDirBytes: int64(limits.MaxRunDirBytes),
		MaxFileBytes:   int64(min(limits.MaxArtifactBytes, limits.MaxRunDirBytes)),
	}
}
