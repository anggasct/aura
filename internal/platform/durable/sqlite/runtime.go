package sqlite

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/anggasct/aura/internal/platform/durable"
)

const (
	defaultPollInterval        = 500 * time.Millisecond
	defaultHostTTL             = 10 * time.Second
	defaultStopGrace           = 10 * time.Second
	defaultRecoveryConcurrency = 8
	defaultClockCheck          = time.Minute
	writeTimeout               = 5 * time.Second
)

var (
	_ durable.Runtime          = (*Runtime)(nil)
	_ durable.HandlerRegistrar = (*Runtime)(nil)
)

type Options struct {
	DB                  *sql.DB
	Logger              *slog.Logger
	Clock               durable.Clock
	PollInterval        time.Duration
	HostTTL             time.Duration
	StopGrace           time.Duration
	RecoveryConcurrency int
	ClockCheck          time.Duration
}

type callKey struct {
	service string
	handler string
}

type counters struct {
	claimed   atomic.Int64
	replaying atomic.Int64
	waiting   atomic.Int64
	parked    atomic.Int64
	renewals  atomic.Int64
	ended     atomic.Int64
}

type hostStats struct {
	claimed   int64
	replaying int64
	waiting   int64
	parked    int64
	renewals  int64
	ended     int64
}

type Runtime struct {
	db                  *sql.DB
	logger              *slog.Logger
	clock               durable.Clock
	pollInterval        time.Duration
	hostTTL             time.Duration
	stopGrace           time.Duration
	recoveryConcurrency int
	clockCheck          time.Duration
	hostID              string

	hosted   atomic.Bool
	counters counters

	mu       sync.Mutex
	handlers map[string]durable.Handler
	declared map[string]struct{}
	calls    map[callKey]durable.CallHandler
	host     *host
}

