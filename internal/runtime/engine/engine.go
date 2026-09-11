package runtimeengine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"sync"
	"time"

	"github.com/anggasct/aura/internal/durable"
	"github.com/anggasct/aura/internal/runtime"
	"github.com/anggasct/aura/internal/store"
)

type Config struct {
	MaxActiveTurns  int
	MaxPendingTurns int
	TurnTimeout     time.Duration
	ShutdownTimeout time.Duration
	DefaultAgentID  string
	Durable         *DurableConfig
}

type DurableConfig struct {
	Sessions SessionStore
	Runtime  durable.Runtime
}

func (c *Config) applyDefaults() {
	if c.MaxActiveTurns <= 0 {
		c.MaxActiveTurns = 4
	}
	if c.MaxPendingTurns <= 0 {
		c.MaxPendingTurns = 64
	}
	if c.TurnTimeout <= 0 {
		c.TurnTimeout = 5 * time.Minute
	}
	if c.ShutdownTimeout <= 0 {
		c.ShutdownTimeout = 30 * time.Second
	}
}

func (c *Config) validate() error {
	var problems []error
	if c.MaxActiveTurns <= 0 {
		problems = append(problems, invalidArgument("max_active_turns must be positive"))
	}
	if c.MaxPendingTurns < c.MaxActiveTurns {
		problems = append(problems, invalidArgument("max_pending_turns must be at least max_active_turns"))
	}
	if c.TurnTimeout <= 0 {
		problems = append(problems, invalidArgument("turn_timeout must be positive"))
	}
	if c.ShutdownTimeout <= 0 {
		problems = append(problems, invalidArgument("shutdown_timeout must be positive"))
	}
	return errors.Join(problems...)
}

type TurnExecutor interface {
	Execute(ctx context.Context, req *runtime.TurnRequest) iter.Seq2[store.RuntimeEvent, error]
}

type Engine struct {
	cfg            Config
	events         EventStore
	dedupe         DedupeStore
	executor       TurnExecutor
	logger         *slog.Logger
	sessionStore   SessionStore
	durableRuntime durable.Runtime

	mu       sync.Mutex
	sessions map[string]*sessionQueue
	turns    map[string]*turn
	staged   map[string]*turn
	granted  map[string]struct{}
	sesKeys  map[string]struct{}
	pending  int
	active   int
	shutdown bool
	wg       sync.WaitGroup
}

type EventStore interface {
	Append(ctx context.Context, e *store.RuntimeEvent) error
	AppendSequenced(ctx context.Context, sessionID string, e *store.RuntimeEvent) (uint64, error)
	UpsertEvent(ctx context.Context, e *store.RuntimeEvent) (uint64, bool, error)
	LastSequence(ctx context.Context, sessionID string) (uint64, error)
}

type DedupeStore interface {
	Accept(ctx context.Context, source, externalID string, expiresAt time.Time, accepted *store.RuntimeEvent) (originalTurnID string, created bool, err error)
	ListTurnEvents(ctx context.Context, turnID string) ([]store.RuntimeEvent, error)
}

func NewEngine(cfg Config, events EventStore, dedupe DedupeStore, executor TurnExecutor, logger *slog.Logger) (*Engine, error) {
	if events == nil {
		return nil, invalidArgument("event store must not be nil")
	}
	if dedupe == nil {
		return nil, invalidArgument("dedupe store must not be nil")
	}
	if executor == nil {
		return nil, invalidArgument("executor must not be nil")
	}
	if logger == nil {
		logger = slog.Default()
	}
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	engine := &Engine{
		cfg:      cfg,
		events:   events,
		dedupe:   dedupe,
		executor: executor,
		logger:   logger,
		sessions: make(map[string]*sessionQueue),
		turns:    make(map[string]*turn),
		staged:   make(map[string]*turn),
		granted:  make(map[string]struct{}),
		sesKeys:  make(map[string]struct{}),
	}
	if cfg.Durable != nil {
		engine.sessionStore = cfg.Durable.Sessions
		engine.durableRuntime = cfg.Durable.Runtime
	}
	if registrar, ok := engine.durableRuntime.(durable.HandlerRegistrar); ok {
		registrar.RegisterHandler("turn", engine.serveTurn)
	}
	return engine, nil
}

