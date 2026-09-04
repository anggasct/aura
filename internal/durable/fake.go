package durable

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// ErrUnknownRun is returned for operations on a run this runtime never
// started.
var ErrUnknownRun = errors.New("unknown durable run")

// Clock supplies time to handlers so tests drive timers deterministically.
type Clock interface {
	Now() time.Time
	// Timer returns a channel that fires once after d elapses.
	Timer(d time.Duration) <-chan time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now().UTC() }

func (realClock) Timer(d time.Duration) <-chan time.Time {
	if d <= 0 {
		fired := make(chan time.Time, 1)
		fired <- time.Now()
		return fired
	}
	timer := time.NewTimer(d)
	return timer.C
}

// RealClock reads the wall clock.
func RealClock() Clock { return realClock{} }

// ManualClock advances only when tests tell it to, making timers
// deterministic and instant.
type ManualClock struct {
	mu      sync.Mutex
	current time.Time
	waiters []manualWaiter
}

type manualWaiter struct {
	at time.Time
	ch chan time.Time
}

func NewManualClock(start time.Time) *ManualClock {
	return &ManualClock{current: start.UTC()}
}

func (c *ManualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.current
}

// Advance moves the clock forward and fires every timer scheduled within
// the elapsed span.
func (c *ManualClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.current = c.current.Add(d)
	target := c.current
	var due []manualWaiter
	remaining := c.waiters[:0]
	for _, waiter := range c.waiters {
		if !waiter.at.After(target) {
			due = append(due, waiter)
		} else {
			remaining = append(remaining, waiter)
		}
	}
	c.waiters = remaining
	c.mu.Unlock()
	for _, waiter := range due {
		waiter.ch <- target
		close(waiter.ch)
	}
}

// Timer schedules a fire time on the manual clock.
func (c *ManualClock) Timer(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	c.waiters = append(c.waiters, manualWaiter{at: c.current.Add(d), ch: ch})
	return ch
}

// Invocation is the handler-facing execution context: Signal replaces
// direct blocking and Sleep is a durable timer. Cancellation arrives
// through the handler's own context parameter and through the context a
// Signal caller supplies for its wait.
type Invocation struct {
	run     RunRef
	payload []byte
	done    <-chan struct{}
	clock   Clock
	mu      *sync.Mutex
	signals map[string]*signalQueue
}

type signalQueue struct {
	// waiter is the reply channel of the single parked Signal call, nil
	// when none. A re-Signal for the same name replaces an abandoned
	// waiter instead of queuing behind it, so a timed-out attempt can
	// never wedge the next attempt's registration.
	waiter    chan signalDelivery
	delivered []signalDelivery
}

type signalDelivery struct {
	payload []byte
}

// Run returns the execution this invocation belongs to.
func (i *Invocation) Run() RunRef { return i.run }

// Payload returns the StartRequest payload.
func (i *Invocation) Payload() []byte { return i.payload }

// Signal blocks until the named signal arrives, the wait context ends, or
// the invocation ends. The payload returns with ok=true; ok=false means
// the invocation or the wait was cancelled before the signal arrived. A
// cancelled wait never consumes a delivery: a payload that raced the
// cancellation stays queued for the next waiter.
func (i *Invocation) Signal(ctx context.Context, name string) ([]byte, bool) {
	i.mu.Lock()
	queue := i.signals[name]
	if queue == nil {
		queue = &signalQueue{}
		i.signals[name] = queue
	}
	if len(queue.delivered) > 0 {
		delivery := queue.delivered[0]
		queue.delivered = queue.delivered[1:]
		i.mu.Unlock()
		return delivery.payload, true
	}
	reply := make(chan signalDelivery, 1)
	queue.waiter = reply
	i.mu.Unlock()
	select {
	case delivery := <-reply:
		if ctx.Err() != nil {
			i.detachWaiter(name, reply, &delivery)
			return nil, false
		}
		return delivery.payload, true
	case <-ctx.Done():
		i.detachWaiter(name, reply, nil)
		return nil, false
	case <-i.done:
		return nil, false
	}
}

