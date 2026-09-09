package runtimeengine

import (
	"context"
	"time"

	"github.com/anggasct/aura/internal/runtime"
	"github.com/anggasct/aura/internal/runtime/ingress"
	runtimesessions "github.com/anggasct/aura/internal/runtime/sessions"
	"github.com/anggasct/aura/internal/store"
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
	t.start = func() { e.runTurn(turnCtx, t) }
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
		e.logger.ErrorContext(ctx, "runtime failed to release durable turn", "error", err, "turn_id", t.turnID)
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
