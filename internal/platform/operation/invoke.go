package operation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/sqlite"
	"github.com/anggasct/aura/internal/platform/telemetry"
)

const (
	outcomeOK        = "ok"
	outcomePreviewed = "previewed"
	outcomeRefused   = "refused"
	outcomeFailed    = "failed"
	unknownLabel     = "unknown"
)

type invocation struct {
	registry  *Registry
	entry     *entry
	commits   atomic.Int32
	committed atomic.Bool
	first     atomic.Pointer[Result]
	misused   atomic.Bool
	paths     map[string]string
	filesMu   sync.Mutex
	opened    []File
}

type runInfo struct {
	notPerformed bool
	code         string
}

func refusal(err error) runInfo {
	if interrupted(err) {
		return runInfo{}
	}
	code, _ := fault.CodeOf[ErrorCode](err)
	return runInfo{notPerformed: true, code: string(code)}
}

func failure(found *entry, committed bool, cause, wrapped error) runInfo {
	if committed || interrupted(cause) {
		return runInfo{}
	}
	if code, ok := definitiveRefusal(cause); ok {
		return runInfo{notPerformed: true, code: code}
	}
	if found.descriptor.Commit == CommitTransactional {
		return refusal(wrapped)
	}
	return runInfo{}
}

func agentSurface(call *Call) bool {
	return call != nil && call.Caller.Surface == SurfaceAgent
}

func (r *Registry) Invoke(ctx context.Context, id string, call *Call) (Result, error) {
	found, ok := r.entries[id]
	if !ok {
		r.observe(ctx, &observation{id: unknownLabel, outcome: outcomeRefused, started: time.Now()})
		return Result{}, NewError(ErrorCodeUnknown, "operation: no such operation")
	}
	if agentSurface(call) {
		return Result{}, r.refuseAgentSurface(ctx, found, call)
	}
	result, _, err := r.run(ctx, found, call)
	return result, err
}

func (r *Registry) refuseAgentSurface(ctx context.Context, found *entry, call *Call) error {
	r.observe(ctx, &observation{id: found.descriptor.ID, surface: call.Caller.Surface, risk: found.descriptor.Risk, outcome: outcomeRefused, started: time.Now()})
	return NewError(ErrorCodeNotExposed, "operation: the agent surface runs only through the tool broker")
}

func (r *Registry) Preview(ctx context.Context, id string, call *Call) (Preview, error) {
	found, ok := r.entries[id]
	if !ok {
		r.observe(ctx, &observation{id: unknownLabel, outcome: outcomeRefused, started: time.Now()})
		return Preview{}, NewError(ErrorCodeUnknown, "operation: no such operation")
	}
	if agentSurface(call) {
		return Preview{}, r.refuseAgentSurface(ctx, found, call)
	}
	return r.preview(ctx, found, call)
}

type observation struct {
	misused bool
	id      string
	surface Surface
	risk    Risk
	outcome string
	started time.Time
}

func (r *Registry) observe(ctx context.Context, seen *observation) {
	elapsed := time.Since(seen.started)
	r.recorder.Record(ctx, &telemetry.OperationObservation{
		ID: seen.id, Surface: string(seen.surface), Risk: string(seen.risk), Outcome: seen.outcome, Duration: elapsed,
	})
	level := slog.LevelInfo
	attrs := []any{
		"component", "operation",
		"operation", seen.id,
		"surface", string(seen.surface),
		"risk", string(seen.risk),
		"status", seen.outcome,
		"latency_ms", elapsed.Milliseconds(),
	}
	if seen.misused {
		level = slog.LevelError
		attrs = append(attrs, "reason", "handler_broke_commit_contract")
	}
	r.logger.Log(ctx, level, "operation invoked", attrs...)
}

func (r *Registry) admit(found *entry, call *Call) (*Call, error) {
	if call == nil {
		return nil, nilArgument("call")
	}
	descriptor := found.descriptor
	surface := call.Caller.Surface
	if !surface.valid() {
		return nil, NewError(ErrorCodeInvalidArgument, "operation: the call has no known surface")
	}
	if !descriptor.Projects(surface) {
		return nil, NewError(ErrorCodeNotExposed, "operation: the surface is not a projection of the operation")
	}
	if surface != SurfaceCLI && call.Caller.Class != principal.ClassOwner {
		return nil, NewError(ErrorCodeDenied, "operation: the caller is not the owner")
	}
	if surface == SurfaceChat && !call.Caller.Direct {
		return nil, NewError(ErrorCodeDenied, "operation: chat operations run only in a direct chat")
	}
	canonical, err := descriptor.Validate(surface, call.Input)
	if err != nil {
		return nil, err
	}
	canonical, paths, err := descriptor.extractPaths(surface, canonical)
	if err != nil {
		return nil, err
	}
	prepared := *call
	prepared.Input = canonical
	files := r.filesFor(&prepared.Caller)
	if err := files.refuseUnresolvedPaths(paths); err != nil {
		return nil, err
	}
	prepared.Files = files
	prepared.run = &invocation{registry: r, entry: found, paths: paths}
	return &prepared, nil
}

