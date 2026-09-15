package sync

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"
)

const (
	maxQueueDepth   = 4
	maxBackoffDelay = 5 * time.Minute
	baseBackoff     = 5 * time.Second
	maxPassDuration = 10 * time.Minute
)

type PassFunc func(ctx context.Context) PassOutcome

type PassOutcome struct {
	State     WorkerState
	LocalRef  string
	RemoteRef string
	UnknownID string
	Result    string
	Conflict  bool
	Retryable bool
}

type WorkerConfig struct {
	Interval time.Duration
	Logger   *slog.Logger
	Clock    func() time.Time
	Jitter   func() float64
}

type Worker struct {
	config  WorkerConfig
	state   *StateStore
	pass    PassFunc
	metrics *Metrics

	mu       sync.Mutex
	lease    bool
	queue    int
	backoff  time.Duration
	timer    *time.Timer
	started  bool
	stop     chan struct{}
	done     chan struct{}
	notified chan struct{}
}

func NewWorker(config WorkerConfig, state *StateStore, pass PassFunc) (*Worker, error) {
	if state == nil {
		return nil, errNilArgument("state")
	}
	if pass == nil {
		return nil, errNilArgument("pass")
	}
	if config.Interval <= 0 {
		return nil, Errorf(ErrorCodeInvalidArgument, "worker interval must be positive")
	}
	if config.Logger == nil {
		config.Logger = slog.Default()
	}
	if config.Clock == nil {
		config.Clock = func() time.Time { return time.Now().UTC() }
	}
	if config.Jitter == nil {
		config.Jitter = rand.Float64
	}
	return &Worker{
		config:   config,
		state:    state,
		pass:     pass,
		metrics:  &Metrics{},
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
		notified: make(chan struct{}, 1),
	}, nil
}

func (w *Worker) Notify() {
	select {
	case w.notified <- struct{}{}:
	default:
	}
}

func (w *Worker) QueueDepth() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.queue
}

func (w *Worker) Start(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	runCtx := context.WithoutCancel(ctx)
	w.mu.Lock()
	started := w.started
	w.started = true
	w.mu.Unlock()
	if started {
		return runCtx
	}
	go w.loop(ctx)
	return runCtx
}

func (w *Worker) Stop() {
	w.mu.Lock()
	started := w.started
	w.mu.Unlock()
	if !started {
		return
	}
	select {
	case <-w.stop:
	default:
		close(w.stop)
	}
	w.mu.Lock()
	timer := w.timer
	w.timer = nil
	w.mu.Unlock()
	if timer != nil {
		timer.Stop()
	}
	<-w.done
}

func (w *Worker) loop(ctx context.Context) {
	defer close(w.done)
	w.mu.Lock()
	w.timer = time.NewTimer(w.jitteredIntervalLocked())
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		if w.timer != nil {
			w.timer.Stop()
			w.timer = nil
		}
		w.mu.Unlock()
	}()
	for {
		w.mu.Lock()
		timer := w.timer
		stopped := w.stoppedLocked()
		w.mu.Unlock()
		if timer == nil || stopped {
			return
		}
		select {
		case <-w.stop:
			return
		case <-ctx.Done():
			return
		case <-w.notified:
			w.runPass(context.WithoutCancel(ctx))
			w.reschedule()
			w.drainNotified(ctx)
		case <-timer.C:
			w.runPass(context.WithoutCancel(ctx))
			w.reschedule()
		}
	}
}

func (w *Worker) stoppedLocked() bool {
	select {
	case <-w.stop:
		return true
	default:
		return false
	}
}

func (w *Worker) drainNotified(ctx context.Context) {
	for {
		select {
		case <-w.notified:
			w.runPass(context.WithoutCancel(ctx))
			w.reschedule()
		default:
			return
		}
	}
}

func (w *Worker) reschedule() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.timer != nil {
		w.timer.Stop()
	}
	select {
	case <-w.stop:
		w.timer = nil
		return
	default:
	}
	w.timer = time.NewTimer(w.jitteredIntervalLocked())
}

