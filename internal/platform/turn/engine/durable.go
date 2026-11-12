package runtimeengine

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/turn"
	"github.com/anggasct/aura/internal/platform/turn/ingress"
	runtimesessions "github.com/anggasct/aura/internal/platform/turn/sessions"
	"github.com/anggasct/aura/internal/store"
)

type SessionStore interface {
	Admit(ctx context.Context, req *runtimesessions.AdmitRequest) (runtimesessions.AdmitResult, error)
	Release(ctx context.Context, sessionID, turnID string) (runtimesessions.ReleaseResult, error)
	Status(ctx context.Context, sessionID string) (runtimesessions.StatusResult, error)
	Abort(ctx context.Context, sessionID string) (runtimesessions.AbortResult, error)
	ActiveTurns(ctx context.Context) ([]runtimesessions.QueuedTurn, error)
}

func turnDescriptorFromRequest(req *turn.TurnRequest) runtimesessions.Descriptor {
	parts := make([]runtimesessions.Part, 0, len(req.Parts))
	for _, part := range req.Parts {
		parts = append(parts, runtimesessions.Part{Text: part.Text, Image: part.Image})
	}
	return runtimesessions.Descriptor{
		TurnID:           req.TurnID,
		SessionID:        req.SessionID,
		PrincipalID:      req.PrincipalID,
		PrincipalClass:   string(req.PrincipalClass),
		Origin:           string(req.Origin),
		Parts:            parts,
		ReplyContext:     req.ReplyContext,
		UntrustedContext: untrustedContextFromRecall(req.UntrustedContext),
		RequireRecall:    req.RequireRecall,
		IdempotencyKey:   req.IdempotencyKey,
		Deadline:         req.Deadline,
		MaxTokens:        req.Budget.MaxTokens,
		MaxCost:          req.Budget.MaxCost,
		TraceParent:      req.TraceParent,
		AgentID:          req.AgentID,
		Persona:          req.Persona,
		Skill:            req.Skill,
		StreamPartial:    req.StreamPartial,
		Knowledge:        slices.Clone(req.Knowledge),
	}
}

func turnRequestFromDescriptor(desc *runtimesessions.Descriptor) *turn.TurnRequest {
	parts := make([]runtimeingress.InputPart, 0, len(desc.Parts))
	for _, part := range desc.Parts {
		parts = append(parts, runtimeingress.InputPart{Text: part.Text, Image: part.Image})
	}
	return &turn.TurnRequest{
		TurnID:           desc.TurnID,
		SessionID:        desc.SessionID,
		PrincipalID:      desc.PrincipalID,
		PrincipalClass:   principal.ParseClass(desc.PrincipalClass),
		Origin:           turn.Origin(desc.Origin),
		Parts:            parts,
		ReplyContext:     desc.ReplyContext,
		UntrustedContext: untrustedRecallFromContext(desc.UntrustedContext),
		RequireRecall:    desc.RequireRecall,
		IdempotencyKey:   desc.IdempotencyKey,
		Deadline:         desc.Deadline,
		Budget:           turn.Budget{MaxTokens: desc.MaxTokens, MaxCost: desc.MaxCost},
		TraceParent:      desc.TraceParent,
		AgentID:          desc.AgentID,
		Persona:          desc.Persona,
		Skill:            desc.Skill,
		StreamPartial:    desc.StreamPartial,
		Knowledge:        slices.Clone(desc.Knowledge),
	}
}

func untrustedContextFromRecall(recall *turn.UntrustedRecall) *runtimesessions.UntrustedContext {
	if recall == nil {
		return nil
	}
	var documents []runtimesessions.UntrustedDocument
	for i := range recall.Documents {
		document := &recall.Documents[i]
		documents = append(documents, runtimesessions.UntrustedDocument{
			ID:           document.ID,
			SessionID:    document.SessionID,
			FromSequence: document.FromSequence,
			ToSequence:   document.ToSequence,
			Trust:        string(document.Trust),
			Content:      document.Content,
		})
	}
	return &runtimesessions.UntrustedContext{
		Query:     recall.Query,
		Summary:   recall.Summary,
		Trust:     string(recall.Trust),
		Documents: documents,
	}
}

