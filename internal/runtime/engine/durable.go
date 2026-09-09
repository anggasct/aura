package runtimeengine

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/anggasct/aura/internal/durable"
	"github.com/anggasct/aura/internal/runtime"
	"github.com/anggasct/aura/internal/runtime/ingress"
	runtimesessions "github.com/anggasct/aura/internal/runtime/sessions"
	"github.com/anggasct/aura/internal/store"
	"github.com/anggasct/aura/internal/toolbroker"
)

type SessionStore interface {
	Admit(ctx context.Context, req *runtimesessions.AdmitRequest) (runtimesessions.AdmitResult, error)
	Release(ctx context.Context, sessionID, turnID string) (runtimesessions.ReleaseResult, error)
	Recover(ctx context.Context, sessionID string, open, terminal []string) (runtimesessions.RecoverResult, error)
	Abort(ctx context.Context, sessionID string) (runtimesessions.AbortResult, error)
}

func turnDescriptorFromRequest(req *runtime.TurnRequest) runtimesessions.Descriptor {
	parts := make([]runtimesessions.Part, 0, len(req.Parts))
	for _, part := range req.Parts {
		parts = append(parts, runtimesessions.Part{Text: part.Text})
	}
	return runtimesessions.Descriptor{
		TurnID:         req.TurnID,
		SessionID:      req.SessionID,
		PrincipalID:    req.PrincipalID,
		Origin:         string(req.Origin),
		Parts:          parts,
		IdempotencyKey: req.IdempotencyKey,
		Deadline:       req.Deadline,
		MaxTokens:      req.Budget.MaxTokens,
		MaxCost:        req.Budget.MaxCost,
		TraceParent:    req.TraceParent,
		AgentID:        req.AgentID,
	}
}

func turnRequestFromDescriptor(desc *runtimesessions.Descriptor) *runtime.TurnRequest {
	parts := make([]runtimeingress.InputPart, 0, len(desc.Parts))
	for _, part := range desc.Parts {
		parts = append(parts, runtimeingress.InputPart{Text: part.Text})
	}
	return &runtime.TurnRequest{
		TurnID:         desc.TurnID,
		SessionID:      desc.SessionID,
		PrincipalID:    desc.PrincipalID,
		Origin:         runtime.Origin(desc.Origin),
		Parts:          parts,
		IdempotencyKey: desc.IdempotencyKey,
		Deadline:       desc.Deadline,
		Budget:         runtime.Budget{MaxTokens: desc.MaxTokens, MaxCost: desc.MaxCost},
		TraceParent:    desc.TraceParent,
		AgentID:        desc.AgentID,
	}
}

func (e *Engine) MarkRecovered() {
	e.recoverOnce.Do(func() { close(e.recovered) })
}

func (e *Engine) serveTurn(ctx context.Context, inv durable.Invocation) error {
	var desc runtimesessions.Descriptor
	if err := json.Unmarshal(inv.Payload(), &desc); err != nil {
		return invalidArgument("durable turn payload is not a descriptor")
	}
	return e.DriveTurn(ctx, inv, &desc)
}

