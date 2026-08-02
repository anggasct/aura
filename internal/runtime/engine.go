package runtime

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

	"github.com/anggasct/aura/internal/store"
)

// Config bounds the queue. Zero fields select the contract defaults: four
// active turns, 64 pending turns, a five-minute turn timeout, and a
// 30-second shutdown grace.
type Config struct {
	MaxActiveTurns  int
	MaxPendingTurns int
	TurnTimeout     time.Duration
	ShutdownTimeout time.Duration
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

// TurnExecutor runs one turn's inner loop. The fake executor is the
// deterministic step-1 implementation; the ADK runner adapter replaces it
// behind the same interface. Execute is named differently from the
// AgentRuntime boundary so the queued runtime and its inner loop cannot be
// conflated. Executor events carry kind and payload; the engine stamps
// identity, sequence, and timestamp, and persists every event before
// forwarding it.
type TurnExecutor interface {
	Execute(ctx context.Context, req *TurnRequest) iter.Seq2[store.RuntimeEvent, error]
}

// Engine implements AgentRuntime with a bounded per-session FIFO queue,
// global concurrency limits, durable terminal events, and stream
// subscribers that apply bounded backpressure.
type Engine struct {
	cfg      Config
	events   EventStore
	dedupe   DedupeStore
	executor TurnExecutor
	logger   *slog.Logger

	mu       sync.Mutex
	sessions map[string]*sessionQueue
	turns    map[string]*turn
	pending  int
	active   int
	shutdown bool
	wg       sync.WaitGroup
}

// EventStore is the subset of the store event port the runtime needs.
type EventStore interface {
	Append(ctx context.Context, e *store.RuntimeEvent) error
	LastSequence(ctx context.Context, sessionID string) (uint64, error)
}

// DedupeStore claims ingress keys and reads stored turn events. The claim and
// the accepted event land in one transaction, so a duplicate key never
// creates a second event sequence.
type DedupeStore interface {
	Accept(ctx context.Context, source, externalID string, expiresAt time.Time, accepted *store.RuntimeEvent) (originalTurnID string, created bool, err error)
	ListTurnEvents(ctx context.Context, turnID string) ([]store.RuntimeEvent, error)
}

// NewEngine builds a runtime over the given event and dedupe ports.
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
	return &Engine{
		cfg:      cfg,
		events:   events,
		dedupe:   dedupe,
		executor: executor,
		logger:   logger,
		sessions: make(map[string]*sessionQueue),
		turns:    make(map[string]*turn),
	}, nil
}

type sessionQueue struct {
	mu     sync.Mutex
	active bool
	queue  []*turn
}

type turn struct {
	req      TurnRequest
	turnID   string
	accepted store.RuntimeEvent
	subs     map[*subscriber]struct{}
	cancel   context.CancelFunc
	start    func()
	done     chan struct{}
}

type subscriber struct {
	events chan store.RuntimeEvent
	done   chan struct{}
	once   sync.Once
}

func newSubscriber() *subscriber {
	return &subscriber{
		events: make(chan store.RuntimeEvent, 64),
		done:   make(chan struct{}),
	}
}

// stop marks the subscriber abandoned; the worker stops forwarding to it but
// the turn continues to durable completion.
func (s *subscriber) stop() {
	s.once.Do(func() { close(s.done) })
}

var _ AgentRuntime = (*Engine)(nil)