func (r *Registry) preview(ctx context.Context, found *entry, call *Call) (preview Preview, err error) {
	seen := &observation{id: found.descriptor.ID, risk: found.descriptor.Risk, outcome: outcomeFailed, started: time.Now()}
	if call != nil {
		seen.surface = call.Caller.Surface
	}
	defer func() {
		if err == nil {
			seen.outcome = outcomePreviewed
		}
		r.observe(ctx, seen)
	}()
	prepared, err := r.admit(found, call)
	if err != nil {
		seen.outcome = outcomeRefused
		return Preview{}, err
	}
	defer prepared.run.closeFiles()
	if found.binding.Preview == nil {
		seen.outcome = outcomeRefused
		return Preview{}, NewError(ErrorCodeNotExposed, "operation: the operation has no preview")
	}
	preview, err = found.binding.Preview(ctx, prepared)
	if err != nil {
		return Preview{}, asOperationError(err)
	}
	preview, err = r.confirmable(found.descriptor, prepared, preview)
	if err != nil {
		return Preview{}, asOperationError(err)
	}
	return preview, nil
}

func (r *Registry) run(ctx context.Context, found *entry, call *Call) (result Result, info runInfo, err error) {
	seen := &observation{id: found.descriptor.ID, risk: found.descriptor.Risk, outcome: outcomeFailed, started: time.Now()}
	if call != nil {
		seen.surface = call.Caller.Surface
	}
	var prepared *Call
	defer func() {
		if err == nil {
			seen.outcome = outcomeOK
		}
		if prepared != nil && prepared.run.misused.Load() {
			seen.outcome = outcomeFailed
			seen.misused = true
		}
		r.observe(ctx, seen)
	}()
	prepared, err = r.admit(found, call)
	if err != nil {
		seen.outcome = outcomeRefused
		return Result{}, refusal(err), err
	}
	defer prepared.run.closeFiles()
	if err := r.confirmed(found.descriptor, prepared); err != nil {
		seen.outcome = outcomeRefused
		return Result{}, refusal(err), err
	}
	if limit := found.binding.RateLimit; limit != nil && prepared.Caller.Surface == SurfaceAgent && !resumedRun(ctx) {
		if err := limit(ctx, prepared); err != nil {
			seen.outcome = outcomeRefused
			limited := asOperationError(err)
			return Result{}, refusal(limited), limited
		}
	}
	handled, err := found.binding.Handle(ctx, prepared)
	return r.finish(found, prepared, handled, err)
}

func (r *Registry) finish(found *entry, prepared *Call, handled Result, handlerErr error) (Result, runInfo, error) {
	transactional := found.descriptor.Commit == CommitTransactional
	committed := prepared.run.committed.Load()
	if transactional && committed && prepared.run.brokeCommitContract(handlerErr) {
		prepared.run.misused.Store(true)
		return *prepared.run.first.Load(), runInfo{}, nil
	}
	if handlerErr != nil {
		wrapped := asOperationError(handlerErr)
		return Result{}, failure(found, committed, handlerErr, wrapped), wrapped
	}
	if transactional && prepared.run.commits.Load() != 1 {
		return Result{}, runInfo{notPerformed: !committed, code: string(ErrorCodeUncommitted)}, NewError(ErrorCodeUncommitted, "operation: the handler did not commit exactly once")
	}
	data, err := normalizeResult(handled.Data)
	if err != nil {
		return Result{}, runInfo{notPerformed: transactional && !committed, code: string(ErrorCodeFailed)}, err
	}
	return Result{Data: data}, runInfo{}, nil
}

func asOperationError(err error) error {
	if _, coded := fault.CodeOf[ErrorCode](err); coded {
		return err
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return wrapError(ErrorCodeFailed, "", err)
}

func normalizeResult(data json.RawMessage) (json.RawMessage, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return json.RawMessage("{}"), nil
	}
	if trimmed[0] != '{' || !json.Valid(trimmed) {
		return nil, NewError(ErrorCodeFailed, "the result is not a JSON object")
	}
	return trimmed, nil
}

func (c *Call) Commit(ctx context.Context, apply func(ctx context.Context) (Result, error)) (Result, error) {
	if c == nil || apply == nil || c.run == nil {
		return Result{}, nilArgument("call or commit function")
	}
	return c.run.commit(ctx, c, apply)
}

