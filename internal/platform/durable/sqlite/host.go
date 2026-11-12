package sqlite

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"runtime/debug"
	"slices"
	"sync"
	"time"

	"github.com/anggasct/aura/internal/platform/durable"
)

const claimBatchSize = 64

const claimSQL = `UPDATE durable_run SET owner = ?, claim = claim + 1, updated_at = ?
WHERE key IN (
    SELECT key FROM durable_run
    WHERE state = 'running'
      AND handler IN (SELECT value FROM json_each(?))
      AND (owner IS NULL OR owner NOT IN (SELECT owner FROM durable_host WHERE expires_at > ` + dbNowMS + `))
    ORDER BY rowid
    LIMIT ?)
  AND EXISTS (SELECT 1 FROM durable_host WHERE owner = ? AND expires_at > ` + dbNowMS + `)
RETURNING key, handler, payload, claim, cancel_requested`

const endRunSQL = `UPDATE durable_run SET
    state = CASE WHEN cancel_requested = 1 THEN 'cancelled' ELSE ? END,
    detail = CASE WHEN cancel_requested = 1 THEN '' ELSE ? END,
    owner = NULL,
    updated_at = ?
WHERE key = ? AND owner = ? AND claim = ? AND state = 'running'`

var errInvocationEnded = errors.New("durable invocation ended")

type claimedRun struct {
	key             string
	handler         string
	payload         []byte
	claim           int64
	cancelRequested bool
}

type runEnd struct {
	lost   bool
	state  durable.RunState
	detail string
}

type handlerResult struct {
	err      error
	panicked bool
	detail   string
}

type execution struct {
	key      string
	owner    string
	claim    int64
	payload  []byte
	scope    func() context.Context
	cause    func() error
	done     <-chan struct{}
	cancel   context.CancelCauseFunc
	inv      *invocation
	returned chan struct{}
	finished chan struct{}
	finish   sync.Once
	pending  *runEnd
}

type host struct {
	rt    *Runtime
	wake  chan struct{}
	slots chan struct{}

	claimMu sync.Mutex
	mu      sync.Mutex
	owner   string
	execs   map[string]*execution

	leaseOK      bool
	renewedAt    time.Time
	cursorReady  bool
	signalCursor int64
	warned       map[string]struct{}
}

func (r *Runtime) Run(ctx context.Context) error {
	if !r.hosted.CompareAndSwap(false, true) {
		return &Error{Code: ErrorCodeHostRunning, Detail: "this durable runtime already hosts runs"}
	}
	h := &host{
		rt:     r,
		wake:   make(chan struct{}, 1),
		slots:  make(chan struct{}, r.recoveryConcurrency),
		owner:  r.hostID,
		execs:  map[string]*execution{},
		warned: map[string]struct{}{},
	}
	if err := h.writeLease(ctx); err != nil {
		return err
	}
	r.setHost(h)
	defer r.setHost(nil)
	h.serve(ctx)
	return h.stop(ctx)
}

func (h *host) nudge() {
	select {
	case h.wake <- struct{}{}:
	default:
	}
}

func (h *host) currentOwner() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.owner
}

func (h *host) execution(key string) *execution {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.execs[key]
}

func (h *host) snapshot() []*execution {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Collect(maps.Values(h.execs))
}

func (h *host) cancelLocal(key, owner string, claim int64) *execution {
	h.claimMu.Lock()
	defer h.claimMu.Unlock()
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.owner != owner {
		return nil
	}
	exec := h.execs[key]
	if exec == nil || exec.claim != claim {
		return nil
	}
	exec.cancel(durable.ErrRunCancelled)
	return exec
}

func (h *host) logFailure(ctx context.Context, step string, err error) {
	if ctx.Err() != nil {
		return
	}
	h.rt.logger.ErrorContext(ctx, "durable host step failed", "step", step, "error", err)
}

