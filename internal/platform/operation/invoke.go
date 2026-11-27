package operation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
}

type runInfo struct {
	notPerformed bool
}

func (r *Registry) Invoke(ctx context.Context, id string, call *Call) (Result, error) {
	found, ok := r.entries[id]
	if !ok {
		r.observe(ctx, &observation{id: unknownLabel, outcome: outcomeRefused, started: time.Now()})
		return Result{}, newError(ErrorCodeUnknown, "operation: no such operation")
	}
	result, _, err := r.run(ctx, found, call)
	return result, err
}

func (r *Registry) Preview(ctx context.Context, id string, call *Call) (Preview, error) {
	found, ok := r.entries[id]
	if !ok {
		r.observe(ctx, &observation{id: unknownLabel, outcome: outcomeRefused, started: time.Now()})
		return Preview{}, newError(ErrorCodeUnknown, "operation: no such operation")
	}
	return r.preview(ctx, found, call)
}

type observation struct {
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
	r.logger.InfoContext(ctx, "operation invoked",
		"component", "operation",
		"operation", seen.id,
		"surface", string(seen.surface),
		"risk", string(seen.risk),
		"status", seen.outcome,
		"latency_ms", elapsed.Milliseconds(),
	)
}

func (r *Registry) admit(found *entry, call *Call) (*Call, error) {
	if call == nil {
		return nil, nilArgument("call")
	}
	descriptor := found.descriptor
	surface := call.Caller.Surface
	if !surface.valid() {
		return nil, newError(ErrorCodeInvalidArgument, "operation: the call has no known surface")
	}
	if !descriptor.Projects(surface) {
		return nil, newError(ErrorCodeNotExposed, "operation: the surface is not a projection of the operation")
	}
	if surface != SurfaceCLI && call.Caller.Class != principal.ClassOwner {
		return nil, newError(ErrorCodeDenied, "operation: the caller is not the owner")
	}
	canonical, err := descriptor.Validate(surface, call.Input)
	if err != nil {
		return nil, err
	}
	prepared := *call
	prepared.Input = canonical
	prepared.run = &invocation{registry: r, entry: found}
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
	if found.binding.Preview == nil {
		seen.outcome = outcomeRefused
		return Preview{}, newError(ErrorCodeNotExposed, "operation: the operation has no preview")
	}
	preview, err = found.binding.Preview(ctx, prepared)
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
	defer func() {
		if err == nil {
			seen.outcome = outcomeOK
		}
		r.observe(ctx, seen)
	}()
	prepared, err := r.admit(found, call)
	if err != nil {
		seen.outcome = outcomeRefused
		return Result{}, runInfo{notPerformed: true}, err
	}
	if limit := found.binding.RateLimit; limit != nil {
		if err := limit(ctx, prepared); err != nil {
			seen.outcome = outcomeRefused
			return Result{}, runInfo{notPerformed: true}, asOperationError(err)
		}
	}
	handled, err := found.binding.Handle(ctx, prepared)
	return r.finish(found, prepared, handled, err)
}

func (r *Registry) finish(found *entry, prepared *Call, handled Result, handlerErr error) (Result, runInfo, error) {
	transactional := found.descriptor.Commit == CommitTransactional
	committed := prepared.run.committed.Load()
	if handlerErr != nil {
		return Result{}, runInfo{notPerformed: transactional && !committed}, asOperationError(handlerErr)
	}
	if transactional && prepared.run.commits.Load() != 1 {
		return Result{}, runInfo{notPerformed: true}, newError(ErrorCodeUncommitted, "operation: the handler did not commit exactly once")
	}
	data, err := normalizeResult(handled.Data)
	if err != nil {
		return Result{}, runInfo{notPerformed: transactional && !committed}, err
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
		return nil, newError(ErrorCodeFailed, "the result is not a JSON object")
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
		return Result{}, newError(ErrorCodeInvalid, "operation: only a transactional operation commits")
	}
	if n.commits.Add(1) != 1 {
		return Result{}, newError(ErrorCodeUncommitted, "operation: the handler committed more than once")
	}
	surface := call.Caller.Surface
	if surface == SurfaceCLI || call.IdempotencyKey == "" {
		result, err := n.applyOnly(ctx, descriptor, apply)
		if err == nil {
			n.committed.Store(true)
		}
		return result, err
	}
	result, err := n.applyWithReceipt(ctx, call, apply)
	if err == nil {
		n.committed.Store(true)
	}
	return result, err
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
		return Result{}, newError(ErrorCodeInvalidArgument, "operation: a receipt needs the request digest")
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
		return Result{}, newError(ErrorCodeKeyReused, "operation: the idempotency key belongs to another request")
	}
	return Result{Data: found.result}, nil
}

func (r *Registry) winner(ctx context.Context, call *Call) (Result, error) {
	found, ok, err := r.receipts.lookup(ctx, call.IdempotencyKey)
	if err != nil {
		return Result{}, err
	}
	if !ok {
		return Result{}, newError(ErrorCodeFailed, "operation: the receipt that won the key is missing")
	}
	return recordedResult(&found, call.RequestDigest)
}
