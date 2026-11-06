package codeexec

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"

	"github.com/anggasct/aura/internal/approval"
	"github.com/anggasct/aura/internal/sandbox"
	"github.com/anggasct/aura/internal/toolbroker"
	"github.com/anggasct/aura/internal/tools"
)

const (
	toolVersion      = "v1"
	defaultMaxResult = 64 * 1024
)

type arguments struct {
	Runtime string `json:"runtime"`
	Source  string `json:"source"`
	Stdin   string `json:"stdin"`
}

func (r *Runner) Adapter() toolbroker.Adapter {
	return r.adapt
}

func (r *Runner) ArgumentCheck(raw json.RawMessage) error {
	args, err := decodeArguments(raw)
	if err != nil {
		return err
	}
	if int64(len(args.Source)) > r.limits.MaxSourceBytes {
		return Errorf(ErrorCodeInvalidArgument, "argument %q must be at most %d bytes", "source", r.limits.MaxSourceBytes)
	}
	if int64(len(args.Stdin)) > r.limits.MaxStdinBytes {
		return Errorf(ErrorCodeInvalidArgument, "argument %q must be at most %d bytes", "stdin", r.limits.MaxStdinBytes)
	}
	return nil
}

func decodeArguments(raw json.RawMessage) (arguments, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var args arguments
	if err := decoder.Decode(&args); err != nil {
		return arguments{}, Errorf(ErrorCodeInvalidArgument, "arguments are not valid")
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return arguments{}, Errorf(ErrorCodeInvalidArgument, "arguments have trailing content")
	}
	return args, nil
}

func (r *Runner) adapt(ctx context.Context, request *toolbroker.ToolRequest, constraints approval.Constraints) (toolbroker.ToolResult, error) {
	if request == nil {
		return toolbroker.ToolResult{}, toolbroker.Errorf(toolbroker.ResultInvalidArgument, "request must not be nil")
	}
	if request.ToolName != tools.CodeExecuteToolName || request.ToolVersion != toolVersion {
		return toolbroker.ToolResult{}, toolbroker.Errorf(toolbroker.ResultInvalidArgument, "unsupported tool request")
	}
	args, err := decodeArguments(request.Arguments)
	if err == nil {
		err = r.ArgumentCheck(request.Arguments)
	}
	if err != nil {
		return toolbroker.ToolResult{}, toolbroker.NotPerformed(string(ErrorCodeInvalidArgument), toolbroker.Errorf(toolbroker.ResultInvalidArgument, "%v", err))
	}
	runCtx := ctx
	if !request.Deadline.IsZero() {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithDeadline(ctx, request.Deadline)
		defer cancel()
	}
	outcome, err := r.Execute(runCtx, &Request{Runtime: args.Runtime, Source: args.Source, Stdin: args.Stdin, Timeout: constraints.Timeout})
	if err != nil {
		return toolbroker.ToolResult{}, notPerformed(runCtx, err)
	}
	maxBytes := constraints.MaxOutputBytes
	if maxBytes <= 0 {
		maxBytes = defaultMaxResult
	}
	output, err := encodeBounded(&outcome, &r.limits, maxBytes)
	if err != nil {
		return toolbroker.ToolResult{}, toolbroker.Errorf(toolbroker.ResultExecutionFailed, "code_execute result could not be encoded")
	}
	return toolbroker.ToolResult{Class: outcome.Class.toolClass(), Output: output}, nil
}

func notPerformed(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return toolbroker.NotPerformed("cancelled", ctxErr)
	}
	code, _ := CodeOf(err)
	if sandboxCode, ok := sandbox.CodeOf(err); ok && sandboxCode == sandbox.ErrorCodeSandboxUnavailable {
		code = ErrorCodeUnavailable
	}
	switch code {
	case ErrorCodeUnavailable:
		return toolbroker.NotPerformed(string(code), toolbroker.Errorf(toolbroker.ResultCapabilityUnavailable, "the requested interpreter runtime is not available"))
	case ErrorCodeInvalidArgument:
		return toolbroker.NotPerformed(string(code), toolbroker.Errorf(toolbroker.ResultInvalidArgument, "%v", err))
	default:
		return toolbroker.NotPerformed(string(ErrorCodeRunFailed), toolbroker.Errorf(toolbroker.ResultExecutionFailed, "the interpreter run could not start"))
	}
}