func (h *host) writeLease(ctx context.Context) error {
	wctx, cancel := detached(ctx)
	defer cancel()
	owner := h.currentOwner()
	if _, err := h.rt.db.ExecContext(wctx, `INSERT INTO durable_host (owner, expires_at, created_at) VALUES (?, `+dbNowMS+` + ?, ?)`,
		owner, h.rt.hostTTL.Milliseconds(), h.rt.stamp()); err != nil {
		return fmt.Errorf("durable host lease: %w", err)
	}
	h.leaseOK = true
	h.renewedAt = time.Now()
	return nil
}

func (h *host) serve(ctx context.Context) {
	poll := time.NewTicker(h.rt.pollInterval)
	defer poll.Stop()
	renew := time.NewTicker(h.rt.hostTTL / 3)
	defer renew.Stop()
	h.pass(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-poll.C:
			h.pass(ctx)
		case <-h.wake:
			h.pass(ctx)
		case <-renew.C:
			h.renew(ctx)
		}
	}
}

func (h *host) pass(ctx context.Context) {
	if !h.leaseOK || ctx.Err() != nil {
		return
	}
	if err := h.deleteExpiredHosts(ctx); err != nil {
		h.logFailure(ctx, "delete_expired_hosts", err)
	}
	h.retryPendingEnds(ctx)
	if !h.cursorReady {
		if err := h.initSignalCursor(ctx); err != nil {
			h.logFailure(ctx, "signal_cursor", err)
			return
		}
	}
	if err := h.claimAll(ctx); err != nil {
		h.logFailure(ctx, "claim", err)
	}
	if err := h.applyCancelRequests(ctx); err != nil {
		h.logFailure(ctx, "cancel_requests", err)
	}
	if err := h.wakeSignalled(ctx); err != nil {
		h.logFailure(ctx, "signals", err)
	}
	if err := h.warnUnclaimable(ctx); err != nil {
		h.logFailure(ctx, "unclaimable", err)
	}
}

func (h *host) deleteExpiredHosts(ctx context.Context) error {
	wctx, cancel := detached(ctx)
	defer cancel()
	if _, err := h.rt.db.ExecContext(wctx, `DELETE FROM durable_host WHERE expires_at <= `+dbNowMS); err != nil {
		return fmt.Errorf("delete expired hosts: %w", err)
	}
	return nil
}