func untrustedRecallFromContext(stored *runtimesessions.UntrustedContext) *turn.UntrustedRecall {
	if stored == nil {
		return nil
	}
	var documents []turn.UntrustedDocument
	for i := range stored.Documents {
		document := &stored.Documents[i]
		documents = append(documents, turn.UntrustedDocument{
			ID:           document.ID,
			SessionID:    document.SessionID,
			FromSequence: document.FromSequence,
			ToSequence:   document.ToSequence,
			Trust:        approval.TrustLabel(document.Trust),
			Content:      document.Content,
		})
	}
	return &turn.UntrustedRecall{
		Query:     stored.Query,
		Summary:   stored.Summary,
		Trust:     approval.TrustLabel(stored.Trust),
		Documents: documents,
	}
}

func (e *Engine) serveTurn(ctx context.Context, inv durable.Invocation) error {
	var desc runtimesessions.Descriptor
	if err := json.Unmarshal(inv.Payload(), &desc); err != nil {
		return invalidArgument("durable turn payload is not a descriptor")
	}
	return e.DriveTurn(ctx, inv, &desc)
}

func (e *Engine) claimDurable(ctx context.Context, req *turn.TurnRequest) (accepted store.RuntimeEvent, originalTurnID string, replay, granted bool, err error) {
	if e.sessionStore == nil {
		return store.RuntimeEvent{}, "", false, false, invalidArgument("durable session store is not configured")
	}
	descriptor := turnDescriptorFromRequest(req)
	eventID := NewTurnID()
	e.mu.Lock()
	if e.admitting == nil {
		e.admitting = map[string]string{}
	}
	e.admitting[req.TurnID] = req.SessionID
	e.mu.Unlock()
	staging := false
	defer func() {
		if !staging {
			e.mu.Lock()
			delete(e.admitting, req.TurnID)
			delete(e.granted, req.TurnID)
			e.mu.Unlock()
		}
	}()
	result, err := e.sessionStore.Admit(ctx, &runtimesessions.AdmitRequest{
		Turn:            descriptor,
		AcceptedEventID: eventID,
		MaxPending:      e.cfg.MaxPendingTurns,
	})
	if err != nil {
		e.releasePending()
		return store.RuntimeEvent{}, "", false, false, codedError(turn.ErrorCodeStorageUnavailable, "durable admit failed", err)
	}
	if result.Overloaded {
		e.releasePending()
		return store.RuntimeEvent{}, "", false, false, codedError(turn.ErrorCodeRuntimeOverloaded, "pending turn queue is full", nil)
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
		Kind:          turn.EventKindTurnAccepted,
		SchemaVersion: 1,
		Payload:       []byte(result.EventPayload),
		CreatedAt:     createdAt,
	}
	staging = true
	return accepted, "", false, result.ToStart != nil, nil
}

type durableStaging struct {
	req      *turn.TurnRequest
	accepted *store.RuntimeEvent
	sub      *subscriber
	granted  bool
	adopted  bool
}

func (e *Engine) stageDurable(ctx context.Context, staging *durableStaging) {
	req := staging.req
	subs := map[*subscriber]struct{}{}
	if staging.sub != nil {
		subs[staging.sub] = struct{}{}
	}
	turnCtx, cancel := context.WithCancel(ctx)
	t := &turnState{
		req:          *req,
		turnID:       req.TurnID,
		accepted:     *staging.accepted,
		subs:         subs,
		cancel:       cancel,
		done:         make(chan struct{}),
		holdsPending: !staging.adopted,
	}
	if e.durableRuntime != nil {
		t.start = func() { e.pollDurableTurn(turnCtx, t) }
	} else {
		t.start = func() { e.runTurn(turnCtx, t) }
	}
	e.mu.Lock()
	if staging.adopted {
		if e.shutdown || e.knownLocked(req.TurnID) {
			e.mu.Unlock()
			cancel()
			return
		}
	} else {
		delete(e.admitting, req.TurnID)
		if e.shutdown {
			delete(e.granted, req.TurnID)
			e.mu.Unlock()
			e.terminateAdmittedDuringShutdown(context.WithoutCancel(ctx), t)
			return
		}
	}
	e.turns[req.TurnID] = t
	e.staged[req.TurnID] = t
	if e.sesKeys == nil {
		e.sesKeys = map[string]struct{}{}
	}
	e.sesKeys[req.SessionID] = struct{}{}
	if staging.granted {
		if e.granted == nil {
			e.granted = map[string]struct{}{}
		}
		e.granted[req.TurnID] = struct{}{}
	}
	e.mu.Unlock()
	e.scheduleDurable(ctx)
}

