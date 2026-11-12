package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/anggasct/aura/internal/platform/durable"
)

const (
	actionResult  = "result"
	actionFailed  = "failed"
	waitPending   = "pending"
	waitSignalled = "signal"
	waitTimedOut  = "timeout"
)

type actionRecord struct {
	failed bool
	result []byte
	detail string
}

func (a actionRecord) answer() ([]byte, error) {
	if a.failed {
		return nil, &Error{Code: ErrorCodeActionFailed, Detail: a.detail}
	}
	return copyBytes(a.result), nil
}

type waitKey struct {
	name string
	seq  int64
}

type waitRecord struct {
	deadline time.Time
	outcome  string
}

type waitDecision struct {
	outcome string
	payload []byte
}

type decideInput struct {
	position int64
	reached  bool
}

type invocation struct {
	rt      *Runtime
	exec    *execution
	release func()

	mu        sync.Mutex
	closed    bool
	actions   map[string]actionRecord
	waits     map[waitKey]waitRecord
	timers    map[int64]time.Time
	nextTimer int64
	nextWait  map[string]int64
	positions map[string]int64
	consumers map[string]chan struct{}
	wakeup    chan struct{}
	timerWG   sync.WaitGroup
}

func newInvocation(rt *Runtime, exec *execution) *invocation {
	return &invocation{
		rt:        rt,
		exec:      exec,
		release:   func() {},
		actions:   map[string]actionRecord{},
		waits:     map[waitKey]waitRecord{},
		timers:    map[int64]time.Time{},
		nextWait:  map[string]int64{},
		positions: map[string]int64{},
		consumers: map[string]chan struct{}{},
		wakeup:    make(chan struct{}),
	}
}

func (inv *invocation) Run() durable.RunRef {
	return durable.RunRef{Key: inv.exec.key}
}

func (inv *invocation) Payload() []byte {
	return copyBytes(inv.exec.payload)
}

func (inv *invocation) done() <-chan struct{} {
	return inv.exec.done
}

func (inv *invocation) ended() bool {
	return inv.exec.cause() != nil
}

func (inv *invocation) lost() bool {
	return errors.Is(inv.exec.cause(), durable.ErrOwnershipLost)
}

func (inv *invocation) lose() {
	inv.exec.cancel(durable.ErrOwnershipLost)
}

func (inv *invocation) releaseSlot() {
	inv.release()
}

func (inv *invocation) close() {
	inv.mu.Lock()
	inv.closed = true
	inv.mu.Unlock()
	inv.timerWG.Wait()
}

func (inv *invocation) wakeAll() {
	inv.mu.Lock()
	defer inv.mu.Unlock()
	close(inv.wakeup)
	inv.wakeup = make(chan struct{})
}

func (inv *invocation) wakeChannel() <-chan struct{} {
	inv.mu.Lock()
	defer inv.mu.Unlock()
	return inv.wakeup
}

func (inv *invocation) isReplay() bool {
	inv.mu.Lock()
	defer inv.mu.Unlock()
	return len(inv.actions)+len(inv.waits)+len(inv.timers) > 0
}

func (inv *invocation) retry(ctx context.Context, op func(ctx context.Context) error) bool {
	for {
		err := op(ctx)
		if err == nil {
			return true
		}
		if errors.Is(err, errNotOwned) {
			inv.lose()
			return false
		}
		if ctx.Err() != nil || inv.ended() {
			return false
		}
		inv.rt.logger.ErrorContext(ctx, "durable journal access failed", "run_key", inv.exec.key, "error", err)
		timer := time.NewTimer(inv.rt.pollInterval)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return false
		case <-inv.done():
			timer.Stop()
			return false
		}
	}
}

func (inv *invocation) load(ctx context.Context) error {
	ctx, cancel := detached(ctx)
	defer cancel()
	actions, err := inv.loadActions(ctx)
	if err != nil {
		return err
	}
	waits, err := inv.loadWaits(ctx)
	if err != nil {
		return err
	}
	timers, err := inv.loadTimers(ctx)
	if err != nil {
		return err
	}
	inv.mu.Lock()
	defer inv.mu.Unlock()
	inv.actions, inv.waits, inv.timers = actions, waits, timers
	return nil
}