// detachWaiter unregisters a wait abandoned through its context and
// restores any payload that raced the cancellation so a later waiter still
// receives it. A live waiter is woken with the payload directly instead of
// leaving it stranded in the delivered queue.
func (i *Invocation) detachWaiter(name string, reply chan signalDelivery, raced *signalDelivery) {
	i.mu.Lock()
	defer i.mu.Unlock()
	queue := i.signals[name]
	if queue == nil {
		return
	}
	if queue.waiter == reply {
		queue.waiter = nil
	}
	if raced == nil {
		select {
		case delivery := <-reply:
			raced = &delivery
		default:
			return
		}
	}
	if waiter := queue.waiter; waiter != nil {
		waiter <- *raced
		return
	}
	queue.delivered = append([]signalDelivery{*raced}, queue.delivered...)
}

// Sleep blocks for the duration on the runtime clock; it returns the
// cancellation error when the invocation ends first.
func (i *Invocation) Sleep(d time.Duration) error {
	timer := i.clock.Timer(d)
	select {
	case <-timer:
		return nil
	case <-i.done:
		return context.Canceled
	}
}

// Timer exposes the runtime clock so a handler can race a timer against
// other channels.
func (i *Invocation) Timer(d time.Duration) <-chan time.Time {
	return i.clock.Timer(d)
}

// Fake is the in-process Runtime: handlers run on their own goroutine,
// signals queue or wake waiting handlers, cancellation is cooperative, and
// state transitions are observable. It is the conformance reference for the
// durable adapter and proves suspension semantics in-process.
type Fake struct {
	mu       sync.Mutex
	handlers map[string]Handler
	runs     map[string]*fakeRun
	clock    Clock
	logger   func(format string, args ...any)
}

type fakeRun struct {
	ref        RunRef
	cancel     context.CancelFunc
	done       chan struct{}
	invocation *Invocation
	mu         sync.Mutex
	state      RunState
	detail     string
}

// NewFake returns a fake runtime on the real clock; use WithClock in tests.
func NewFake() *Fake {
	return &Fake{handlers: map[string]Handler{}, runs: map[string]*fakeRun{}, clock: RealClock()}
}

// WithClock overrides the clock before first use.
func (f *Fake) WithClock(clock Clock) *Fake {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clock = clock
	return f
}

// WithLogger installs an event sink for test diagnostics.
func (f *Fake) WithLogger(logger func(format string, args ...any)) *Fake {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logger = logger
	return f
}

// RegisterHandler installs the handler for a service name.
func (f *Fake) RegisterHandler(name string, fn Handler) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handlers[name] = fn
}

func (f *Fake) log(format string, args ...any) {
	f.mu.Lock()
	logger := f.logger
	f.mu.Unlock()
	if logger != nil {
		logger(format, args...)
	}
}

// Start runs the named handler under the request key. A key that already
// exists returns its reference without re-running (idempotent per key).
func (f *Fake) Start(ctx context.Context, req StartRequest) (RunRef, error) {
	if req.Key == "" {
		return RunRef{}, errors.New("durable start requires a key")
	}
	f.mu.Lock()
	if run, ok := f.runs[req.Key]; ok {
		f.mu.Unlock()
		return run.ref, nil
	}
	handler, ok := f.handlers[req.Handler]
	if !ok {
		f.mu.Unlock()
		return RunRef{}, fmt.Errorf("no durable handler registered for %q", req.Handler)
	}
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	run := &fakeRun{ref: RunRef{Key: req.Key}, cancel: cancel, done: make(chan struct{}), state: RunRunning}
	f.runs[req.Key] = run
	clock := f.clock
	f.mu.Unlock()

	inv := &Invocation{
		run:     run.ref,
		payload: append([]byte(nil), req.Payload...),
		done:    runCtx.Done(),
		clock:   clock,
		mu:      &sync.Mutex{},
		signals: map[string]*signalQueue{},
	}
	f.mu.Lock()
	run.invocation = inv
	f.mu.Unlock()
	go func() {
		defer close(run.done)
		handlerErr := handler(runCtx, inv)
		state := RunSucceeded
		detail := ""
		switch {
		case runCtx.Err() != nil:
			state = RunCancelled
		case handlerErr != nil:
			state = RunFailed
			detail = handlerErr.Error()
		}
		run.mu.Lock()
		run.state = state
		run.detail = detail
		run.mu.Unlock()
		// Release any Signal caller still parked on this invocation so no
		// waiter survives run completion.
		cancel()
		f.log("durable run %s finished as %s", run.ref.Key, state)
	}()
	return run.ref, nil
}

