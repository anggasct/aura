package durable

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

var ErrUnknownRun = errors.New("unknown durable run")

type Clock interface {
	Now() time.Time
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

func RealClock() Clock { return realClock{} }

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

func (c *ManualClock) Timer(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	c.waiters = append(c.waiters, manualWaiter{at: c.current.Add(d), ch: ch})
	return ch
}

type invocation struct {
	run     RunRef
	payload []byte
	done    <-chan struct{}
	clock   Clock
	mu      *sync.Mutex
	signals map[string]*signalQueue
}

type signalQueue struct {
	waiter    chan signalDelivery
	delivered []signalDelivery
}

type signalDelivery struct {
	payload []byte
}

func (i *invocation) Run() RunRef { return i.run }

func (i *invocation) Payload() []byte { return i.payload }

func (i *invocation) Signal(ctx context.Context, name string) ([]byte, bool) {
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

func (i *invocation) detachWaiter(name string, reply chan signalDelivery, raced *signalDelivery) {
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

func (i *invocation) Sleep(d time.Duration) error {
	timer := i.clock.Timer(d)
	select {
	case <-timer:
		return nil
	case <-i.done:
		return context.Canceled
	}
}

func (i *invocation) Timer(d time.Duration) <-chan time.Time {
	return i.clock.Timer(d)
}

func (i *invocation) Wait(ctx context.Context, name string, timeout time.Duration) (payload []byte, timedOut, ok bool) {
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
		return delivery.payload, false, true
	}
	reply := make(chan signalDelivery, 1)
	queue.waiter = reply
	i.mu.Unlock()
	timer := i.clock.Timer(timeout)
	select {
	case delivery := <-reply:
		if ctx.Err() != nil {
			i.detachWaiter(name, reply, &delivery)
			return nil, false, false
		}
		return delivery.payload, false, true
	case <-timer:
		i.detachWaiter(name, reply, nil)
		return nil, true, true
	case <-ctx.Done():
		i.detachWaiter(name, reply, nil)
		return nil, false, false
	case <-i.done:
		return nil, false, false
	}
}

func (i *invocation) RunAction(ctx context.Context, key string, fn func(ctx context.Context) ([]byte, error)) ([]byte, error) {
	if key == "" {
		return nil, errors.New("durable run action requires a key")
	}
	if fn == nil {
		return nil, errors.New("durable run action requires a function")
	}
	return fn(ctx)
}

type Fake struct {
	mu        sync.Mutex
	handlers  map[string]Handler
	calls     map[string]CallHandler
	consumed  map[string]struct{}
	deadlines map[string]time.Time
	runs      map[string]*fakeRun
	clock     Clock
	logger    func(format string, args ...any)
}

type fakeRun struct {
	ref        RunRef
	cancel     context.CancelFunc
	done       chan struct{}
	invocation *invocation
	mu         sync.Mutex
	state      RunState
	detail     string
}

func NewFake() *Fake {
	return &Fake{handlers: map[string]Handler{}, runs: map[string]*fakeRun{}, clock: RealClock()}
}

func (f *Fake) WithClock(clock Clock) *Fake {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clock = clock
	return f
}

func (f *Fake) WithLogger(logger func(format string, args ...any)) *Fake {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logger = logger
	return f
}

func (f *Fake) RegisterHandler(name string, fn Handler) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handlers[name] = fn
}

type CallHandler func(ctx context.Context, key string, payload []byte) ([]byte, error)

func (f *Fake) RegisterCall(service, handler string, fn CallHandler) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.calls == nil {
		f.calls = map[string]CallHandler{}
	}
	f.calls[service+"\x00"+handler] = fn
}

func (f *Fake) Call(ctx context.Context, req CallRequest) ([]byte, error) {
	if req.Service == "" {
		return nil, errors.New("durable call requires a service")
	}
	if req.Key == "" {
		return nil, errors.New("durable call requires a key")
	}
	if req.Handler == "" {
		return nil, errors.New("durable call requires a handler")
	}
	f.mu.Lock()
	fn := f.calls[req.Service+"\x00"+req.Handler]
	f.mu.Unlock()
	if fn == nil {
		return nil, fmt.Errorf("durable call: no handler registered for %q on service %q", req.Handler, req.Service)
	}
	return fn(ctx, req.Key, req.Payload)
}

func (f *Fake) log(format string, args ...any) {
	f.mu.Lock()
	logger := f.logger
	f.mu.Unlock()
	if logger != nil {
		logger(format, args...)
	}
}

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

	inv := &invocation{
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
		cancel()
		f.log("durable run %s finished as %s", run.ref.Key, state)
	}()
	return run.ref, nil
}

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

func (f *Fake) WaitReady(run RunRef) {
	f.mu.Lock()
	target := f.runs[run.Key]
	f.mu.Unlock()
	if target != nil {
		<-target.done
	}
}

func (f *Fake) invocationFor(key string) *invocation {
	f.mu.Lock()
	defer f.mu.Unlock()
	run := f.runs[key]
	if run == nil {
		return nil
	}
	return run.invocation
}

func (f *Fake) ResolveApproval(ctx context.Context, req ResolveApprovalRequest) error {
	if req.ApprovalID == "" {
		return errors.New("durable resolve approval requires an approval id")
	}
	runKey, signal, err := ParseApprovalAddr(req.ApprovalID)
	if err != nil {
		return err
	}
	f.mu.Lock()
	if f.consumed == nil {
		f.consumed = map[string]struct{}{}
	}
	if _, ok := f.consumed[req.ApprovalID]; ok {
		f.mu.Unlock()
		return fmt.Errorf("durable approval %q was already consumed", req.ApprovalID)
	}
	f.consumed[req.ApprovalID] = struct{}{}
	f.mu.Unlock()
	return f.Signal(ctx, RunRef{Key: runKey}, signal, req.Payload)
}

func (f *Fake) RegisterTurnDeadline(sessionID string, deadline time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deadlines == nil {
		f.deadlines = map[string]time.Time{}
	}
	f.deadlines[sessionID] = deadline
}

func (f *Fake) TurnDeadline(_ context.Context, sessionID string) (time.Time, bool, error) {
	if sessionID == "" {
		return time.Time{}, false, errors.New("durable turn deadline requires a session id")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	deadline, ok := f.deadlines[sessionID]
	return deadline, ok, nil
}