func (inv *invocation) loadActions(ctx context.Context) (map[string]actionRecord, error) {
	rows, err := inv.rt.db.QueryContext(ctx, `SELECT action_key, outcome, result, detail FROM durable_action WHERE run_key = ?`, inv.exec.key)
	if err != nil {
		return nil, fmt.Errorf("load actions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	actions := map[string]actionRecord{}
	for rows.Next() {
		var key, outcome, detail string
		var result []byte
		if err := rows.Scan(&key, &outcome, &result, &detail); err != nil {
			return nil, fmt.Errorf("load actions: scan: %w", err)
		}
		actions[key] = actionRecord{failed: outcome == actionFailed, result: copyBytes(result), detail: detail}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load actions: %w", err)
	}
	return actions, nil
}

func (inv *invocation) loadWaits(ctx context.Context) (map[waitKey]waitRecord, error) {
	rows, err := inv.rt.db.QueryContext(ctx, `SELECT name, seq, deadline, outcome FROM durable_wait WHERE run_key = ?`, inv.exec.key)
	if err != nil {
		return nil, fmt.Errorf("load waits: %w", err)
	}
	defer func() { _ = rows.Close() }()
	waits := map[waitKey]waitRecord{}
	for rows.Next() {
		var key waitKey
		var deadline, outcome string
		if err := rows.Scan(&key.name, &key.seq, &deadline, &outcome); err != nil {
			return nil, fmt.Errorf("load waits: scan: %w", err)
		}
		at, err := time.Parse(time.RFC3339Nano, deadline)
		if err != nil {
			return nil, fmt.Errorf("load waits: deadline: %w", err)
		}
		waits[key] = waitRecord{deadline: at, outcome: outcome}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load waits: %w", err)
	}
	return waits, nil
}

func (inv *invocation) loadTimers(ctx context.Context) (map[int64]time.Time, error) {
	rows, err := inv.rt.db.QueryContext(ctx, `SELECT seq, fire_at FROM durable_timer WHERE run_key = ?`, inv.exec.key)
	if err != nil {
		return nil, fmt.Errorf("load timers: %w", err)
	}
	defer func() { _ = rows.Close() }()
	timers := map[int64]time.Time{}
	for rows.Next() {
		var seq int64
		var fireAt string
		if err := rows.Scan(&seq, &fireAt); err != nil {
			return nil, fmt.Errorf("load timers: scan: %w", err)
		}
		at, err := time.Parse(time.RFC3339Nano, fireAt)
		if err != nil {
			return nil, fmt.Errorf("load timers: fire time: %w", err)
		}
		timers[seq] = at
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load timers: %w", err)
	}
	return timers, nil
}

func (inv *invocation) fencedTx(ctx context.Context, op string, fn func(tx *sql.Tx) error) error {
	return inv.rt.inTx(ctx, op, func(tx *sql.Tx) error {
		var owned bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM durable_run WHERE key = ? AND owner = ? AND claim = ? AND state = 'running')`,
			inv.exec.key, inv.exec.owner, inv.exec.claim).Scan(&owned); err != nil {
			return fmt.Errorf("%s: read owner: %w", op, err)
		}
		if !owned {
			return errNotOwned
		}
		return fn(tx)
	})
}

func (inv *invocation) RunAction(ctx context.Context, key string, fn func(ctx context.Context) ([]byte, error)) ([]byte, error) {
	if key == "" {
		return nil, invalidArgument("durable run action requires a key")
	}
	if fn == nil {
		return nil, invalidArgument("durable run action requires a function")
	}
	if inv.lost() {
		return nil, ownershipLost(inv.exec.key)
	}
	inv.mu.Lock()
	recorded, found := inv.actions[key]
	inv.mu.Unlock()
	if found {
		return recorded.answer()
	}
	inv.releaseSlot()
	out, fnErr := fn(ctx)
	if fnErr != nil && inv.ended() {
		return nil, fnErr
	}
	candidate := actionRecord{result: copyBytes(out)}
	if fnErr != nil {
		candidate = actionRecord{failed: true, result: []byte{}, detail: fnErr.Error()}
	}
	stored, won, err := inv.recordAction(ctx, key, candidate)
	if errors.Is(err, errNotOwned) {
		inv.lose()
		return nil, ownershipLost(inv.exec.key)
	}
	if err != nil {
		return nil, err
	}
	inv.mu.Lock()
	inv.actions[key] = stored
	inv.mu.Unlock()
	if stored.failed && won {
		return nil, &Error{Code: ErrorCodeActionFailed, Detail: stored.detail, cause: fnErr}
	}
	return stored.answer()
}

func (inv *invocation) recordAction(ctx context.Context, key string, candidate actionRecord) (actionRecord, bool, error) {
	wctx, cancel := detached(ctx)
	defer cancel()
	stored := candidate
	won := false
	err := inv.fencedTx(wctx, "durable record action", func(tx *sql.Tx) error {
		var outcome, detail string
		var result []byte
		err := tx.QueryRowContext(wctx, `SELECT outcome, result, detail FROM durable_action WHERE run_key = ? AND action_key = ?`, inv.exec.key, key).Scan(&outcome, &result, &detail)
		if err == nil {
			stored = actionRecord{failed: outcome == actionFailed, result: copyBytes(result), detail: detail}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("durable record action: read: %w", err)
		}
		outcome = actionResult
		if candidate.failed {
			outcome = actionFailed
		}
		if _, err := tx.ExecContext(wctx, `INSERT INTO durable_action (run_key, action_key, outcome, result, detail, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
			inv.exec.key, key, outcome, bytesOrEmpty(candidate.result), candidate.detail, inv.rt.stamp()); err != nil {
			return fmt.Errorf("durable record action: write: %w", err)
		}
		won = true
		return nil
	})
	if err != nil {
		return actionRecord{}, false, err
	}
	return stored, won, nil
}

func (inv *invocation) Sleep(d time.Duration) error {
	if inv.ended() {
		return context.Canceled
	}
	fireAt, ok := inv.fireTime(d)
	if !ok || !inv.until(fireAt) {
		return context.Canceled
	}
	return nil
}

func (inv *invocation) Timer(d time.Duration) <-chan time.Time {
	fired := make(chan time.Time, 1)
	if inv.ended() {
		return fired
	}
	fireAt, ok := inv.fireTime(d)
	if !ok {
		return fired
	}
	if now := inv.rt.clock.Now(); !now.Before(fireAt) {
		fired <- now
		return fired
	}
	inv.releaseSlot()
	inv.mu.Lock()
	defer inv.mu.Unlock()
	if inv.closed {
		return fired
	}
	inv.timerWG.Go(func() {
		if inv.until(fireAt) && !inv.ended() {
			fired <- inv.rt.clock.Now()
		}
	})
	return fired
}

func (inv *invocation) fireTime(d time.Duration) (time.Time, bool) {
	inv.mu.Lock()
	seq := inv.nextTimer
	inv.nextTimer++
	fireAt, found := inv.timers[seq]
	inv.mu.Unlock()
	if found {
		return fireAt, true
	}
	inv.releaseSlot()
	fireAt = inv.rt.clock.Now().Add(d)
	scope := inv.exec.scope()
	if !inv.retry(scope, func(ctx context.Context) error { return inv.recordTimer(ctx, seq, fireAt) }) {
		return time.Time{}, false
	}
	inv.mu.Lock()
	inv.timers[seq] = fireAt
	inv.mu.Unlock()
	return fireAt, true
}

func (inv *invocation) recordTimer(ctx context.Context, seq int64, fireAt time.Time) error {
	wctx, cancel := detached(ctx)
	defer cancel()
	return inv.fencedTx(wctx, "durable record timer", func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(wctx, `INSERT INTO durable_timer (run_key, seq, fire_at, created_at) VALUES (?, ?, ?, ?) ON CONFLICT (run_key, seq) DO NOTHING`,
			inv.exec.key, seq, fireAt.UTC().Format(time.RFC3339Nano), inv.rt.stamp()); err != nil {
			return fmt.Errorf("durable record timer: %w", err)
		}
		return nil
	})
}