func (e *Engine) claimDurable(ctx context.Context, req *runtime.TurnRequest) (accepted store.RuntimeEvent, originalTurnID string, replay, granted bool, err error) {
	if e.sessionStore == nil {
		return store.RuntimeEvent{}, "", false, false, invalidArgument("durable session store is not configured")
	}
	select {
	case <-e.recovered:
	default:
		select {
		case <-e.recovered:
		case <-ctx.Done():
			e.releasePending()
			return store.RuntimeEvent{}, "", false, false, codedError(runtime.ErrorCodeRuntimeOverloaded, "durable recovery pending", ctx.Err())
		}
	}
	descriptor := turnDescriptorFromRequest(req)
	eventID := NewTurnID()
	result, err := e.sessionStore.Admit(ctx, &runtimesessions.AdmitRequest{
		Turn:            descriptor,
		AcceptedEventID: eventID,
		MaxPending:      e.cfg.MaxPendingTurns,
	})
	if err != nil {
		e.releasePending()
		return store.RuntimeEvent{}, "", false, false, codedError(runtime.ErrorCodeStorageUnavailable, "durable admit failed", err)
	}
	if result.Overloaded {
		e.releasePending()
		return store.RuntimeEvent{}, "", false, false, codedError(runtime.ErrorCodeRuntimeOverloaded, "pending turn queue is full", nil)
	}
	if result.Replayed {
		return store.RuntimeEvent{}, result.OriginalTurnID, true, false, nil
	}
	if result.ToStart != nil && result.ToStart.Descriptor.TurnID != req.TurnID {
		e.releasePending()
		return store.RuntimeEvent{}, "", false, false, invalidArgument("durable admit granted another turn")
	}
	createdAt, err := time.Parse(time.RFC3339Nano, result.EventCreatedAt)
	if err != nil {
		e.releasePending()
		return store.RuntimeEvent{}, "", false, false, invalidArgument("durable admit returned an invalid accepted timestamp")
	}
	accepted = store.RuntimeEvent{
		ID:            eventID,
		SessionID:     req.SessionID,
		Sequence:      result.Sequence,
		TurnID:        req.TurnID,
		Author:        req.PrincipalID,
		Kind:          runtime.EventKindTurnAccepted,
		SchemaVersion: 1,
		Payload:       []byte(result.EventPayload),
		CreatedAt:     createdAt,
	}
	return accepted, "", false, result.ToStart != nil, nil
}

func (e *Engine) stageDurable(ctx context.Context, req *runtime.TurnRequest, accepted *store.RuntimeEvent, sub *subscriber, granted bool) {
	subs := map[*subscriber]struct{}{}
	if sub != nil {
		subs[sub] = struct{}{}
	}
	turnCtx, cancel := context.WithCancel(ctx)
	t := &turn{
		req:      *req,
		turnID:   req.TurnID,
		accepted: *accepted,
		subs:     subs,
		cancel:   cancel,
		done:     make(chan struct{}),
	}
	if e.durableRuntime != nil {
		t.start = func() { e.pollDurableTurn(turnCtx, t) }
	} else {
		t.start = func() { e.runTurn(turnCtx, t) }
	}
	e.mu.Lock()
	if e.shutdown {
		e.mu.Unlock()
		e.terminateStaged(context.WithoutCancel(ctx), t)
		return
	}
	e.turns[req.TurnID] = t
	e.staged[req.TurnID] = t
	if e.sesKeys == nil {
		e.sesKeys = map[string]struct{}{}
	}
	e.sesKeys[req.SessionID] = struct{}{}
	if granted {
		if e.granted == nil {
			e.granted = map[string]struct{}{}
		}
		e.granted[req.TurnID] = struct{}{}
	}
	e.mu.Unlock()
	e.scheduleDurable(ctx)
}

func (e *Engine) tryStartStagedLocked(ctx context.Context, t *turn) {
	if e.shutdown {
		e.mu.Unlock()
		e.terminateStaged(ctx, t)
		e.mu.Lock()
		return
	}
	if e.active >= e.cfg.MaxActiveTurns {
		if e.granted == nil {
			e.granted = map[string]struct{}{}
		}
		e.granted[t.turnID] = struct{}{}
		return
	}
	delete(e.staged, t.turnID)
	delete(e.granted, t.turnID)
	e.active++
	e.pending--
	e.wg.Add(1)
	go t.start()
}

func (e *Engine) scheduleDurable(ctx context.Context) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for {
		if e.active >= e.cfg.MaxActiveTurns || e.shutdown {
			return
		}
		var next *turn
		for turnID := range e.granted {
			if t, ok := e.staged[turnID]; ok {
				next = t
				break
			}
			delete(e.granted, turnID)
		}
		if next == nil {
			return
		}
		e.tryStartStagedLocked(ctx, next)
	}
}