func (e *Engine) terminateAdmittedDuringShutdown(ctx context.Context, t *turnState) {
	e.terminateStaged(ctx, t)
	if _, err := e.sessionStore.Abort(ctx, t.req.SessionID); err != nil {
		e.logger.ErrorContext(ctx, "runtime failed to abort durable session", "error", err, "turn_id", t.turnID)
	}
}

func (e *Engine) knownLocked(turnID string) bool {
	if _, ok := e.turns[turnID]; ok {
		return true
	}
	_, ok := e.admitting[turnID]
	return ok
}

func (e *Engine) tryStartStagedLocked(ctx context.Context, t *turnState) {
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
	t.running = true
	e.active++
	if t.holdsPending {
		t.holdsPending = false
		e.pending--
	}
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
		var next *turnState
		for turnID := range e.granted {
			if t, ok := e.staged[turnID]; ok {
				next = t
				break
			}
			if _, admitting := e.admitting[turnID]; !admitting {
				delete(e.granted, turnID)
			}
		}
		if next == nil {
			return
		}
		e.tryStartStagedLocked(ctx, next)
	}
}

func (e *Engine) RecoverTurns(ctx context.Context) error {
	if e.sessionStore == nil {
		return nil
	}
	e.mu.Lock()
	shutdown := e.shutdown
	e.mu.Unlock()
	if shutdown {
		return nil
	}
	active, err := e.sessionStore.ActiveTurns(ctx)
	if err != nil {
		return codedError(turn.ErrorCodeStorageUnavailable, "failed to list active durable turns", err)
	}
	var problems []error
	for i := range active {
		listed := &active[i]
		if err := e.actOnActiveTurn(ctx, listed.Descriptor.SessionID, listed); err != nil {
			problems = append(problems, fmt.Errorf("recover durable turn %q: %w", listed.Descriptor.TurnID, err))
		}
	}
	return errors.Join(problems...)
}

func (e *Engine) releaseTurn(ctx context.Context, t *turnState) {
	ctx = context.WithoutCancel(ctx)
	if _, err := e.sessionStore.Release(ctx, t.req.SessionID, t.turnID); err != nil {
		e.mu.Lock()
		shutdown := e.shutdown
		e.mu.Unlock()
		if shutdown {
			e.logger.DebugContext(ctx, "runtime failed to release durable turn during shutdown", "error", err, "turn_id", t.turnID)
		} else {
			e.logger.ErrorContext(ctx, "runtime failed to release durable turn", "error", err, "turn_id", t.turnID)
		}
	}
	e.advanceSession(ctx, t.req.SessionID)
}

func (e *Engine) settleSucceededTurn(ctx context.Context, t *turnState) {
	ctx = context.WithoutCancel(ctx)
	status, err := e.sessionStore.Status(ctx, t.req.SessionID)
	if err != nil {
		e.logger.ErrorContext(ctx, "runtime failed to read durable session status", "error", err, "turn_id", t.turnID)
		return
	}
	if status.Active != nil && status.Active.Descriptor.TurnID == t.turnID {
		e.releaseTurn(ctx, t)
		return
	}
	e.applySessionStatus(ctx, t.req.SessionID, &status)
}

func (e *Engine) advanceSession(ctx context.Context, sessionID string) {
	status, err := e.sessionStore.Status(ctx, sessionID)
	if err != nil {
		e.logger.ErrorContext(ctx, "runtime failed to read durable session status", "error", err, "session_id", sessionID)
		return
	}
	e.applySessionStatus(ctx, sessionID, &status)
}

func (e *Engine) applySessionStatus(ctx context.Context, sessionID string, status *runtimesessions.StatusResult) {
	if status.Active == nil || status.Active.Descriptor.TurnID == "" {
		if status.QueueDepth == 0 {
			e.mu.Lock()
			delete(e.sesKeys, sessionID)
			e.mu.Unlock()
		}
		return
	}
	active := runtimesessions.QueuedTurn{Descriptor: status.Active.Descriptor, Sequence: status.Active.Sequence}
	if err := e.actOnActiveTurn(ctx, sessionID, &active); err != nil {
		e.logger.ErrorContext(ctx, "runtime failed to act on the active durable turn", "error", err, "turn_id", active.Descriptor.TurnID)
	}
}