func (inv *invocation) until(at time.Time) bool {
	clock := inv.rt.clock
	for {
		now := clock.Now()
		if !now.Before(at) {
			return true
		}
		inv.releaseSlot()
		fired := clock.Timer(min(at.Sub(now), inv.rt.clockCheck))
		if !clock.Now().Before(at) {
			return true
		}
		inv.rt.counters.parked.Add(1)
		select {
		case <-fired:
			inv.rt.counters.parked.Add(-1)
		case <-inv.done():
			inv.rt.counters.parked.Add(-1)
			return false
		}
	}
}

func (inv *invocation) consume(ctx context.Context, name string) (func(), bool) {
	inv.mu.Lock()
	lock := inv.consumers[name]
	if lock == nil {
		lock = make(chan struct{}, 1)
		inv.consumers[name] = lock
	}
	inv.mu.Unlock()
	select {
	case lock <- struct{}{}:
		return func() { <-lock }, true
	case <-ctx.Done():
		return nil, false
	case <-inv.done():
		return nil, false
	}
}

func (inv *invocation) position(name string) int64 {
	inv.mu.Lock()
	defer inv.mu.Unlock()
	return inv.positions[name]
}

func (inv *invocation) advance(name string) {
	inv.mu.Lock()
	defer inv.mu.Unlock()
	inv.positions[name]++
}