type sessionQueue struct {
	mu     sync.Mutex
	active bool
	queue  []*turn
}

type turn struct {
	req      runtime.TurnRequest
	turnID   string
	accepted store.RuntimeEvent
	subs     map[*subscriber]struct{}
	cancel   context.CancelFunc
	start    func()
	done     chan struct{}
}

type subscriber struct {
	events  chan store.RuntimeEvent
	done    chan struct{}
	once    sync.Once
	mu      sync.Mutex
	closed  bool
	gapped  bool
	lastSeq uint64
}

func (s *subscriber) trackLocked(ev *store.RuntimeEvent) bool {
	if ev == nil {
		return false
	}
	if ev.Sequence != 0 {
		if ev.Sequence <= s.lastSeq {
			return false
		}
		s.lastSeq = ev.Sequence
	}
	return true
}

const subscriberBufferSize = 64

func newSubscriber() *subscriber {
	return &subscriber{
		events: make(chan store.RuntimeEvent, subscriberBufferSize),
		done:   make(chan struct{}),
	}
}

func (s *subscriber) stop() {
	s.once.Do(func() { close(s.done) })
}

func (s *subscriber) send(ev *store.RuntimeEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || !s.trackLocked(ev) {
		return
	}
	select {
	case s.events <- *ev:
		return
	case <-s.done:
		return
	default:
	}
	if ev.Kind == runtime.EventKindModelDelta {
		return
	}
	if !s.makeRoomLocked() {
		s.closed = true
		s.gapped = true
		close(s.events)
		return
	}
	s.events <- *ev
}

func (s *subscriber) makeRoomLocked() bool {
	queued := make([]store.RuntimeEvent, 0, len(s.events))
	removedDelta := false
	for {
		select {
		case queuedEvent := <-s.events:
			if !removedDelta && queuedEvent.Kind == runtime.EventKindModelDelta {
				removedDelta = true
				continue
			}
			queued = append(queued, queuedEvent)
		default:
			for i := range queued {
				s.events <- queued[i]
			}
			return removedDelta || len(s.events) < cap(s.events)
		}
	}
}

func (s *subscriber) sendContext(ctx context.Context, ev *store.RuntimeEvent) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || !s.trackLocked(ev) {
		return false
	}
	select {
	case s.events <- *ev:
		return true
	case <-s.done:
		return false
	case <-ctx.Done():
		return false
	}
}

func (s *subscriber) closeEvents() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	close(s.events)
}

func (s *subscriber) wasGapped() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gapped
}

var _ runtime.AgentRuntime = (*Engine)(nil)

func (e *Engine) Run(ctx context.Context, req *runtime.TurnRequest) iter.Seq2[store.RuntimeEvent, error] {
	return func(yield func(store.RuntimeEvent, error) bool) {
		copyReq := *req
		sub, err := e.submit(ctx, &copyReq)
		if err != nil {
			yield(store.RuntimeEvent{}, err)
			return
		}
		defer sub.stop()

		var lastSequence uint64
		for {
			select {
			case <-ctx.Done():
				e.cancelTurn(copyReq.TurnID)
				e.drainUntilTerminal(ctx, copyReq.TurnID, sub, yield)
				return
			case ev, ok := <-sub.events:
				if !ok {
					if sub.wasGapped() {
						e.replayAfterGap(ctx, copyReq.TurnID, lastSequence, yield)
					}
					return
				}
				lastSequence = ev.Sequence
				if !yield(ev, nil) {
					return
				}
				if isTerminalKind(ev.Kind) {
					return
				}
			}
		}
	}
}