// Signal delivers a named signal payload to the run: a waiting Signal call
// wakes with it, otherwise it queues for the next Signal call.
func (f *Fake) Signal(_ context.Context, run RunRef, name string, payload []byte) error {
	f.mu.Lock()
	run_ := f.runs[run.Key]
	f.mu.Unlock()
	if run_ == nil {
		return fmt.Errorf("%w: %s", ErrUnknownRun, run.Key)
	}
	run_.mu.Lock()
	state := run_.state
	run_.mu.Unlock()
	if state == RunSucceeded || state == RunFailed || state == RunCancelled {
		return fmt.Errorf("durable run %s is %s and cannot receive signals", run.Key, state)
	}
	select {
	case <-run_.done:
		return fmt.Errorf("durable run %s already ended", run.Key)
	default:
	}
	inv := f.invocationFor(run.Key)
	if inv == nil {
		return fmt.Errorf("%w: %s", ErrUnknownRun, run.Key)
	}
	inv.mu.Lock()
	queue := inv.signals[name]
	if queue == nil {
		queue = &signalQueue{}
		inv.signals[name] = queue
	}
	delivery := signalDelivery{payload: append([]byte(nil), payload...)}
	if queue.waiter != nil {
		queue.waiter <- delivery
		queue.waiter = nil
	} else {
		queue.delivered = append(queue.delivered, delivery)
	}
	inv.mu.Unlock()
	f.log("durable run %s signaled %s", run.Key, name)
	return nil
}

// Cancel cooperatively cancels the run; handlers observe it through their
// context and signal waits.
func (f *Fake) Cancel(_ context.Context, run RunRef) error {
	f.mu.Lock()
	target := f.runs[run.Key]
	f.mu.Unlock()
	if target == nil {
		return fmt.Errorf("%w: %s", ErrUnknownRun, run.Key)
	}
	target.mu.Lock()
	terminal := target.state != RunRunning && target.state != RunSuspended
	target.mu.Unlock()
	if terminal {
		return nil
	}
	target.cancel()
	<-target.done
	return nil
}

// Status reports the run state.
func (f *Fake) Status(_ context.Context, run RunRef) (RunStatus, error) {
	f.mu.Lock()
	target := f.runs[run.Key]
	f.mu.Unlock()
	if target == nil {
		return RunStatus{}, fmt.Errorf("%w: %s", ErrUnknownRun, run.Key)
	}
	target.mu.Lock()
	defer target.mu.Unlock()
	return RunStatus{State: target.state, Detail: target.detail}, nil
}

// WaitReady blocks until the run reaches a terminal state, for tests.
func (f *Fake) WaitReady(run RunRef) {
	f.mu.Lock()
	target := f.runs[run.Key]
	f.mu.Unlock()
	if target != nil {
		<-target.done
	}
}

func (f *Fake) invocationFor(key string) *Invocation {
	f.mu.Lock()
	defer f.mu.Unlock()
	run := f.runs[key]
	if run == nil {
		return nil
	}
	return run.invocation
}