func (e *Engine) releaseTurn(ctx context.Context, t *turn) {
	ctx = context.WithoutCancel(ctx)
	result, err := e.sessionStore.Release(ctx, t.req.SessionID, t.turnID)
	if err != nil {
		e.mu.Lock()
		shutdown := e.shutdown
		e.mu.Unlock()
		if shutdown {
			e.logger.DebugContext(ctx, "runtime failed to release durable turn during shutdown", "error", err, "turn_id", t.turnID)
		} else {
			e.logger.ErrorContext(ctx, "runtime failed to release durable turn", "error", err, "turn_id", t.turnID)
		}
		return
	}
	if result.ToStart != nil {
		e.mu.Lock()
		shutdown := e.shutdown
		staged, ok := e.staged[result.ToStart.Descriptor.TurnID]
		if ok {
			if staged.req.SessionID != t.req.SessionID {
				e.mu.Unlock()
				e.logger.ErrorContext(ctx, "durable release returned a turn from another session", "turn_id", result.ToStart.Descriptor.TurnID)
				return
			}
			if shutdown {
				e.mu.Unlock()
				e.terminateStaged(ctx, staged)
				return
			}
			if e.granted == nil {
				e.granted = map[string]struct{}{}
			}
			e.granted[staged.turnID] = struct{}{}
			e.mu.Unlock()
			e.scheduleDurable(ctx)
			return
		}
		e.mu.Unlock()
		if shutdown {
			return
		}
		e.startReleasedTurn(ctx, result.ToStart)
		return
	}
	if result.QueueDepth == 0 {
		e.mu.Lock()
		delete(e.sesKeys, t.req.SessionID)
		e.mu.Unlock()
	}
}

func (e *Engine) startReleasedTurn(ctx context.Context, started *runtimesessions.QueuedTurn) {
	if started.Descriptor.TurnID == "" {
		e.logger.ErrorContext(ctx, "durable release returned an empty turn")
		return
	}
	req := turnRequestFromDescriptor(&started.Descriptor)
	accepted, err := e.acceptedForReleased(ctx, req)
	if err != nil {
		e.logger.ErrorContext(ctx, "runtime failed to load released turn events", "error", err, "turn_id", req.TurnID)
		return
	}
	e.stageDurable(ctx, req, accepted, nil, true)
}

func (e *Engine) acceptedForReleased(ctx context.Context, req *runtime.TurnRequest) (*store.RuntimeEvent, error) {
	events, err := e.dedupe.ListTurnEvents(ctx, req.TurnID)
	if err != nil {
		return nil, err
	}
	for i := range events {
		if events[i].Kind == runtime.EventKindTurnAccepted {
			accepted := events[i]
			return &accepted, nil
		}
	}
	return nil, invalidArgument("released turn has no accepted event")
}

func (e *Engine) RecoverSession(ctx context.Context, sessionID string, open, terminal []string) error {
	if e.sessionStore == nil {
		return invalidArgument("durable session store is not configured")
	}
	if sessionID == "" {
		return invalidArgument("recover requires a session id")
	}
	result, err := e.sessionStore.Recover(ctx, sessionID, open, terminal)
	if err != nil {
		return codedError(runtime.ErrorCodeStorageUnavailable, "durable recover failed", err)
	}
	if !result.Found || result.ToStart == nil {
		return nil
	}
	req := turnRequestFromDescriptor(&result.ToStart.Descriptor)
	accepted, err := e.acceptedForReleased(ctx, req)
	if err != nil {
		return codedError(runtime.ErrorCodeStorageUnavailable, "durable recover failed", err)
	}
	e.mu.Lock()
	shutdown := e.shutdown
	if !shutdown {
		e.pending++
	}
	e.mu.Unlock()
	e.stageDurable(ctx, req, accepted, nil, true)
	return nil
}

func (e *Engine) terminateStaged(ctx context.Context, t *turn) {
	terminal := &store.RuntimeEvent{
		ID:            NewTurnID(),
		SessionID:     t.req.SessionID,
		TurnID:        t.turnID,
		Author:        t.req.PrincipalID,
		Kind:          runtime.EventKindTurnCancelled,
		SchemaVersion: 1,
		Payload:       cancelledPayload("runtime shutdown", e.agentID(&t.req)),
		CreatedAt:     time.Now().UTC(),
	}
	if _, err := e.events.AppendSequenced(ctx, t.req.SessionID, terminal); err != nil {
		e.logger.ErrorContext(ctx, "runtime failed to persist staged terminal event", "error", err, "turn_id", t.turnID)
	}
	e.broadcast(t, &t.accepted)
	e.broadcast(t, terminal)
	for sub := range t.subs {
		sub.closeEvents()
	}
	e.mu.Lock()
	delete(e.turns, t.turnID)
	delete(e.staged, t.turnID)
	delete(e.granted, t.turnID)
	select {
	case <-t.done:
	default:
		close(t.done)
	}
	e.mu.Unlock()
}