func (e *Engine) drainUntilTerminal(ctx context.Context, turnID string, sub *subscriber, yield func(store.RuntimeEvent, error) bool) {
	var lastSequence uint64
	for ev := range sub.events {
		lastSequence = ev.Sequence
		if !yield(ev, nil) {
			return
		}
		if isTerminalKind(ev.Kind) {
			return
		}
	}
	if sub.wasGapped() {
		e.replayAfterGap(ctx, turnID, lastSequence, yield)
	}
}

func (e *Engine) replayAfterGap(ctx context.Context, turnID string, afterSequence uint64, yield func(store.RuntimeEvent, error) bool) {
	e.mu.Lock()
	t, live := e.turns[turnID]
	e.mu.Unlock()
	if live {
		<-t.done
	}

	events, err := e.dedupe.ListTurnEvents(context.WithoutCancel(ctx), turnID)
	if err != nil {
		yield(store.RuntimeEvent{Kind: runtime.EventKindTurnFailed, Payload: failedPayload(runtime.ErrorCodeRuntimeInternal, "failed to replay the live turn", err, "")}, nil)
		return
	}
	for i := range events {
		if events[i].Sequence <= afterSequence {
			continue
		}
		if !yield(events[i], nil) {
			return
		}
		if isTerminalKind(events[i].Kind) {
			return
		}
	}
}

func isTerminalKind(kind string) bool {
	switch kind {
	case runtime.EventKindTurnCompleted, runtime.EventKindTurnFailed, runtime.EventKindTurnCancelled:
		return true
	}
	return false
}

func (e *Engine) claim(ctx context.Context, req *runtime.TurnRequest) (accepted store.RuntimeEvent, originalTurnID string, replay, granted bool, err error) {
	if req == nil {
		return store.RuntimeEvent{}, "", false, false, invalidArgument("turn request must not be nil")
	}
	if req.SessionID == "" {
		return store.RuntimeEvent{}, "", false, false, invalidArgument("session id must not be empty")
	}
	if req.Origin == "" {
		return store.RuntimeEvent{}, "", false, false, invalidArgument("origin must not be empty")
	}
	if req.TurnID == "" {
		req.TurnID = NewTurnID()
	}

	e.mu.Lock()
	if e.shutdown {
		e.mu.Unlock()
		return store.RuntimeEvent{}, "", false, false, codedError(runtime.ErrorCodeRuntimeOverloaded, "runtime is shutting down", nil)
	}
	if e.pending >= e.cfg.MaxPendingTurns {
		e.mu.Unlock()
		return store.RuntimeEvent{}, "", false, false, codedError(runtime.ErrorCodeRuntimeOverloaded, "pending turn queue is full", nil)
	}
	e.pending++
	e.mu.Unlock()

	if e.sessionStore != nil {
		return e.claimDurable(ctx, req)
	}

	e.mu.Lock()
	sq := e.sessions[req.SessionID]
	if sq == nil {
		sq = &sessionQueue{}
		e.sessions[req.SessionID] = sq
	}
	e.mu.Unlock()

	accepted = acceptedEvent(req, 0, e.agentID(req))
	if req.IdempotencyKey != "" {
		err = sq.lock(func() error {
			seq, seqErr := e.nextSequence(ctx, req.SessionID)
			if seqErr != nil {
				return seqErr
			}
			accepted.Sequence = seq
			var created bool
			originalTurnID, created, seqErr = e.dedupe.Accept(ctx, string(req.Origin), req.IdempotencyKey, time.Now().Add(dedupeWindow), &accepted)
			replay = !created
			return seqErr
		})
		if err != nil {
			e.releasePending()
			return store.RuntimeEvent{}, "", false, false, codedError(runtime.ErrorCodeStorageUnavailable, "dedupe claim failed", err)
		}
		return accepted, originalTurnID, replay, false, nil
	}

	err = sq.lock(func() error {
		seq, seqErr := e.nextSequence(ctx, req.SessionID)
		if seqErr != nil {
			return seqErr
		}
		accepted.Sequence = seq
		return e.events.Append(ctx, &accepted)
	})
	if err != nil {
		e.releasePending()
		return store.RuntimeEvent{}, "", false, false, codedError(runtime.ErrorCodeStorageUnavailable, "failed to persist the accepted turn", err)
	}
	return accepted, "", false, false, nil
}