func New(opts *Options) (*Runtime, error) {
	if opts == nil {
		return nil, invalidArgument("durable runtime options are required")
	}
	if err := validateOptions(opts); err != nil {
		return nil, err
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	clock := opts.Clock
	if clock == nil {
		clock = durable.RealClock()
	}
	return &Runtime{
		db:                  opts.DB,
		logger:              logger.With("component", "durable"),
		clock:               clock,
		pollInterval:        orDefault(opts.PollInterval, defaultPollInterval),
		hostTTL:             orDefault(opts.HostTTL, defaultHostTTL),
		stopGrace:           orDefault(opts.StopGrace, defaultStopGrace),
		recoveryConcurrency: orDefault(opts.RecoveryConcurrency, defaultRecoveryConcurrency),
		clockCheck:          orDefault(opts.ClockCheck, defaultClockCheck),
		hostID:              rand.Text(),
		handlers:            map[string]durable.Handler{},
		declared:            map[string]struct{}{},
		calls:               map[callKey]durable.CallHandler{},
	}, nil
}

func validateOptions(opts *Options) error {
	var problems []error
	if opts.DB == nil {
		problems = append(problems, invalidArgument("durable runtime requires a database"))
	}
	durations := []struct {
		name  string
		value time.Duration
	}{
		{"poll interval", opts.PollInterval},
		{"host ttl", opts.HostTTL},
		{"stop grace", opts.StopGrace},
		{"clock check", opts.ClockCheck},
	}
	for _, d := range durations {
		switch {
		case d.value < 0:
			problems = append(problems, invalidArgument("durable "+d.name+" must not be negative"))
		case d.value > 0 && d.value < time.Millisecond:
			problems = append(problems, invalidArgument("durable "+d.name+" must be at least one millisecond"))
		}
	}
	if opts.RecoveryConcurrency < 0 {
		problems = append(problems, invalidArgument("durable recovery concurrency must not be negative"))
	}
	return errors.Join(problems...)
}

func orDefault[T time.Duration | int](value, fallback T) T {
	if value == 0 {
		return fallback
	}
	return value
}

func (r *Runtime) RegisterHandler(name string, fn durable.Handler) {
	if name == "" || fn == nil {
		r.logger.ErrorContext(context.Background(), "durable handler registration ignored", "handler", name)
		return
	}
	r.mu.Lock()
	r.handlers[name] = fn
	h := r.host
	r.mu.Unlock()
	if h != nil {
		h.nudge()
	}
}

func (r *Runtime) DeclareHandlers(names ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, name := range names {
		if name == "" {
			r.logger.ErrorContext(context.Background(), "durable handler declaration ignored", "handler", name)
			continue
		}
		r.declared[name] = struct{}{}
	}
}

func (r *Runtime) RegisterCall(service, handler string, fn durable.CallHandler) {
	if service == "" || handler == "" || fn == nil {
		r.logger.ErrorContext(context.Background(), "durable call registration ignored", "service", service, "handler", handler)
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls[callKey{service: service, handler: handler}] = fn
}

func (r *Runtime) knowsHandler(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.handlers[name]; ok {
		return true
	}
	_, ok := r.declared[name]
	return ok
}

func (r *Runtime) registeredHandlers() map[string]durable.Handler {
	r.mu.Lock()
	defer r.mu.Unlock()
	return maps.Clone(r.handlers)
}

func (r *Runtime) currentHost() *host {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.host
}

func (r *Runtime) setHost(h *host) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.host = h
}

func (r *Runtime) stamp() string {
	return r.clock.Now().UTC().Format(time.RFC3339Nano)
}

func (r *Runtime) stats() hostStats {
	return hostStats{
		claimed:   r.counters.claimed.Load(),
		replaying: r.counters.replaying.Load(),
		waiting:   r.counters.waiting.Load(),
		parked:    r.counters.parked.Load(),
		renewals:  r.counters.renewals.Load(),
		ended:     r.counters.ended.Load(),
	}
}

func (r *Runtime) Start(ctx context.Context, req durable.StartRequest) (durable.RunRef, error) {
	if req.Key == "" {
		return durable.RunRef{}, invalidArgument("durable start requires a key")
	}
	if req.Handler == "" {
		return durable.RunRef{}, invalidArgument("durable start requires a handler")
	}
	ref := durable.RunRef{Key: req.Key}
	if req.Reuse == durable.ReuseEnded {
		return r.startReusingEnded(ctx, &req)
	}
	if !r.knowsHandler(req.Handler) {
		exists, err := runExists(ctx, r.db, req.Key)
		if err != nil {
			return durable.RunRef{}, fmt.Errorf("durable start: %w", err)
		}
		if exists {
			return ref, nil
		}
		return durable.RunRef{}, handlerNotRegistered(req.Handler)
	}
	stamp := r.stamp()
	if _, err := r.db.ExecContext(ctx, `INSERT INTO durable_run (key, handler, payload, state, created_at, updated_at)
VALUES (?, ?, ?, 'running', ?, ?) ON CONFLICT (key) DO NOTHING`, req.Key, req.Handler, bytesOrEmpty(req.Payload), stamp, stamp); err != nil {
		return durable.RunRef{}, fmt.Errorf("durable start: %w", err)
	}
	if h := r.currentHost(); h != nil {
		h.nudge()
	}
	return ref, nil
}

func (r *Runtime) startReusingEnded(ctx context.Context, req *durable.StartRequest) (durable.RunRef, error) {
	if !r.knowsHandler(req.Handler) {
		return durable.RunRef{}, handlerNotRegistered(req.Handler)
	}
	if err := r.inTx(ctx, "durable start", func(tx *sql.Tx) error { return r.replaceEnded(ctx, tx, req) }); err != nil {
		return durable.RunRef{}, err
	}
	if h := r.currentHost(); h != nil {
		h.nudge()
	}
	return durable.RunRef{Key: req.Key}, nil
}

func (r *Runtime) replaceEnded(ctx context.Context, tx *sql.Tx, req *durable.StartRequest) error {
	var handler, state string
	var claim int64
	err := tx.QueryRowContext(ctx, `SELECT handler, state, claim FROM durable_run WHERE key = ?`, req.Key).Scan(&handler, &state, &claim)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return fmt.Errorf("durable start: read run: %w", err)
	case handler != req.Handler:
		return keyInUse(req.Key)
	case durable.RunState(state) == durable.RunRunning:
		return nil
	default:
		if _, err := tx.ExecContext(ctx, `DELETE FROM durable_run WHERE key = ?`, req.Key); err != nil {
			return fmt.Errorf("durable start: delete ended run: %w", err)
		}
	}
	stamp := r.stamp()
	if _, err := tx.ExecContext(ctx, `INSERT INTO durable_run (key, handler, payload, state, claim, created_at, updated_at)
VALUES (?, ?, ?, 'running', ?, ?, ?)`, req.Key, req.Handler, bytesOrEmpty(req.Payload), claim, stamp, stamp); err != nil {
		return fmt.Errorf("durable start: insert run: %w", err)
	}
	return nil
}

func (r *Runtime) Signal(ctx context.Context, run durable.RunRef, name string, payload []byte) error {
	if name == "" {
		return invalidArgument("durable signal requires a name")
	}
	err := r.inTx(ctx, "durable signal", func(tx *sql.Tx) error {
		if err := requireRunning(ctx, tx, run.Key); err != nil {
			return err
		}
		return appendSignal(ctx, tx, &signalRow{runKey: run.Key, name: name, payload: payload, stamp: r.stamp()})
	})
	if err != nil {
		return err
	}
	r.notifySignal(run.Key)
	return nil
}

func (r *Runtime) notifySignal(key string) {
	h := r.currentHost()
	if h == nil {
		return
	}
	if exec := h.execution(key); exec != nil {
		exec.inv.wakeAll()
	}
	h.nudge()
}