func (e *Engine) fillTurnEvent(t *turn, ev *store.RuntimeEvent) {
	if ev.ID == "" {
		ev.ID = NewTurnID()
	}
	ev.SessionID = t.req.SessionID
	ev.TurnID = t.turnID
	if ev.Author == "" {
		ev.Author = t.req.PrincipalID
	}
	if ev.SchemaVersion == 0 {
		ev.SchemaVersion = 1
	}
	if ev.CreatedAt.IsZero() {
		ev.CreatedAt = time.Now().UTC()
	}
}

func (e *Engine) terminalOutcome(ctx context.Context, execErr error, req *runtime.TurnRequest) (kind string, payload []byte) {
	switch {
	case ctx.Err() == context.Canceled:
		return runtime.EventKindTurnCancelled, cancelledPayload("turn cancelled", e.agentID(req))
	case ctx.Err() == context.DeadlineExceeded:
		return runtime.EventKindTurnFailed, failedPayload(runtime.ErrorCodeTurnDeadlineExceeded, "turn deadline elapsed", nil, e.agentID(req))
	case execErr != nil:
		code, ok := runtime.CodeOf(execErr)
		if !ok {
			code = runtime.ErrorCodeRuntimeInternal
		}
		return runtime.EventKindTurnFailed, failedPayload(code, "turn execution failed", execErr, e.agentID(req))
	default:
		return runtime.EventKindTurnCompleted, completedPayload(e.agentID(req))
	}
}

func (e *Engine) finishTurn(ctx context.Context, t *turn) {
	e.mu.Lock()
	e.active--
	close(t.done)
	delete(e.turns, t.turnID)
	e.mu.Unlock()
	e.schedule(ctx)
}

const durableStatusPoll = 500 * time.Millisecond

func (e *Engine) DriveTurn(ctx context.Context, inv durable.Invocation, desc *runtimesessions.Descriptor) error {
	if inv == nil {
		return invalidArgument("durable turn drive requires an invocation")
	}
	if desc == nil {
		return invalidArgument("durable turn drive requires a descriptor")
	}
	if err := desc.Validate(); err != nil {
		return err
	}
	scope := durable.NewTurnScope(inv)
	if scope == nil {
		return invalidArgument("durable turn drive requires an invocation")
	}
	ctx = durable.WithTurnScope(ctx, scope)
	ctx = toolbroker.WithApprovalSink(ctx, toolbroker.NewApprovalEventSink(e.events, e.Publish))
	req := turnRequestFromDescriptor(desc)
	accepted, err := e.acceptedForReleased(ctx, req)
	if err != nil {
		return err
	}
	t := &turn{req: *req, turnID: req.TurnID, accepted: *accepted}
	deadline := req.Deadline
	if deadline.IsZero() {
		deadline = accepted.CreatedAt.Add(e.cfg.TurnTimeout)
	}
	ctx, stop := context.WithDeadline(ctx, deadline)
	defer stop()

	persist := func(ctx context.Context, ev *store.RuntimeEvent) error {
		if ev.ID == "" {
			ev.ID = scope.NextEventID(req.TurnID)
		}
		e.fillTurnEvent(t, ev)
		sequence, _, err := e.events.UpsertEvent(ctx, ev)
		if err != nil {
			return codedError(runtime.ErrorCodeStorageUnavailable, "failed to persist "+ev.Kind, err)
		}
		ev.Sequence = sequence
		e.broadcast(t, ev)
		return nil
	}
	emit := func(ctx context.Context, kind string, payload []byte) error {
		ev := &store.RuntimeEvent{Kind: kind, Payload: payload}
		return persist(ctx, ev)
	}

	var execErr error
	for ev, err := range e.executor.Execute(ctx, req) {
		if err != nil {
			execErr = err
			break
		}
		if emitErr := persist(ctx, &ev); emitErr != nil {
			execErr = emitErr
			break
		}
	}
	if ctx.Err() == context.Canceled {
		return ctx.Err()
	}
	terminalKind, terminalPayload := e.terminalOutcome(ctx, execErr, req)
	if err := emit(context.WithoutCancel(ctx), terminalKind, terminalPayload); err != nil {
		return err
	}
	return nil
}