func (w *Worker) jitteredIntervalLocked() time.Duration {
	base := float64(w.config.Interval)
	jitter := w.config.Jitter()
	delay := base * (0.8 + 0.4*jitter)
	if w.backoff > 0 {
		delay += float64(w.backoff)
	}
	if delay > float64(maxBackoffDelay+w.config.Interval) {
		delay = float64(maxBackoffDelay + w.config.Interval)
	}
	return time.Duration(delay)
}

func (w *Worker) runPass(parent context.Context) {
	w.mu.Lock()
	if w.lease {
		if w.queue < maxQueueDepth {
			w.queue++
		}
		w.mu.Unlock()
		return
	}
	w.lease = true
	w.mu.Unlock()
	coalesced := func() int {
		defer func() {
			w.mu.Lock()
			w.lease = false
			w.mu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(parent, maxPassDuration)
		defer cancel()
		w.state.MarkRunning()
		start := w.config.Clock()
		outcome := w.pass(ctx)
		elapsed := w.config.Clock().Sub(start)
		w.recordOutcome(ctx, &outcome, elapsed)
		w.mu.Lock()
		pending := w.queue
		w.queue = 0
		w.mu.Unlock()
		return pending
	}()
	for range coalesced {
		w.runPass(parent)
	}
}

//nolint:contextcheck // detached pass context outlives parent cancel so shutdown does not abort a bounded pass mid-write.
func (w *Worker) RunOnce(ctx context.Context) PassOutcome {
	if ctx == nil {
		ctx = context.Background()
	}
	detached := context.WithoutCancel(ctx)
	passCtx, cancel := context.WithTimeout(detached, maxPassDuration)
	defer cancel()
	w.state.MarkRunning()
	start := w.config.Clock()
	outcome := w.pass(passCtx)
	elapsed := w.config.Clock().Sub(start)
	w.recordOutcome(passCtx, &outcome, elapsed)
	return outcome
}

func (w *Worker) MetricsSnapshot() Metrics {
	if w.metrics == nil {
		return Metrics{}
	}
	return w.metrics.Snapshot()
}

func (w *Worker) Backoff() time.Duration {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.backoff
}

func (w *Worker) recordOutcome(ctx context.Context, outcome *PassOutcome, elapsed time.Duration) {
	now := w.config.Clock()
	normalized := normalizeSyncResult(outcome.Result)
	switch {
	case outcome.State == WorkerUnknown || outcome.UnknownID != "":
		w.state.MarkUnknown(outcome.UnknownID, now)
		w.noteRetryable(outcome.Retryable)
	case outcome.Conflict || outcome.State == WorkerConflict:
		w.state.MarkConflict(outcome.LocalRef, outcome.RemoteRef, now)
		w.noteRetryable(outcome.Retryable)
	case outcome.State == WorkerDisabled:
		w.state.MarkDisabled()
		w.noteRetryable(false)
	default:
		w.state.MarkIdle(outcome.LocalRef, outcome.RemoteRef, now, elapsed, normalized)
		w.noteRetryable(outcome.Retryable)
	}
	if w.metrics != nil {
		w.metrics.Record(Observation{
			Result:   normalized,
			Conflict: outcome.Conflict || outcome.State == WorkerConflict,
			Unknown:  outcome.State == WorkerUnknown || outcome.UnknownID != "",
			Latency:  elapsed,
		})
	}
	w.config.Logger.InfoContext(ctx, "sync pass completed",
		"component", "sync",
		"result", normalized,
		"latency_ms", elapsed.Milliseconds())
}

func (w *Worker) noteRetryable(retryable bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !retryable {
		w.backoff = 0
		return
	}
	if w.backoff == 0 {
		w.backoff = baseBackoff
		return
	}
	w.backoff *= 2
	if w.backoff > maxBackoffDelay {
		w.backoff = maxBackoffDelay
	}
}