func (e *Engine) enqueue(ctx context.Context, req *runtime.TurnRequest, accepted *store.RuntimeEvent, sub *subscriber) {
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
		e.releasePending()
		go e.terminateQueued(context.WithoutCancel(ctx), t)
		return
	}
	e.sessions[req.SessionID].queue = append(e.sessions[req.SessionID].queue, t)
	e.turns[req.TurnID] = t
	e.mu.Unlock()

	e.schedule(ctx)
}

func (e *Engine) submit(ctx context.Context, req *runtime.TurnRequest) (*subscriber, error) {
	accepted, originalTurnID, replay, granted, err := e.claim(ctx, req)
	if err != nil {
		return nil, err
	}
	sub := newSubscriber()
	if replay {
		e.releasePending()
		go e.replay(ctx, originalTurnID, sub)
		return sub, nil
	}
	if e.sessionStore != nil {
		e.stageDurable(ctx, req, &accepted, sub, granted)
		return sub, nil
	}
	e.enqueue(ctx, req, &accepted, sub)
	return sub, nil
}

const dedupeWindow = 24 * time.Hour

func (e *Engine) releasePending() {
	e.mu.Lock()
	e.pending--
	e.mu.Unlock()
}

func acceptedEvent(req *runtime.TurnRequest, seq uint64, agentID string) store.RuntimeEvent {
	payload := fmt.Sprintf(`{"origin":%q,"principal":%q}`, req.Origin, req.PrincipalID)
	if agentID != "" {
		payload = fmt.Sprintf(`{"origin":%q,"principal":%q,"agent_id":%q}`, req.Origin, req.PrincipalID, agentID)
	}
	return store.RuntimeEvent{
		ID:            NewTurnID(),
		SessionID:     req.SessionID,
		Sequence:      seq,
		TurnID:        req.TurnID,
		Author:        req.PrincipalID,
		Kind:          runtime.EventKindTurnAccepted,
		SchemaVersion: 1,
		Payload:       []byte(payload),
		CreatedAt:     time.Now().UTC(),
	}
}

func (e *Engine) agentID(req *runtime.TurnRequest) string {
	if req.AgentID != "" {
		return req.AgentID
	}
	return e.cfg.DefaultAgentID
}

func (e *Engine) nextSequence(ctx context.Context, sessionID string) (uint64, error) {
	last, err := e.events.LastSequence(ctx, sessionID)
	if err != nil {
		return 0, err
	}
	return last + 1, nil
}

func (e *Engine) sessionQueue(sessionID string) *sessionQueue {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.sessions[sessionID]
}

func (sq *sessionQueue) lock(fn func() error) error {
	sq.mu.Lock()
	defer sq.mu.Unlock()
	return fn()
}

