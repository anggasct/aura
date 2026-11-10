package toolsbuiltin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync/atomic"
	"time"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/modules/codeexec"
	"github.com/anggasct/aura/internal/modules/codeexec/interpreter"
	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/tools"
	"github.com/anggasct/aura/internal/store"
)

type (
	CodeObservation = codeexec.Observation
	CodeObserver    = codeexec.Observer
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

func (e *Executor) Artifacts() store.ArtifactStore {
	return e.artifacts
}

func codeKey() string {
	return tools.CodeExecuteToolName + "@" + tools.Version
}

type CodeOptions struct {
	Store     *interpreter.Store
	Verified  *interpreter.VerifiedSet
	Artifacts store.ArtifactStore
	Observer  CodeObserver
	RunRoot   string
	Limits    *config.Interpreter
}

func NewCodeRunner(options *CodeOptions) (*codeexec.Runner, error) {
	if options == nil || options.Limits == nil || options.Artifacts == nil {
		return nil, errors.New("code runner options, limits and artifact store are required")
	}
	return codeexec.NewRunner(&codeexec.Options{
		Store: options.Store, Verified: options.Verified, Artifacts: codeArtifacts{store: options.Artifacts},
		Observer: options.Observer, RunRoot: options.RunRoot, Limits: codeLimits(options.Limits),
	})
}

type codeArtifacts struct {
	store store.ArtifactStore
}

func (a codeArtifacts) Put(ctx context.Context, content io.Reader, meta *codeexec.ArtifactMeta) (codeexec.ArtifactReceipt, error) {
	ref, err := a.store.Put(ctx, content, &store.ArtifactMetadata{
		ID: meta.ID, SessionID: meta.SessionID, Filename: meta.Filename, MediaType: meta.MediaType, Metadata: meta.Metadata,
	})
	if err != nil {
		if code, ok := store.CodeOf(err); ok && code == store.ErrorCodeArtifactQuotaExceeded {
			return codeexec.ArtifactReceipt{}, codeexec.Errorf(codeexec.ErrorCodeArtifactQuota, "the artifact store quota is exhausted")
		}
		return codeexec.ArtifactReceipt{}, errors.New("the artifact store refused the artifact")
	}
	return codeexec.ArtifactReceipt{ID: ref.ID, Digest: ref.BlobDigest, SizeBytes: ref.SizeBytes}, nil
}

func codeLimits(limits *config.Interpreter) codeexec.Limits {
	return codeexec.Limits{
		Timeout:        time.Duration(limits.Timeout),
		BuildTimeout:   time.Duration(limits.BuildTimeout),
		MaxSourceBytes: int64(limits.MaxSourceBytes),
		MaxStdinBytes:  int64(limits.MaxStdinBytes),
		MaxStdoutBytes: int64(limits.MaxStdoutBytes),
		MaxStderrBytes: int64(limits.MaxStderrBytes),
		MaxMemoryBytes: int64(limits.MaxMemoryBytes),
		MaxProcesses:   int64(limits.MaxProcesses),
		MaxRunDirBytes: int64(limits.MaxRunDirBytes),
		MaxFileBytes:   int64(min(limits.MaxArtifactBytes, limits.MaxRunDirBytes)),
		MaxOutputFiles: int64(limits.MaxOutputFiles),
	}
}