func (inv *invocation) Signal(ctx context.Context, name string) ([]byte, bool) {
	if name == "" || ctx.Err() != nil || inv.ended() {
		return nil, false
	}
	unlock, ok := inv.consume(ctx, name)
	if !ok {
		return nil, false
	}
	defer unlock()
	payload, ok := inv.awaitSignal(ctx, name)
	if !ok {
		return nil, false
	}
	inv.advance(name)
	return payload, true
}

func (inv *invocation) awaitSignal(ctx context.Context, name string) ([]byte, bool) {
	ref := signalRef{runKey: inv.exec.key, name: name, seq: inv.position(name)}
	for {
		wake := inv.wakeChannel()
		var payload []byte
		var found bool
		read := func(ctx context.Context) error {
			var err error
			payload, found, err = readSignal(ctx, inv.rt.db, ref)
			return err
		}
		if !inv.retry(ctx, read) {
			return nil, false
		}
		if found {
			return payload, true
		}
		inv.releaseSlot()
		inv.rt.counters.parked.Add(1)
		select {
		case <-wake:
			inv.rt.counters.parked.Add(-1)
		case <-ctx.Done():
			inv.rt.counters.parked.Add(-1)
			return nil, false
		case <-inv.done():
			inv.rt.counters.parked.Add(-1)
			return nil, false
		}
	}
}

func (inv *invocation) Wait(ctx context.Context, name string, timeout time.Duration) (payload []byte, timedOut, ok bool) {
	if name == "" || ctx.Err() != nil || inv.ended() {
		return nil, false, false
	}
	unlock, acquired := inv.consume(ctx, name)
	if !acquired {
		return nil, false, false
	}
	defer unlock()
	inv.mu.Lock()
	key := waitKey{name: name, seq: inv.nextWait[name]}
	inv.nextWait[name]++
	record, found := inv.waits[key]
	inv.mu.Unlock()
	if !found {
		inv.releaseSlot()
		record = waitRecord{deadline: inv.rt.clock.Now().Add(timeout), outcome: waitPending}
		if !inv.retry(ctx, func(ctx context.Context) error { return inv.recordWait(ctx, key, record.deadline) }) {
			return nil, false, false
		}
		inv.rememberWait(key, record)
	}
	switch record.outcome {
	case waitTimedOut:
		return nil, true, true
	case waitSignalled:
		signal, received := inv.awaitSignal(ctx, name)
		if !received {
			return nil, false, false
		}
		inv.advance(name)
		return signal, false, true
	}
	return inv.decideWait(ctx, key, record.deadline)
}

func (inv *invocation) rememberWait(key waitKey, record waitRecord) {
	inv.mu.Lock()
	defer inv.mu.Unlock()
	inv.waits[key] = record
}