func (e *Engine) schedule(ctx context.Context) {
	if e.sessionStore != nil {
		e.scheduleDurable(ctx)
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.startRunnableLocked()
}

func (e *Engine) startRunnableLocked() {
	for e.active < e.cfg.MaxActiveTurns && !e.shutdown {
		var picked *turn
		for _, sq := range e.sessions {
			if sq.active || len(sq.queue) == 0 {
				continue
			}
			picked = sq.queue[0]
			sq.queue = sq.queue[1:]
			sq.active = true
			e.active++
			e.pending--
			break
		}
		if picked == nil {
			return
		}
		e.wg.Add(1)
		go picked.start()
	}
}

func (e *Engine) runTurn(ctx context.Context, t *turn) {
	defer e.wg.Done()
	defer func() {
		e.mu.Lock()
		if sq := e.sessions[t.req.SessionID]; sq != nil {
			sq.active = false
		}
		e.mu.Unlock()
		e.finishTurn(ctx, t)
	}()

	deadline := t.req.Deadline
	if deadline.IsZero() {
		deadline = time.Now().Add(e.cfg.TurnTimeout)
	}
	ctx, stop := context.WithDeadline(ctx, deadline)
	defer stop()

	persistEvent := func(ctx context.Context, ev *store.RuntimeEvent) error {
		e.fillTurnEvent(t, ev)
		if e.sessionStore != nil {
			if _, err := e.events.AppendSequenced(ctx, t.req.SessionID, ev); err != nil {
				return codedError(runtime.ErrorCodeStorageUnavailable, "failed to persist "+ev.Kind, err)
			}
			e.broadcast(t, ev)
			return nil
		}
		err := e.sessionQueue(t.req.SessionID).lock(func() error {
			seq, err := e.nextSequence(ctx, t.req.SessionID)
			if err != nil {
				return err
			}
			ev.Sequence = seq
			return e.events.Append(ctx, ev)
		})
		if err != nil {
			return codedError(runtime.ErrorCodeStorageUnavailable, "failed to persist "+ev.Kind, err)
		}
		e.broadcast(t, ev)
		return nil
	}

	emit := func(ctx context.Context, kind string, payload []byte) error {
		ev := &store.RuntimeEvent{Kind: kind, Payload: payload}
		return persistEvent(ctx, ev)
	}

	e.broadcast(t, &t.accepted)

	var terminalKind string
	var terminalPayload []byte
	var execErr error
	for ev, err := range e.executor.Execute(ctx, &t.req) {
		if err != nil {
			execErr = err
			break
		}
		if emitErr := persistEvent(ctx, &ev); emitErr != nil {
			execErr = emitErr
			break
		}
	}

	terminalKind, terminalPayload = e.terminalOutcome(ctx, execErr, &t.req)
	if err := emit(context.WithoutCancel(ctx), terminalKind, terminalPayload); err != nil {
		e.logger.ErrorContext(ctx, "runtime failed to persist terminal event", "error", err, "turn_id", t.turnID)
	}
	if e.sessionStore != nil {
		e.releaseTurn(ctx, t)
	}

	for sub := range t.subs {
		sub.closeEvents()
	}
}

func (e *Engine) broadcast(t *turn, ev *store.RuntimeEvent) {
	for sub := range t.subs {
		sub.send(ev)
	}
}

func (e *Engine) Publish(ev *store.RuntimeEvent) {
	if ev == nil || ev.TurnID == "" {
		return
	}
	e.mu.Lock()
	t := e.turns[ev.TurnID]
	if t == nil || t.req.SessionID != ev.SessionID {
		e.mu.Unlock()
		return
	}
	subs := make([]*subscriber, 0, len(t.subs))
	for sub := range t.subs {
		subs = append(subs, sub)
	}
	e.mu.Unlock()
	for _, sub := range subs {
		sub.send(ev)
	}
}

func (e *Engine) cancelTurn(turnID string) {
	e.mu.Lock()
	t, ok := e.turns[turnID]
	e.mu.Unlock()
	if !ok || t.cancel == nil {
		return
	}
	t.cancel()
}

func (e *Engine) replay(ctx context.Context, turnID string, sub *subscriber) {
	defer sub.stop()
	e.mu.Lock()
	t, live := e.turns[turnID]
	e.mu.Unlock()
	if live {
		select {
		case <-t.done:
		case <-sub.done:
			return
		}
	}
	events, err := e.dedupe.ListTurnEvents(ctx, turnID)
	if err != nil {
		sub.sendContext(ctx, &store.RuntimeEvent{Kind: runtime.EventKindTurnFailed, Payload: failedPayload(runtime.ErrorCodeRuntimeInternal, "failed to replay the original turn", err, "")})
		return
	}
	for i := range events {
		if !sub.sendContext(ctx, &events[i]) {
			return
		}
		if isTerminalKind(events[i].Kind) {
			return
		}
	}
	sub.closeEvents()
}

func (e *Engine) Shutdown(ctx context.Context) error {
	e.mu.Lock()
	e.shutdown = true
	var queued []*turn
	for _, sq := range e.sessions {
		queued = append(queued, sq.queue...)
		sq.queue = nil
	}
	var staged []*turn
	var sesKeys []string
	if e.sessionStore != nil {
		for _, t := range e.staged {
			staged = append(staged, t)
		}
		clear(e.staged)
		clear(e.granted)
		for key := range e.sesKeys {
			sesKeys = append(sesKeys, key)
		}
		clear(e.sesKeys)
	}
	e.pending = 0
	e.mu.Unlock()

	drainCtx := context.WithoutCancel(ctx)
	for _, t := range queued {
		e.terminateQueued(drainCtx, t)
	}
	for _, t := range staged {
		e.terminateStaged(drainCtx, t)
	}
	for _, key := range sesKeys {
		if _, err := e.sessionStore.Abort(drainCtx, key); err != nil {
			e.logger.ErrorContext(drainCtx, "runtime failed to abort durable session", "error", err)
		}
	}

	done := make(chan struct{})
	go func() {
		e.wg.Wait()
		close(done)
	}()

	grace := e.cfg.ShutdownTimeout
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining < grace {
			grace = remaining
		}
	}
	timer := time.NewTimer(grace)
	defer timer.Stop()

	select {
	case <-done:
		return nil
	case <-timer.C:
		e.mu.Lock()
		for _, t := range e.turns {
			if t.cancel != nil {
				t.cancel()
			}
		}
		e.mu.Unlock()
		timer.Reset(grace)
		select {
		case <-done:
			return nil
		case <-timer.C:
			return codedError(runtime.ErrorCodeRuntimeInternal, "shutdown grace elapsed with turns still draining", nil)
		}
	}
}