func (n *invocation) commit(ctx context.Context, call *Call, apply func(ctx context.Context) (Result, error)) (Result, error) {
	descriptor := n.entry.descriptor
	if descriptor.Commit != CommitTransactional {
		return Result{}, NewError(ErrorCodeInvalid, "operation: only a transactional operation commits")
	}
	if n.commits.Add(1) != 1 {
		return Result{}, NewError(ErrorCodeUncommitted, "operation: the handler committed more than once")
	}
	surface := call.Caller.Surface
	if surface == SurfaceCLI || call.IdempotencyKey == "" {
		result, err := n.applyOnly(ctx, descriptor, apply)
		if err == nil {
			n.recordFirst(&result)
		}
		return result, err
	}
	result, err := n.applyWithReceipt(ctx, call, apply)
	if err == nil {
		n.recordFirst(&result)
	}
	return result, err
}

func (n *invocation) brokeCommitContract(handlerErr error) bool {
	if handlerErr != nil {
		return !interrupted(handlerErr)
	}
	return n.commits.Load() > 1
}

func (n *invocation) recordFirst(result *Result) {
	recorded := *result
	n.first.Store(&recorded)
	n.committed.Store(true)
}

func (n *invocation) applyOnly(ctx context.Context, descriptor *Descriptor, apply func(ctx context.Context) (Result, error)) (Result, error) {
	var result Result
	err := sqlite.RunInTx(ctx, n.registry.receipts.db, func(ctx context.Context) error {
		applied, err := apply(ctx)
		if err != nil {
			return err
		}
		data, err := normalizeResult(applied.Data)
		if err != nil {
			return err
		}
		if _, err := receiptData(descriptor, data); err != nil {
			return err
		}
		result = Result{Data: data}
		return nil
	})
	if err != nil {
		return Result{}, err
	}
	return result, nil
}

func (n *invocation) applyWithReceipt(ctx context.Context, call *Call, apply func(ctx context.Context) (Result, error)) (Result, error) {
	registry := n.registry
	descriptor := n.entry.descriptor
	if call.RequestDigest == "" {
		return Result{}, NewError(ErrorCodeInvalidArgument, "operation: a receipt needs the request digest")
	}
	var result Result
	err := sqlite.RunInTx(ctx, registry.receipts.db, func(ctx context.Context) error {
		existing, found, err := registry.receipts.lookup(ctx, call.IdempotencyKey)
		if err != nil {
			return err
		}
		if found {
			recorded, replayErr := recordedResult(&existing, call.RequestDigest)
			result = recorded
			return replayErr
		}
		applied, err := apply(ctx)
		if err != nil {
			return err
		}
		data, err := normalizeResult(applied.Data)
		if err != nil {
			return err
		}
		stored, err := receiptData(descriptor, data)
		if err != nil {
			return err
		}
		insertErr := registry.receipts.insert(ctx, &receipt{
			key: call.IdempotencyKey, operation: descriptor.ID, digest: call.RequestDigest,
			surface: call.Caller.Surface, result: stored, createdAt: registry.clock.Now(),
		})
		if insertErr != nil {
			return insertErr
		}
		result = Result{Data: data}
		return nil
	})
	if errors.Is(err, errReceiptExists) {
		return registry.winner(ctx, call)
	}
	if err != nil {
		return Result{}, err
	}
	return result, nil
}

func recordedResult(found *receipt, digest string) (Result, error) {
	if found.digest != digest {
		return Result{}, NewError(ErrorCodeKeyReused, "operation: the idempotency key belongs to another request")
	}
	return Result{Data: found.result}, nil
}

func (r *Registry) winner(ctx context.Context, call *Call) (Result, error) {
	found, ok, err := r.receipts.lookup(ctx, call.IdempotencyKey)
	if err != nil {
		return Result{}, err
	}
	if !ok {
		return Result{}, NewError(ErrorCodeFailed, "operation: the receipt that won the key is missing")
	}
	return recordedResult(&found, call.RequestDigest)
}

func (r *Registry) confirmed(descriptor *Descriptor, call *Call) error {
	if !descriptor.confirmsWithCode() {
		return nil
	}
	if call.Caller.Surface == SurfaceAgent {
		return NewError(ErrorCodeConfirmation, "an operation that confirms with a code cannot be applied from the agent surface")
	}
	if call.PreviewDigest == "" || call.Confirmation == "" || !r.confirmer.Matches(call.PreviewDigest, call.Confirmation) {
		return NewError(ErrorCodeConfirmation, "the code does not match the current preview")
	}
	return nil
}