func (e *Engine) actOnActiveTurn(ctx context.Context, sessionID string, active *runtimesessions.QueuedTurn) error {
	turnID := active.Descriptor.TurnID
	if turnID == "" {
		return invalidArgument("durable session holds an active turn without an id")
	}
	if active.Descriptor.SessionID != sessionID {
		e.logger.ErrorContext(ctx, "durable session returned a turn from another session", "turn_id", turnID)
		return nil
	}
	e.mu.Lock()
	if e.shutdown {
		e.mu.Unlock()
		return nil
	}
	if t, ok := e.turns[turnID]; ok {
		if t.req.SessionID != sessionID {
			e.mu.Unlock()
			e.logger.ErrorContext(ctx, "durable session returned a turn from another session", "turn_id", turnID)
			return nil
		}
		_, staged := e.staged[turnID]
		if staged {
			e.granted[turnID] = struct{}{}
		}
		e.mu.Unlock()
		if staged {
			e.scheduleDurable(ctx)
		}
		return nil
	}
	if admittingSession, admitting := e.admitting[turnID]; admitting {
		if admittingSession == sessionID {
			e.granted[turnID] = struct{}{}
		}
		e.mu.Unlock()
		if admittingSession != sessionID {
			e.logger.ErrorContext(ctx, "durable session returned a turn from another session", "turn_id", turnID)
		}
		return nil
	}
	e.mu.Unlock()
	return e.adoptDurable(context.WithoutCancel(ctx), active)
}

func (e *Engine) adoptDurable(ctx context.Context, active *runtimesessions.QueuedTurn) error {
	req := turnRequestFromDescriptor(&active.Descriptor)
	runExists := false
	if e.durableRuntime != nil {
		_, err := e.durableRuntime.Status(ctx, durable.RunRef{Key: req.TurnID})
		switch {
		case err == nil:
			runExists = true
		case errors.Is(err, durable.ErrUnknownRun):
		default:
			return codedError(turn.ErrorCodeStorageUnavailable, "failed to read the durable turn run", err)
		}
	}
	events, err := e.turnEvents(ctx, req.TurnID)
	if err != nil {
		return err
	}
	accepted, err := acceptedEventOf(events)
	if err != nil {
		return err
	}
	if runExists {
		e.attachDurable(ctx, req, accepted)
		return nil
	}
	e.stageDurable(ctx, &durableStaging{req: req, accepted: accepted, granted: true, adopted: true})
	return nil
}

func (e *Engine) attachDurable(ctx context.Context, req *turn.TurnRequest, accepted *store.RuntimeEvent) {
	turnCtx, cancel := context.WithCancel(ctx)
	t := &turnState{
		req:      *req,
		turnID:   req.TurnID,
		accepted: *accepted,
		subs:     map[*subscriber]struct{}{},
		cancel:   cancel,
		done:     make(chan struct{}),
		attached: true,
	}
	t.start = func() { e.pollDurableTurn(turnCtx, t) }
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.shutdown || e.knownLocked(req.TurnID) {
		cancel()
		return
	}
	e.turns[req.TurnID] = t
	if e.sesKeys == nil {
		e.sesKeys = map[string]struct{}{}
	}
	e.sesKeys[req.SessionID] = struct{}{}
	t.running = true
	e.active++
	e.wg.Add(1)
	go t.start()
}

func (e *Engine) turnEvents(ctx context.Context, turnID string) ([]store.RuntimeEvent, error) {
	events, err := e.dedupe.ListTurnEvents(ctx, turnID)
	if err != nil {
		return nil, codedError(turn.ErrorCodeStorageUnavailable, "failed to read the durable turn events", err)
	}
	return events, nil
}

func acceptedEventOf(events []store.RuntimeEvent) (*store.RuntimeEvent, error) {
	for i := range events {
		if events[i].Kind == turn.EventKindTurnAccepted {
			accepted := events[i]
			return &accepted, nil
		}
	}
	return nil, invalidArgument("durable turn has no accepted event")
}