func (r *Runtime) Cancel(ctx context.Context, run durable.RunRef) error {
	var liveOwner string
	var liveClaim int64
	err := r.inTx(ctx, "durable cancel", func(tx *sql.Tx) error {
		var state string
		var owner sql.NullString
		var claim int64
		err := tx.QueryRowContext(ctx, `SELECT state, owner, claim FROM durable_run WHERE key = ?`, run.Key).Scan(&state, &owner, &claim)
		if errors.Is(err, sql.ErrNoRows) {
			return unknownRun(run.Key)
		}
		if err != nil {
			return fmt.Errorf("durable cancel: read run: %w", err)
		}
		if durable.RunState(state) != durable.RunRunning {
			return nil
		}
		live := false
		if owner.Valid {
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM durable_host WHERE owner = ? AND expires_at > `+dbNowMS+`)`, owner.String).Scan(&live); err != nil {
				return fmt.Errorf("durable cancel: read host: %w", err)
			}
		}
		if !live {
			if _, err := tx.ExecContext(ctx, `UPDATE durable_run SET state = 'cancelled', detail = '', owner = NULL, cancel_requested = 1, updated_at = ? WHERE key = ?`, r.stamp(), run.Key); err != nil {
				return fmt.Errorf("durable cancel: write run: %w", err)
			}
			return nil
		}
		if _, err := tx.ExecContext(ctx, `UPDATE durable_run SET cancel_requested = 1, updated_at = ? WHERE key = ?`, r.stamp(), run.Key); err != nil {
			return fmt.Errorf("durable cancel: request: %w", err)
		}
		liveOwner = owner.String
		liveClaim = claim
		return nil
	})
	if err != nil || liveOwner == "" {
		return err
	}
	h := r.currentHost()
	if h == nil {
		return nil
	}
	exec := h.cancelLocal(run.Key, liveOwner, liveClaim)
	h.nudge()
	if exec == nil {
		return nil
	}
	select {
	case <-exec.finished:
	case <-ctx.Done():
	}
	return nil
}

func (r *Runtime) Status(ctx context.Context, run durable.RunRef) (durable.RunStatus, error) {
	var state, detail string
	err := r.db.QueryRowContext(ctx, `SELECT state, detail FROM durable_run WHERE key = ?`, run.Key).Scan(&state, &detail)
	if errors.Is(err, sql.ErrNoRows) {
		return durable.RunStatus{}, unknownRun(run.Key)
	}
	if err != nil {
		return durable.RunStatus{}, fmt.Errorf("durable status: %w", err)
	}
	return durable.RunStatus{State: durable.RunState(state), Detail: detail}, nil
}

func (r *Runtime) Call(ctx context.Context, req durable.CallRequest) ([]byte, error) {
	if req.Service == "" || req.Key == "" || req.Handler == "" {
		return nil, invalidArgument("durable call requires a service, a key and a handler")
	}
	r.mu.Lock()
	fn := r.calls[callKey{service: req.Service, handler: req.Handler}]
	r.mu.Unlock()
	if fn == nil {
		return nil, &Error{Code: ErrorCodeCallNotRegistered, Detail: "no durable call registered for " + req.Handler + " on " + req.Service}
	}
	return fn(ctx, req.Key, req.Payload)
}

func (r *Runtime) ResolveApproval(ctx context.Context, req durable.ResolveApprovalRequest) error {
	if req.ApprovalID == "" {
		return invalidArgument("durable approval requires an id")
	}
	runKey, signal, err := durable.ParseApprovalAddr(req.ApprovalID)
	if err != nil {
		return invalidArgument(err.Error())
	}
	err = r.inTx(ctx, "durable resolve approval", func(tx *sql.Tx) error {
		var consumed bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM durable_approval WHERE approval_id = ?)`, req.ApprovalID).Scan(&consumed); err != nil {
			return fmt.Errorf("durable resolve approval: read approval: %w", err)
		}
		if consumed {
			return &Error{Code: ErrorCodeApprovalConsumed, Detail: "durable approval was already resolved"}
		}
		if err := requireRunning(ctx, tx, runKey); err != nil {
			return err
		}
		stamp := r.stamp()
		if _, err := tx.ExecContext(ctx, `INSERT INTO durable_approval (approval_id, run_key, consumed_at) VALUES (?, ?, ?)`, req.ApprovalID, runKey, stamp); err != nil {
			return fmt.Errorf("durable resolve approval: write approval: %w", err)
		}
		return appendSignal(ctx, tx, &signalRow{runKey: runKey, name: signal, payload: req.Payload, stamp: stamp})
	})
	if err != nil {
		return err
	}
	r.notifySignal(runKey)
	return nil
}

func (r *Runtime) TurnDeadline(ctx context.Context, sessionID string) (time.Time, bool, error) {
	if sessionID == "" {
		return time.Time{}, false, invalidArgument("durable turn deadline requires a session id")
	}
	var deadline sql.NullString
	err := r.db.QueryRowContext(ctx, `SELECT deadline FROM durable_session_turn WHERE session_id = ? AND state = 'active'`, sessionID).Scan(&deadline)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("durable turn deadline: %w", err)
	}
	if !deadline.Valid {
		return time.Time{}, false, nil
	}
	at, err := time.Parse(time.RFC3339Nano, deadline.String)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("durable turn deadline: parse: %w", err)
	}
	return at, true, nil
}

func sortedNames(handlers map[string]durable.Handler) []string {
	names := slices.AppendSeq(make([]string, 0, len(handlers)), maps.Keys(handlers))
	slices.Sort(names)
	return names
}