func (inv *invocation) recordWait(ctx context.Context, key waitKey, deadline time.Time) error {
	wctx, cancel := detached(ctx)
	defer cancel()
	return inv.fencedTx(wctx, "durable record wait", func(tx *sql.Tx) error {
		stamp := inv.rt.stamp()
		if _, err := tx.ExecContext(wctx, `INSERT INTO durable_wait (run_key, name, seq, deadline, outcome, created_at, updated_at)
VALUES (?, ?, ?, ?, 'pending', ?, ?) ON CONFLICT (run_key, name, seq) DO NOTHING`,
			inv.exec.key, key.name, key.seq, deadline.UTC().Format(time.RFC3339Nano), stamp, stamp); err != nil {
			return fmt.Errorf("durable record wait: %w", err)
		}
		return nil
	})
}

func (inv *invocation) decideWait(ctx context.Context, key waitKey, deadline time.Time) (payload []byte, timedOut, ok bool) {
	clock := inv.rt.clock
	for {
		wake := inv.wakeChannel()
		input := decideInput{position: inv.position(key.name), reached: !clock.Now().Before(deadline)}
		var decision waitDecision
		decide := func(ctx context.Context) error {
			var err error
			decision, err = inv.decide(ctx, key, input)
			return err
		}
		if !inv.retry(ctx, decide) {
			return nil, false, false
		}
		switch decision.outcome {
		case waitSignalled:
			inv.rememberWait(key, waitRecord{deadline: deadline, outcome: waitSignalled})
			inv.advance(key.name)
			return decision.payload, false, true
		case waitTimedOut:
			inv.rememberWait(key, waitRecord{deadline: deadline, outcome: waitTimedOut})
			return nil, true, true
		}
		inv.releaseSlot()
		if !inv.parkWait(ctx, wake, deadline) {
			return nil, false, false
		}
	}
}

func (inv *invocation) parkWait(ctx context.Context, wake <-chan struct{}, deadline time.Time) bool {
	clock := inv.rt.clock
	for {
		now := clock.Now()
		if !now.Before(deadline) {
			return true
		}
		expired := clock.Timer(min(deadline.Sub(now), inv.rt.clockCheck))
		if !clock.Now().Before(deadline) {
			return true
		}
		inv.rt.counters.parked.Add(1)
		select {
		case <-wake:
			inv.rt.counters.parked.Add(-1)
			return true
		case <-expired:
			inv.rt.counters.parked.Add(-1)
		case <-ctx.Done():
			inv.rt.counters.parked.Add(-1)
			return false
		case <-inv.done():
			inv.rt.counters.parked.Add(-1)
			return false
		}
	}
}

func (inv *invocation) decide(ctx context.Context, key waitKey, input decideInput) (waitDecision, error) {
	wctx, cancel := detached(ctx)
	defer cancel()
	var decision waitDecision
	err := inv.fencedTx(wctx, "durable decide wait", func(tx *sql.Tx) error {
		var outcome string
		if err := tx.QueryRowContext(wctx, `SELECT outcome FROM durable_wait WHERE run_key = ? AND name = ? AND seq = ?`, inv.exec.key, key.name, key.seq).Scan(&outcome); err != nil {
			return fmt.Errorf("durable decide wait: read: %w", err)
		}
		ref := signalRef{runKey: inv.exec.key, name: key.name, seq: input.position}
		if outcome == waitTimedOut {
			decision = waitDecision{outcome: waitTimedOut}
			return nil
		}
		payload, found, err := readSignal(wctx, tx, ref)
		if err != nil {
			return fmt.Errorf("durable decide wait: %w", err)
		}
		switch {
		case outcome == waitSignalled || found:
			decision = waitDecision{outcome: waitSignalled, payload: payload}
		case input.reached:
			decision = waitDecision{outcome: waitTimedOut}
		default:
			decision = waitDecision{outcome: waitPending}
			return nil
		}
		if outcome != waitPending {
			return nil
		}
		if _, err := tx.ExecContext(wctx, `UPDATE durable_wait SET outcome = ?, updated_at = ? WHERE run_key = ? AND name = ? AND seq = ?`,
			decision.outcome, inv.rt.stamp(), inv.exec.key, key.name, key.seq); err != nil {
			return fmt.Errorf("durable decide wait: write: %w", err)
		}
		return nil
	})
	if err != nil {
		return waitDecision{}, err
	}
	return decision, nil
}