func hasTerminalKind(events []store.RuntimeEvent) bool {
	for i := range events {
		if isTerminalKind(events[i].Kind) {
			return true
		}
	}
	return false
}

func (e *Engine) terminateStaged(ctx context.Context, t *turnState) {
	terminal := &store.RuntimeEvent{
		ID:            NewTurnID(),
		SessionID:     t.req.SessionID,
		TurnID:        t.turnID,
		Author:        t.req.PrincipalID,
		Kind:          turn.EventKindTurnCancelled,
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

func (e *Engine) fillTurnEvent(t *turnState, ev *store.RuntimeEvent) {
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

func (e *Engine) terminalOutcome(ctx context.Context, execErr error, req *turn.TurnRequest) (kind string, payload []byte) {
	switch {
	case ctx.Err() == context.Canceled:
		return turn.EventKindTurnCancelled, cancelledPayload("turn cancelled", e.agentID(req))
	case ctx.Err() == context.DeadlineExceeded:
		return turn.EventKindTurnFailed, failedPayload(turn.ErrorCodeTurnDeadlineExceeded, "turn deadline elapsed", nil, e.agentID(req))
	case execErr != nil:
		code, ok := turn.CodeOf(execErr)
		if !ok {
			code = turn.ErrorCodeRuntimeInternal
		}
		return turn.EventKindTurnFailed, failedPayload(code, "turn execution failed", execErr, e.agentID(req))
	default:
		return turn.EventKindTurnCompleted, completedPayload(e.agentID(req))
	}
}

func (e *Engine) finishTurn(ctx context.Context, t *turnState) {
	e.mu.Lock()
	e.active--
	close(t.done)
	delete(e.turns, t.turnID)
	e.mu.Unlock()
	e.schedule(ctx)
}

const durableStatusPoll = 500 * time.Millisecond

const turnStartAction = "turn-start"

func (e *Engine) turnDeadline(ctx context.Context, inv durable.Invocation, req *turn.TurnRequest) (time.Time, error) {
	if !req.Deadline.IsZero() {
		return req.Deadline, nil
	}
	raw, err := inv.RunAction(ctx, turnStartAction, func(context.Context) ([]byte, error) {
		return json.Marshal(time.Now().UTC())
	})
	if err != nil {
		return time.Time{}, codedError(turn.ErrorCodeStorageUnavailable, "failed to journal the turn start", err)
	}
	var started time.Time
	if err := json.Unmarshal(raw, &started); err != nil {
		return time.Time{}, codedError(turn.ErrorCodeStorageUnavailable, "failed to decode the turn start", err)
	}
	return started.Add(e.cfg.TurnTimeout), nil
}

func (e *Engine) closeCancelledPresence(ctx context.Context, req *turn.TurnRequest) {
	if !durable.Preempted(ctx) {
		e.notifyPresenceFailed(context.WithoutCancel(ctx), req)
	}
	e.notifyPresenceEnded(ctx, req)
}

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
	if e.sessionStore == nil {
		return invalidArgument("durable session store is not configured")
	}
	scope := durable.NewTurnScope(inv)
	if scope == nil {
		return invalidArgument("durable turn drive requires an invocation")
	}
	req := turnRequestFromDescriptor(desc)
	events, err := e.turnEvents(ctx, req.TurnID)
	if err != nil {
		return err
	}
	if hasTerminalKind(events) {
		return e.releaseAndAdvance(ctx, scope, req)
	}
	held, err := e.sessionHoldsTurn(ctx, req)
	if err != nil {
		return err
	}
	if !held {
		return e.refuseUnheldTurn(ctx, req)
	}
	accepted, err := acceptedEventOf(events)
	if err != nil {
		return err
	}
	ctx = durable.WithTurnScope(ctx, scope)
	t := &turnState{req: *req, turnID: req.TurnID, accepted: *accepted}
	deadline, err := e.turnDeadline(ctx, inv, req)
	if err != nil {
		return err
	}
	ctx, stop := context.WithDeadline(ctx, deadline)
	defer stop()

	persist := func(ctx context.Context, ev *store.RuntimeEvent) error {
		if ev.ID == "" {
			ev.ID = scope.NextEventID(req.TurnID)
		}
		if ev.CreatedAt.IsZero() {
			nowRaw, clockErr := scope.Invocation().RunAction(ctx, scope.NextClock(), func(ctx context.Context) ([]byte, error) {
				return json.Marshal(time.Now().UTC())
			})
			if clockErr != nil {
				return codedError(turn.ErrorCodeStorageUnavailable, "failed to journal "+ev.Kind, clockErr)
			}
			var journaled time.Time
			if err := json.Unmarshal(nowRaw, &journaled); err != nil {
				return codedError(turn.ErrorCodeStorageUnavailable, "failed to decode durable clock", err)
			}
			ev.CreatedAt = journaled.UTC()
		}
		e.fillTurnEvent(t, ev)
		sequence, _, err := e.events.UpsertEvent(ctx, ev)
		if err != nil {
			return codedError(turn.ErrorCodeStorageUnavailable, "failed to persist "+ev.Kind, err)
		}
		ev.Sequence = sequence
		e.broadcast(t, ev)
		e.Publish(ev)
		return nil
	}

	var executed []store.RuntimeEvent
	e.notifyPresenceStarted(ctx, req)
	execErr := e.persistSkillActivation(ctx, req, journaledSkillActivation(scope, e.renderSkillActivation), persist)
	if execErr == nil {
		var nextDelta uint64
		for ev, err := range e.executor.Execute(ctx, req) {
			if err != nil {
				execErr = err
				break
			}
			if delta, partial, deltaErr := e.deltaFromPartial(t, &nextDelta, &ev); partial {
				if deltaErr != nil {
					execErr = deltaErr
					break
				}
				if delta != nil {
					e.broadcast(t, delta)
					e.Publish(delta)
				}
				continue
			}
			ev.ID = scope.NextEventID(req.TurnID)
			if emitErr := persist(ctx, &ev); emitErr != nil {
				execErr = emitErr
				break
			}
			executed = append(executed, ev)
			if isPresenceFailureEvent(&ev) {
				e.notifyPresenceFailed(ctx, req)
				continue
			}
			if text, args, ok := extractPresenceActivity(&ev); ok {
				e.notifyPresenceText(ctx, req, text, args)
			}
		}
	}
	if ctx.Err() == context.Canceled {
		e.closeCancelledPresence(ctx, req)
		return ctx.Err()
	}
	terminalKind, terminalPayload := e.terminalOutcome(ctx, execErr, req)
	if terminalKind == turn.EventKindTurnCompleted {
		record, err := e.journalDelivery(context.WithoutCancel(ctx), scope, &completedTurn{req: req, events: executed})
		if err != nil {
			return err
		}
		terminalKind, terminalPayload = record.terminal(e.agentID(req))
	}
	if e.cfg.PostTurnHook != nil {
		hookCtx := context.WithoutCancel(ctx)
		if hookErr := e.cfg.PostTurnHook(hookCtx, req, executed); hookErr != nil {
			e.logger.ErrorContext(hookCtx, "post-turn hook failed", "error", hookErr, "turn_id", req.TurnID)
		}
	}
	if terminalKind != turn.EventKindTurnCompleted {
		e.notifyPresenceFailed(context.WithoutCancel(ctx), req)
	}
	e.notifyPresenceEnded(ctx, req)
	terminal := &store.RuntimeEvent{ID: req.TurnID + "-terminal", Kind: terminalKind, Payload: terminalPayload}
	if err := persist(context.WithoutCancel(ctx), terminal); err != nil {
		return err
	}
	return e.releaseAndAdvance(context.WithoutCancel(ctx), scope, req)
}

const deliveryAction = "delivery"

func (e *Engine) journalDelivery(ctx context.Context, scope *durable.TurnScope, done *completedTurn) (deliveryRecord, error) {
	if done == nil || done.req == nil {
		return deliveryRecord{}, invalidArgument("turn delivery requires the completed turn")
	}
	raw, err := scope.Invocation().RunAction(ctx, deliveryAction, func(ctx context.Context) ([]byte, error) {
		return json.Marshal(e.deliverOnce(ctx, done))
	})
	if err != nil {
		return deliveryRecord{}, codedError(turn.ErrorCodeStorageUnavailable, "failed to journal the turn delivery", err)
	}
	var record deliveryRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return deliveryRecord{}, codedError(turn.ErrorCodeStorageUnavailable, "failed to decode the turn delivery", err)
	}
	return record, nil
}

func (e *Engine) releaseAndAdvance(ctx context.Context, scope *durable.TurnScope, req *turn.TurnRequest) error {
	_, err := scope.Invocation().RunAction(ctx, "release", func(ctx context.Context) ([]byte, error) {
		if _, err := e.sessionStore.Release(ctx, req.SessionID, req.TurnID); err != nil {
			return nil, err
		}
		return []byte(`{}`), nil
	})
	e.advanceSession(ctx, req.SessionID)
	if err != nil {
		return codedError(turn.ErrorCodeStorageUnavailable, "failed to release durable turn", err)
	}
	return nil
}

func (e *Engine) sessionHoldsTurn(ctx context.Context, req *turn.TurnRequest) (bool, error) {
	status, err := e.sessionStore.Status(ctx, req.SessionID)
	if err != nil {
		return false, codedError(turn.ErrorCodeStorageUnavailable, "failed to read the durable session", err)
	}
	return status.Active != nil && status.Active.Descriptor.TurnID == req.TurnID, nil
}

const unheldTurnDetail = "turn is no longer held by its session"

func (e *Engine) refuseUnheldTurn(ctx context.Context, req *turn.TurnRequest) error {
	cancelled := &store.RuntimeEvent{
		ID:            req.TurnID + "-cancel",
		SessionID:     req.SessionID,
		TurnID:        req.TurnID,
		Author:        req.PrincipalID,
		Kind:          turn.EventKindTurnCancelled,
		SchemaVersion: 1,
		Payload:       cancelledPayload(unheldTurnDetail, e.agentID(req)),
		CreatedAt:     time.Now().UTC(),
	}
	if _, _, err := e.events.UpsertEvent(context.WithoutCancel(ctx), cancelled); err != nil {
		e.logger.ErrorContext(ctx, "runtime failed to persist the refusal of an unheld turn", "error", err, "turn_id", req.TurnID)
	}
	return codedError(turn.ErrorCodeTurnCancelled, unheldTurnDetail, nil)
}

func (e *Engine) replayStoredToLive(ctx context.Context, t *turnState) {
	events, err := e.dedupe.ListTurnEvents(context.WithoutCancel(ctx), t.turnID)
	if err != nil {
		e.logger.ErrorContext(ctx, "durable turn replay failed", "error", err, "turn_id", t.turnID)
		return
	}
	for i := range events {
		ev := events[i]
		e.broadcast(t, &ev)
	}
}

func (e *Engine) pollDurableTurn(ctx context.Context, t *turnState) {
	defer e.wg.Done()
	defer e.finishTurn(ctx, t)
	e.broadcast(t, &t.accepted)
	ref := durable.RunRef{Key: t.turnID}
	if !t.attached {
		if ctx.Err() != nil {
			e.endCancelledTurn(ctx, t)
			return
		}
		desc := turnDescriptorFromRequest(&t.req)
		payload, err := json.Marshal(desc)
		if err != nil {
			e.failDurableStart(ctx, t, err)
			return
		}
		ref, err = e.durableRuntime.Start(ctx, durable.StartRequest{Handler: HandlerName, Key: t.turnID, Payload: payload})
		if err != nil {
			e.failDurableStart(ctx, t, err)
			return
		}
	}
	ticker := time.NewTicker(e.statusPoll)
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
			case durable.RunSucceeded:
				e.settleSucceededTurn(ctx, t)
				e.replayStoredToLive(ctx, t)
				e.closeSubscribers(t)
				return
			case durable.RunFailed, durable.RunCancelled:
				if e.recordRunEnd(ctx, t, &status) {
					e.releaseTurn(ctx, t)
					e.replayStoredToLive(ctx, t)
				}
				e.closeSubscribers(t)
				return
			case durable.RunRunning, durable.RunSuspended:
				continue
			}
		}
	}
}