func (h *host) initSignalCursor(ctx context.Context) error {
	var cursor int64
	if err := h.rt.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM durable_signal`).Scan(&cursor); err != nil {
		return fmt.Errorf("read signal cursor: %w", err)
	}
	h.signalCursor = cursor
	h.cursorReady = true
	return nil
}

func (h *host) claimAll(ctx context.Context) error {
	handlers := h.rt.registeredHandlers()
	if len(handlers) == 0 {
		return nil
	}
	names, err := json.Marshal(sortedNames(handlers))
	if err != nil {
		return fmt.Errorf("encode handler names: %w", err)
	}
	for {
		claimed, err := h.claimBatch(ctx, string(names), handlers)
		if err != nil {
			return err
		}
		if claimed < claimBatchSize {
			return nil
		}
	}
}

func (h *host) claimBatch(ctx context.Context, names string, handlers map[string]durable.Handler) (int, error) {
	h.claimMu.Lock()
	defer h.claimMu.Unlock()
	owner := h.currentOwner()
	runs, err := h.claimRows(ctx, owner, names)
	if err != nil {
		return 0, err
	}
	for _, run := range runs {
		h.startExecution(ctx, &run, handlers[run.handler])
	}
	return len(runs), nil
}

func (h *host) claimRows(ctx context.Context, owner, names string) ([]claimedRun, error) {
	wctx, cancel := detached(ctx)
	defer cancel()
	rows, err := h.rt.db.QueryContext(wctx, claimSQL, owner, h.rt.stamp(), names, claimBatchSize, owner)
	if err != nil {
		return nil, fmt.Errorf("claim runs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var runs []claimedRun
	for rows.Next() {
		var run claimedRun
		if err := rows.Scan(&run.key, &run.handler, &run.payload, &run.claim, &run.cancelRequested); err != nil {
			return nil, fmt.Errorf("claim runs: scan: %w", err)
		}
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("claim runs: %w", err)
	}
	return runs, nil
}

func (h *host) startExecution(ctx context.Context, run *claimedRun, fn durable.Handler) {
	base, cancel := context.WithCancelCause(context.WithoutCancel(ctx))
	exec := &execution{
		key:      run.key,
		owner:    h.currentOwner(),
		claim:    run.claim,
		payload:  bytesOrEmpty(run.payload),
		scope:    func() context.Context { return base },
		cause:    func() error { return context.Cause(base) },
		done:     base.Done(),
		cancel:   cancel,
		returned: make(chan struct{}),
		finished: make(chan struct{}),
	}
	exec.inv = newInvocation(h.rt, exec)
	if run.cancelRequested {
		cancel(durable.ErrRunCancelled)
	}
	h.mu.Lock()
	h.execs[run.key] = exec
	h.mu.Unlock()
	h.rt.counters.claimed.Add(1)
	go h.execute(base, exec, fn)
}

func (h *host) execute(ctx context.Context, exec *execution, fn durable.Handler) {
	defer close(exec.returned)
	end := h.invoke(ctx, exec, fn)
	exec.cancel(errInvocationEnded)
	exec.inv.close()
	h.settle(ctx, exec, &end)
}

func (h *host) invoke(ctx context.Context, exec *execution, fn durable.Handler) runEnd {
	inv := exec.inv
	if !inv.retry(ctx, inv.load) {
		return endWithoutHandler(ctx)
	}
	if inv.isReplay() && !h.acquireSlot(ctx, inv) {
		return endWithoutHandler(ctx)
	}
	if ctx.Err() != nil {
		inv.releaseSlot()
		return endWithoutHandler(ctx)
	}
	result := h.callHandler(ctx, exec, fn)
	inv.releaseSlot()
	return endAfterHandler(ctx, &result)
}

func endWithoutHandler(ctx context.Context) runEnd {
	cause := context.Cause(ctx)
	switch {
	case errors.Is(cause, durable.ErrOwnershipLost):
		return runEnd{lost: true}
	case errors.Is(cause, durable.ErrRunCancelled):
		return runEnd{state: durable.RunCancelled}
	default:
		return runEnd{state: durable.RunRunning}
	}
}

func endAfterHandler(ctx context.Context, result *handlerResult) runEnd {
	cause := context.Cause(ctx)
	switch {
	case errors.Is(cause, durable.ErrOwnershipLost):
		return runEnd{lost: true}
	case errors.Is(cause, durable.ErrRunCancelled):
		return runEnd{state: durable.RunCancelled}
	case result.err == nil && !result.panicked:
		return runEnd{state: durable.RunSucceeded}
	case errors.Is(cause, durable.ErrHostStopping):
		return runEnd{state: durable.RunRunning}
	case result.panicked:
		return runEnd{state: durable.RunFailed, detail: result.detail}
	default:
		return runEnd{state: durable.RunFailed, detail: result.err.Error()}
	}
}

func (h *host) acquireSlot(ctx context.Context, inv *invocation) bool {
	h.rt.counters.waiting.Add(1)
	defer h.rt.counters.waiting.Add(-1)
	select {
	case h.slots <- struct{}{}:
		h.rt.counters.replaying.Add(1)
		inv.release = sync.OnceFunc(func() {
			<-h.slots
			h.rt.counters.replaying.Add(-1)
		})
		return true
	case <-ctx.Done():
		return false
	}
}

func (h *host) callHandler(ctx context.Context, exec *execution, fn durable.Handler) (result handlerResult) {
	defer func() {
		if value := recover(); value != nil {
			h.rt.logger.ErrorContext(ctx, "durable handler panicked", "run_key", exec.key, "panic", fmt.Sprint(value), "stack", string(debug.Stack()))
			result = handlerResult{panicked: true, detail: fmt.Sprintf("panic: %v", value)}
		}
	}()
	return handlerResult{err: fn(ctx, exec.inv)}
}

func (h *host) settle(ctx context.Context, exec *execution, end *runEnd) {
	if end.lost {
		h.release(exec)
		return
	}
	written, err := h.writeEnd(ctx, exec, end)
	if err != nil {
		h.rt.logger.ErrorContext(ctx, "durable run end not written", "run_key", exec.key, "error", err)
		h.mu.Lock()
		exec.pending = end
		h.mu.Unlock()
		return
	}
	if !written {
		h.rt.logger.WarnContext(ctx, "durable run end fenced", "run_key", exec.key)
	}
	h.release(exec)
}

func (h *host) writeEnd(ctx context.Context, exec *execution, end *runEnd) (bool, error) {
	wctx, cancel := detached(ctx)
	defer cancel()
	res, err := h.rt.db.ExecContext(wctx, endRunSQL, string(end.state), end.detail, h.rt.stamp(), exec.key, exec.owner, exec.claim)
	if err != nil {
		return false, fmt.Errorf("write run end: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("write run end: %w", err)
	}
	return n == 1, nil
}

func (h *host) release(exec *execution) {
	exec.finish.Do(func() {
		h.mu.Lock()
		if h.execs[exec.key] == exec {
			delete(h.execs, exec.key)
		}
		h.mu.Unlock()
		h.rt.counters.claimed.Add(-1)
		h.rt.counters.ended.Add(1)
		close(exec.finished)
	})
}

func (h *host) retryPendingEnds(ctx context.Context) {
	h.mu.Lock()
	var pending []*execution
	for _, exec := range h.execs {
		if exec.pending != nil {
			pending = append(pending, exec)
		}
	}
	h.mu.Unlock()
	for _, exec := range pending {
		h.mu.Lock()
		end := exec.pending
		h.mu.Unlock()
		if _, err := h.writeEnd(ctx, exec, end); err != nil {
			h.logFailure(ctx, "run_end", err)
			continue
		}
		h.release(exec)
	}
}

func (h *host) applyCancelRequests(ctx context.Context) error {
	keys, err := h.queryKeys(ctx, `SELECT key FROM durable_run WHERE owner = ? AND state = 'running' AND cancel_requested = 1`, h.currentOwner())
	if err != nil {
		return fmt.Errorf("read cancel requests: %w", err)
	}
	for _, key := range keys {
		if exec := h.execution(key); exec != nil {
			exec.cancel(durable.ErrRunCancelled)
		}
	}
	return nil
}

func (h *host) queryKeys(ctx context.Context, query string, args ...any) ([]string, error) {
	rows, err := h.rt.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return keys, nil
}

func (h *host) wakeSignalled(ctx context.Context) error {
	rows, err := h.rt.db.QueryContext(ctx, `SELECT run_key, MAX(id) FROM durable_signal WHERE id > ? GROUP BY run_key`, h.signalCursor)
	if err != nil {
		return fmt.Errorf("read new signals: %w", err)
	}
	defer func() { _ = rows.Close() }()
	cursor := h.signalCursor
	var keys []string
	for rows.Next() {
		var key string
		var id int64
		if err := rows.Scan(&key, &id); err != nil {
			return fmt.Errorf("read new signals: scan: %w", err)
		}
		keys = append(keys, key)
		cursor = max(cursor, id)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read new signals: %w", err)
	}
	h.signalCursor = cursor
	for _, key := range keys {
		if exec := h.execution(key); exec != nil {
			exec.inv.wakeAll()
		}
	}
	return nil
}

func (h *host) warnUnclaimable(ctx context.Context) error {
	names, err := json.Marshal(sortedNames(h.rt.registeredHandlers()))
	if err != nil {
		return fmt.Errorf("encode handler names: %w", err)
	}
	rows, err := h.rt.db.QueryContext(ctx, `SELECT key, handler FROM durable_run
WHERE state = 'running'
  AND handler NOT IN (SELECT value FROM json_each(?))
  AND (owner IS NULL OR owner NOT IN (SELECT owner FROM durable_host WHERE expires_at > `+dbNowMS+`))`, string(names))
	if err != nil {
		return fmt.Errorf("read unclaimable runs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	seen := map[string]struct{}{}
	for rows.Next() {
		var key, handler string
		if err := rows.Scan(&key, &handler); err != nil {
			return fmt.Errorf("read unclaimable runs: scan: %w", err)
		}
		seen[key] = struct{}{}
		if _, warned := h.warned[key]; !warned {
			h.rt.logger.WarnContext(ctx, "durable run has no handler on this host", "run_key", key, "handler", handler)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read unclaimable runs: %w", err)
	}
	h.warned = seen
	return nil
}

func (h *host) renew(ctx context.Context) {
	if !h.leaseOK {
		h.restoreLease(ctx)
		return
	}
	wctx, cancel := detached(ctx)
	defer cancel()
	res, err := h.rt.db.ExecContext(wctx, `UPDATE durable_host SET expires_at = `+dbNowMS+` + ? WHERE owner = ? AND expires_at > `+dbNowMS,
		h.rt.hostTTL.Milliseconds(), h.currentOwner())
	var renewed int64
	if err == nil {
		renewed, err = res.RowsAffected()
	}
	switch {
	case err != nil:
		h.logFailure(ctx, "renew_lease", err)
		if time.Since(h.renewedAt) >= h.rt.hostTTL {
			h.loseLease(ctx)
		}
	case renewed == 0:
		h.loseLease(ctx)
	default:
		h.renewedAt = time.Now()
		h.rt.counters.renewals.Add(1)
	}
}

func (h *host) loseLease(ctx context.Context) {
	h.rt.logger.ErrorContext(ctx, "durable host lease lost")
	h.leaseOK = false
	execs := h.snapshot()
	for _, exec := range execs {
		exec.cancel(durable.ErrOwnershipLost)
	}
	for _, exec := range execs {
		select {
		case <-exec.returned:
		case <-ctx.Done():
			return
		}
	}
	for _, exec := range execs {
		h.release(exec)
	}
	h.restoreLease(ctx)
}

func (h *host) restoreLease(ctx context.Context) {
	wctx, cancel := detached(ctx)
	_, err := h.rt.db.ExecContext(wctx, `DELETE FROM durable_host WHERE owner = ?`, h.currentOwner())
	cancel()
	if err != nil {
		h.logFailure(ctx, "drop_lease", err)
		return
	}
	h.mu.Lock()
	h.owner = rand.Text()
	h.mu.Unlock()
	if err := h.writeLease(ctx); err != nil {
		h.logFailure(ctx, "restore_lease", err)
	}
}

func (h *host) stop(ctx context.Context) error {
	execs := h.snapshot()
	for _, exec := range execs {
		exec.cancel(durable.ErrHostStopping)
	}
	returned := waitReturned(execs, h.rt.stopGrace)
	h.retryPendingEnds(context.WithoutCancel(ctx))
	wctx, cancel := detached(ctx)
	defer cancel()
	owner := h.currentOwner()
	var failures []error
	if _, err := h.rt.db.ExecContext(wctx, `UPDATE durable_run SET owner = NULL, updated_at = ? WHERE owner = ? AND state = 'running'`, h.rt.stamp(), owner); err != nil {
		failures = append(failures, fmt.Errorf("durable host stop: release runs: %w", err))
	}
	if _, err := h.rt.db.ExecContext(wctx, `DELETE FROM durable_host WHERE owner = ?`, owner); err != nil {
		failures = append(failures, fmt.Errorf("durable host stop: delete lease: %w", err))
	}
	if !returned {
		failures = append(failures, &Error{Code: ErrorCodeStopTimeout, Detail: "durable handlers did not return within the stop grace"})
	}
	return errors.Join(failures...)
}

func waitReturned(execs []*execution, grace time.Duration) bool {
	timer := time.NewTimer(grace)
	defer timer.Stop()
	for _, exec := range execs {
		select {
		case <-exec.returned:
		case <-timer.C:
			return false
		}
	}
	return true
}