func (e *Engine) terminateQueued(ctx context.Context, t *turn) {
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
	err := e.sessionQueue(t.req.SessionID).lock(func() error {
		seq, err := e.nextSequence(ctx, t.req.SessionID)
		if err != nil {
			return err
		}
		terminal.Sequence = seq
		return e.events.Append(ctx, terminal)
	})
	if err != nil {
		e.logger.ErrorContext(ctx, "runtime failed to persist queued terminal event", "error", err, "turn_id", t.turnID)
		return
	}
	e.broadcast(t, &t.accepted)
	e.broadcast(t, terminal)
	for sub := range t.subs {
		sub.closeEvents()
	}
	e.mu.Lock()
	delete(e.turns, t.turnID)
	close(t.done)
	e.mu.Unlock()
}

func completedPayload(agentID string) []byte {
	if agentID != "" {
		return []byte(fmt.Sprintf(`{"outcome":"completed","agent_id":%q}`, agentID))
	}
	return []byte(`{"outcome":"completed"}`)
}

func failedPayload(code runtime.ErrorCode, detail string, cause error, agentID string) []byte {
	if cause != nil {
		detail = detail + ": " + cause.Error()
	}
	if agentID != "" {
		return []byte(fmt.Sprintf(`{"code":%q,"detail":%q,"agent_id":%q}`, code, detail, agentID))
	}
	return []byte(fmt.Sprintf(`{"code":%q,"detail":%q}`, code, detail))
}

func cancelledPayload(detail, agentID string) []byte {
	if agentID != "" {
		return []byte(fmt.Sprintf(`{"reason":"cancelled","detail":%q,"agent_id":%q}`, detail, agentID))
	}
	return []byte(fmt.Sprintf(`{"reason":"cancelled","detail":%q}`, detail))
}

func NewTurnID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return "turn_" + hex.EncodeToString(b[:])
}