func (e *Engine) closeSubscribers(t *turnState) {
	for sub := range t.subs {
		sub.closeEvents()
	}
}

func (e *Engine) recordRunEnd(ctx context.Context, t *turnState, status *durable.RunStatus) bool {
	ctx = context.WithoutCancel(ctx)
	ended, err := e.hasTerminalEvent(ctx, t)
	if err != nil {
		e.logger.ErrorContext(ctx, "runtime failed to read the events of an ended durable turn", "error", err, "turn_id", t.turnID)
		return false
	}
	if ended {
		return true
	}
	terminal := &store.RuntimeEvent{
		ID:            t.turnID + "-terminal",
		SessionID:     t.req.SessionID,
		TurnID:        t.turnID,
		Author:        t.req.PrincipalID,
		Kind:          turn.EventKindTurnFailed,
		SchemaVersion: 1,
		Payload:       failedPayload(turn.ErrorCodeRuntimeInternal, cmp.Or(status.Detail, "durable turn run failed"), nil, e.agentID(&t.req)),
		CreatedAt:     time.Now().UTC(),
	}
	if status.State == durable.RunCancelled {
		terminal.ID = t.turnID + "-cancel"
		terminal.Kind = turn.EventKindTurnCancelled
		terminal.Payload = cancelledPayload("turn cancelled", e.agentID(&t.req))
	}
	if _, _, err := e.events.UpsertEvent(ctx, terminal); err != nil {
		e.logger.ErrorContext(ctx, "runtime failed to persist the end of a durable turn", "error", err, "turn_id", t.turnID)
	}
	return true
}