// Run submits a turn and streams its events. A duplicate idempotency key
// replays the original turn's stored events and creates no second event
// sequence. The request is copied, so a caller-owned TurnID is never
// mutated.
func (e *Engine) Run(ctx context.Context, req *TurnRequest) iter.Seq2[store.RuntimeEvent, error] {
	return func(yield func(store.RuntimeEvent, error) bool) {
		copyReq := *req
		sub, err := e.submit(ctx, &copyReq)
		if err != nil {
			yield(store.RuntimeEvent{}, err)
			return
		}
		defer sub.stop()

		for {
			select {
			case <-ctx.Done():
				e.cancelTurn(copyReq.TurnID)
				e.drainUntilTerminal(sub, yield)
				return
			case ev, ok := <-sub.events:
				if !ok {
					return
				}
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

func (e *Engine) drainUntilTerminal(sub *subscriber, yield func(store.RuntimeEvent, error) bool) {
	for ev := range sub.events {
		if !yield(ev, nil) {
			return
		}
		if isTerminalKind(ev.Kind) {
			return
		}
	}
}

func isTerminalKind(kind string) bool {
	switch kind {
	case EventKindTurnCompleted, EventKindTurnFailed, EventKindTurnCancelled:
		return true
	}
	return false
}

func (e *Engine) submit(ctx context.Context, req *TurnRequest) (*subscriber, error) {
	if req == nil {
		return nil, invalidArgument("turn request must not be nil")
	}
	if req.SessionID == "" {
		return nil, invalidArgument("session id must not be empty")
	}
	if req.Origin == "" {
		return nil, invalidArgument("origin must not be empty")
	}
	if req.TurnID == "" {
		req.TurnID = newTurnID()
	}

	e.mu.Lock()
	if e.shutdown {
		e.mu.Unlock()
		return nil, codedError(ErrorCodeRuntimeOverloaded, "runtime is shutting down", nil)
	}
	if e.pending >= e.cfg.MaxPendingTurns {
		e.mu.Unlock()
		return nil, codedError(ErrorCodeRuntimeOverloaded, "pending turn queue is full", nil)
	}
	e.pending++
	sq := e.sessions[req.SessionID]
	if sq == nil {
		sq = &sessionQueue{}
		e.sessions[req.SessionID] = sq
	}
	e.mu.Unlock()

	sub := newSubscriber()
	accepted := acceptedEvent(req, 0)

	var originalTurnID string
	var created bool
	var err error
	if req.IdempotencyKey != "" {
		err = sq.lock(func() error {
			seq, err := e.nextSequence(ctx, req.SessionID)
			if err != nil {
				return err
			}
			accepted.Sequence = seq
			originalTurnID, created, err = e.dedupe.Accept(ctx, string(req.Origin), req.IdempotencyKey, time.Now().Add(dedupeWindow), &accepted)
			return err
		})
		if err != nil {
			e.releasePending()
			return nil, codedError(ErrorCodeStorageUnavailable, "dedupe claim failed", err)
		}
		if !created {
			e.releasePending()
			go e.replay(ctx, originalTurnID, sub)
			return sub, nil
		}
	} else {
		err = sq.lock(func() error {
			seq, err := e.nextSequence(ctx, req.SessionID)
			if err != nil {
				return err
			}
			accepted.Sequence = seq
			return e.events.Append(ctx, &accepted)
		})
		if err != nil {
			e.releasePending()
			return nil, codedError(ErrorCodeStorageUnavailable, "failed to persist the accepted turn", err)
		}
	}

	turnCtx, cancel := context.WithCancel(ctx)
	t := &turn{
		req:      *req,
		turnID:   req.TurnID,
		accepted: accepted,
		subs:     map[*subscriber]struct{}{sub: {}},
		cancel:   cancel,
		done:     make(chan struct{}),
	}
	t.start = func() { e.runTurn(turnCtx, t) }
	e.mu.Lock()
	// Shutdown may have begun while the accepted event was being persisted;
	// the queue is draining and a late enqueue would orphan this turn with a
	// durable accepted event and no terminal. Terminate it durably instead.
	if e.shutdown {
		e.mu.Unlock()
		e.releasePending()
		go e.terminateQueued(context.WithoutCancel(ctx), t)
		return sub, nil
	}
	sq.queue = append(sq.queue, t)
	e.turns[req.TurnID] = t
	e.mu.Unlock()

	e.schedule()
	return sub, nil
}

const dedupeWindow = 24 * time.Hour

func (e *Engine) releasePending() {
	e.mu.Lock()
	e.pending--
	e.mu.Unlock()
}

func acceptedEvent(req *TurnRequest, seq uint64) store.RuntimeEvent {
	payload := fmt.Sprintf(`{"origin":%q,"principal":%q}`, req.Origin, req.PrincipalID)
	return store.RuntimeEvent{
		ID:            newTurnID(),
		SessionID:     req.SessionID,
		Sequence:      seq,
		TurnID:        req.TurnID,
		Author:        req.PrincipalID,
		Kind:          EventKindTurnAccepted,
		SchemaVersion: 1,
		Payload:       []byte(payload),
		CreatedAt:     time.Now().UTC(),
	}
}

func (e *Engine) nextSequence(ctx context.Context, sessionID string) (uint64, error) {
	last, err := e.events.LastSequence(ctx, sessionID)
	if err != nil {
		return 0, err
	}
	return last + 1, nil
}

// sessionQueue returns the queue for a session that has at least one turn;
// submit creates it before any worker can touch the turn.
func (e *Engine) sessionQueue(sessionID string) *sessionQueue {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.sessions[sessionID]
}

// lock runs fn with the per-session allocation lock held. One writer
// allocates sequences per session, so the read-modify-append of sequence
// allocation must be atomic against concurrent accepts and the active turn's
// own event appends.
func (sq *sessionQueue) lock(fn func() error) error {
	sq.mu.Lock()
	defer sq.mu.Unlock()
	return fn()
}

// schedule starts queued turns while a session has no active turn and the
// global active limit has headroom.
func (e *Engine) schedule() {
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
		e.active--
		close(t.done)
		// The turn is terminal and its subscribers are closed; drop it so a
		// long-lived runtime does not accumulate completed turns. Replays
		// fall back to the store when the turn is not live.
		delete(e.turns, t.turnID)
		e.mu.Unlock()
		e.schedule()
	}()

	deadline := t.req.Deadline
	if deadline.IsZero() {
		deadline = time.Now().Add(e.cfg.TurnTimeout)
	}
	ctx, stop := context.WithDeadline(ctx, deadline)
	defer stop()

	// persistEvent stamps the sequence and stores the executor's event with
	// its full fidelity — invocation, branch, author, usage — then broadcasts
	// it. Executor events may be zero-valued except Kind and Payload (the
	// fake executor); those get identity filled here. An event that already
	// carries an ID (the ADK executor maps the runner's original event ID)
	// keeps it, so the stored log is the authoritative event identity.
	persistEvent := func(ctx context.Context, ev *store.RuntimeEvent) error {
		if ev.ID == "" {
			ev.ID = newTurnID()
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
		err := e.sessionQueue(t.req.SessionID).lock(func() error {
			seq, err := e.nextSequence(ctx, t.req.SessionID)
			if err != nil {
				return err
			}
			ev.Sequence = seq
			return e.events.Append(ctx, ev)
		})
		if err != nil {
			return codedError(ErrorCodeStorageUnavailable, "failed to persist "+ev.Kind, err)
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

	switch {
	case ctx.Err() == context.Canceled:
		terminalKind, terminalPayload = EventKindTurnCancelled, cancelledPayload("turn cancelled")
	case ctx.Err() == context.DeadlineExceeded:
		terminalKind, terminalPayload = EventKindTurnFailed, failedPayload(ErrorCodeTurnDeadlineExceeded, "turn deadline elapsed", nil)
	case execErr != nil:
		code, ok := CodeOf(execErr)
		if !ok {
			code = ErrorCodeRuntimeInternal
		}
		terminalKind, terminalPayload = EventKindTurnFailed, failedPayload(code, "turn execution failed", execErr)
	default:
		terminalKind, terminalPayload = EventKindTurnCompleted, completedPayload()
	}
	// The terminal event must be durable even when the turn context is
	// cancelled or expired: "a turn is terminal only after its terminal
	// event is durable" holds regardless of how the turn ended.
	if err := emit(context.WithoutCancel(ctx), terminalKind, terminalPayload); err != nil {
		e.logger.ErrorContext(ctx, "runtime failed to persist terminal event", "error", err, "turn_id", t.turnID)
	}

	for sub := range t.subs {
		close(sub.events)
	}
}

func (e *Engine) broadcast(t *turn, ev *store.RuntimeEvent) {
	for sub := range t.subs {
		select {
		case sub.events <- *ev:
		case <-sub.done:
		}
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

// replay streams a duplicate turn's stored events. When the original turn is
// still live it waits for the terminal event first, so the duplicate sees
// the full sequence.
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
		select {
		case sub.events <- store.RuntimeEvent{Kind: EventKindTurnFailed, Payload: failedPayload(ErrorCodeRuntimeInternal, "failed to replay the original turn", err)}:
		case <-sub.done:
		}
		return
	}
	for i := range events {
		select {
		case sub.events <- events[i]:
		case <-sub.done:
			return
		}
		if isTerminalKind(events[i].Kind) {
			return
		}
	}
	close(sub.events)
}

// Shutdown stops ingress, drains active turns within the grace period, then
// cancels what remains so every accepted turn reaches a durable terminal
// event.
func (e *Engine) Shutdown(ctx context.Context) error {
	e.mu.Lock()
	e.shutdown = true
	var queued []*turn
	for _, sq := range e.sessions {
		queued = append(queued, sq.queue...)
		sq.queue = nil
	}
	e.pending = 0
	e.mu.Unlock()

	for _, t := range queued {
		e.terminateQueued(context.WithoutCancel(ctx), t)
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
		// The first timer.C was consumed; reset for the post-cancel drain
		// window so shutdown stays bounded when an executor ignores
		// cancellation.
		timer.Reset(grace)
		select {
		case <-done:
			return nil
		case <-timer.C:
			return codedError(ErrorCodeRuntimeInternal, "shutdown grace elapsed with turns still draining", nil)
		}
	}
}

// terminateQueued writes a durable terminal event for a turn that never
// started, then closes its subscribers.
func (e *Engine) terminateQueued(ctx context.Context, t *turn) {
	terminal := &store.RuntimeEvent{
		ID:            newTurnID(),
		SessionID:     t.req.SessionID,
		TurnID:        t.turnID,
		Author:        t.req.PrincipalID,
		Kind:          EventKindTurnCancelled,
		SchemaVersion: 1,
		Payload:       cancelledPayload("runtime shutdown"),
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
		close(sub.events)
	}
	e.mu.Lock()
	delete(e.turns, t.turnID)
	close(t.done)
	e.mu.Unlock()
}

func completedPayload() []byte {
	return []byte(`{"outcome":"completed"}`)
}

func failedPayload(code ErrorCode, detail string, cause error) []byte {
	if cause != nil {
		detail = detail + ": " + cause.Error()
	}
	return []byte(fmt.Sprintf(`{"code":%q,"detail":%q}`, code, detail))
}

func cancelledPayload(detail string) []byte {
	return []byte(fmt.Sprintf(`{"reason":"cancelled","detail":%q}`, detail))
}

func newTurnID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return "turn_" + hex.EncodeToString(b[:])
}