func (e *Engine) pollDurableTurn(ctx context.Context, t *turn) {
	defer e.finishTurn(ctx, t)
	e.broadcast(t, &t.accepted)
	desc := turnDescriptorFromRequest(&t.req)
	payload, err := json.Marshal(desc)
	if err != nil {
		e.failDurableStart(ctx, t, err)
		return
	}
	ref, err := e.durableRuntime.Start(ctx, durable.StartRequest{Handler: "turn", Key: t.turnID, Payload: payload})
	if err != nil {
		e.failDurableStart(ctx, t, err)
		return
	}
	ticker := time.NewTicker(durableStatusPoll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			e.cancelDurableTurn(ctx, t, ref)
			return
		case <-ticker.C:
			status, err := e.durableRuntime.Status(ctx, ref)
			if err != nil {
				if errors.Is(err, durable.ErrUnknownRun) {
					e.failDurableStart(ctx, t, err)
					return
				}
				e.logger.DebugContext(ctx, "durable turn status probe failed", "error", err, "turn_id", t.turnID)
				continue
			}
			switch status.State {
			case durable.RunSucceeded, durable.RunFailed, durable.RunCancelled:
				e.releaseTurn(ctx, t)
				for sub := range t.subs {
					sub.closeEvents()
				}
				return
			case durable.RunRunning, durable.RunSuspended:
				continue
			}
		}
	}
}

func (e *Engine) cancelDurableTurn(ctx context.Context, t *turn, ref durable.RunRef) {
	if err := e.durableRuntime.Cancel(context.WithoutCancel(ctx), ref); err != nil {
		e.logger.DebugContext(ctx, "durable turn cancel failed", "error", err, "turn_id", t.turnID)
	}
	if e.hasTerminalEvent(ctx, t) {
		e.releaseTurn(ctx, t)
		return
	}
	cancelled := &store.RuntimeEvent{
		ID:            t.turnID + "-cancel",
		SessionID:     t.req.SessionID,
		TurnID:        t.turnID,
		Author:        t.req.PrincipalID,
		Kind:          runtime.EventKindTurnCancelled,
		SchemaVersion: 1,
		Payload:       cancelledPayload("turn cancelled", e.agentID(&t.req)),
		CreatedAt:     time.Now().UTC(),
	}
	if _, _, err := e.events.UpsertEvent(context.WithoutCancel(ctx), cancelled); err != nil {
		e.logger.ErrorContext(ctx, "runtime failed to persist turn cancellation", "error", err, "turn_id", t.turnID)
	}
	e.broadcast(t, &t.accepted)
	e.broadcast(t, cancelled)
	for sub := range t.subs {
		sub.closeEvents()
	}
	e.releaseTurn(ctx, t)
}

func (e *Engine) hasTerminalEvent(ctx context.Context, t *turn) bool {
	events, err := e.dedupe.ListTurnEvents(ctx, t.turnID)
	if err != nil {
		return false
	}
	for i := range events {
		if isTerminalKind(events[i].Kind) {
			return true
		}
	}
	return false
}

func (e *Engine) failDurableStart(ctx context.Context, t *turn, cause error) {
	terminal := &store.RuntimeEvent{
		ID:            t.turnID + "-terminal",
		SessionID:     t.req.SessionID,
		TurnID:        t.turnID,
		Author:        t.req.PrincipalID,
		Kind:          runtime.EventKindTurnFailed,
		SchemaVersion: 1,
		Payload:       failedPayload(runtime.ErrorCodeStorageUnavailable, "durable turn did not start", cause, e.agentID(&t.req)),
		CreatedAt:     time.Now().UTC(),
	}
	if _, _, err := e.events.UpsertEvent(context.WithoutCancel(ctx), terminal); err != nil {
		e.logger.ErrorContext(ctx, "runtime failed to persist turn start failure", "error", err, "turn_id", t.turnID)
	}
	e.broadcast(t, &t.accepted)
	e.broadcast(t, terminal)
	for sub := range t.subs {
		sub.closeEvents()
	}
	e.releaseTurn(ctx, t)
}