func (e *Engine) cancelDurableTurn(ctx context.Context, t *turnState, ref durable.RunRef) {
	if e.letGoOfEndedTurn(ctx, t) {
		return
	}
	if err := e.durableRuntime.Cancel(context.WithoutCancel(ctx), ref); err != nil {
		e.logger.DebugContext(ctx, "durable turn cancel failed", "error", err, "turn_id", t.turnID)
	}
	if e.letGoOfEndedTurn(ctx, t) {
		return
	}
	e.endCancelledTurn(ctx, t)
}

func (e *Engine) letGoOfEndedTurn(ctx context.Context, t *turnState) bool {
	ended, err := e.hasTerminalEvent(context.WithoutCancel(ctx), t)
	if err != nil || !ended {
		return false
	}
	e.replayStoredToLive(ctx, t)
	e.closeSubscribers(t)
	e.releaseTurn(ctx, t)
	return true
}

func (e *Engine) endCancelledTurn(ctx context.Context, t *turnState) {
	cancelled := &store.RuntimeEvent{
		ID:            t.turnID + "-cancel",
		SessionID:     t.req.SessionID,
		TurnID:        t.turnID,
		Author:        t.req.PrincipalID,
		Kind:          turn.EventKindTurnCancelled,
		SchemaVersion: 1,
		Payload:       cancelledPayload("turn cancelled", e.agentID(&t.req)),
		CreatedAt:     time.Now().UTC(),
	}
	if _, _, err := e.events.UpsertEvent(context.WithoutCancel(ctx), cancelled); err != nil {
		e.logger.ErrorContext(ctx, "runtime failed to persist turn cancellation", "error", err, "turn_id", t.turnID)
	}
	e.broadcast(t, &t.accepted)
	e.broadcast(t, cancelled)
	e.closeSubscribers(t)
	e.releaseTurn(ctx, t)
}

func (e *Engine) hasTerminalEvent(ctx context.Context, t *turnState) (bool, error) {
	events, err := e.dedupe.ListTurnEvents(ctx, t.turnID)
	if err != nil {
		return false, err
	}
	return hasTerminalKind(events), nil
}

func (e *Engine) failDurableStart(ctx context.Context, t *turnState, cause error) {
	terminal := &store.RuntimeEvent{
		ID:            t.turnID + "-terminal",
		SessionID:     t.req.SessionID,
		TurnID:        t.turnID,
		Author:        t.req.PrincipalID,
		Kind:          turn.EventKindTurnFailed,
		SchemaVersion: 1,
		Payload:       failedPayload(turn.ErrorCodeStorageUnavailable, "durable turn did not start", cause, e.agentID(&t.req)),
		CreatedAt:     time.Now().UTC(),
	}
	if _, _, err := e.events.UpsertEvent(context.WithoutCancel(ctx), terminal); err != nil {
		e.logger.ErrorContext(ctx, "runtime failed to persist turn start failure", "error", err, "turn_id", t.turnID)
	}
	e.broadcast(t, &t.accepted)
	e.broadcast(t, terminal)
	e.closeSubscribers(t)
	e.releaseTurn(ctx, t)
}
