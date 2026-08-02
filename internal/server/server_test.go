package server

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

var _ Listener = (*mockListener)(nil)

type mockListener struct {
	name  string
	start func(ctx context.Context) error
}

func (m *mockListener) Name() string                    { return m.name }
func (m *mockListener) Start(ctx context.Context) error { return m.start(ctx) }

func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

func signalSelf(t *testing.T, sig syscall.Signal) {
	t.Helper()
	if err := syscall.Kill(syscall.Getpid(), sig); err != nil {
		t.Fatalf("send signal: %v", err)
	}
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// waitForLog polls until the logger output contains want, so tests send a
// second signal only after the first one was actually consumed by Run. A
// fixed sleep would be race-prone; the kernel coalesces identical pending
// signals, so the second signal must not be sent while the first is still
// undelivered.
func waitForLog(t *testing.T, buf *syncBuffer, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(buf.String(), want) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("log %q not observed within 2s; buffer: %s", want, buf.String())
}

func blockingListener(name string, started chan<- struct{}) *mockListener {
	return &mockListener{
		name: name,
		start: func(ctx context.Context) error {
			if started != nil {
				close(started)
			}
			<-ctx.Done()
			return nil
		},
	}
}

func slowDrainListener(name string, started chan<- struct{}, drain time.Duration) *mockListener {
	return &mockListener{
		name: name,
		start: func(ctx context.Context) error {
			if started != nil {
				close(started)
			}
			<-ctx.Done()
			time.Sleep(drain)
			return nil
		},
	}
}

func TestRunListenerErrorStopsOthers(t *testing.T) {
	failing := &mockListener{name: "failing", start: func(ctx context.Context) error {
		return errors.New("boom")
	}}
	otherStopped := make(chan struct{})
	other := &mockListener{name: "other", start: func(ctx context.Context) error {
		<-ctx.Done()
		close(otherStopped)
		return nil
	}}

	srv := New(Options{Logger: discardLogger()})
	if err := srv.Add(failing); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := srv.Add(other); err != nil {
		t.Fatalf("Add: %v", err)
	}

	err := srv.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("Run error = %v, want boom", err)
	}
	select {
	case <-otherStopped:
	case <-time.After(2 * time.Second):
		t.Fatal("other listener was not cancelled when the failing listener errored")
	}
}

func TestRunPanicRecovery(t *testing.T) {
	panicking := &mockListener{name: "panicking", start: func(ctx context.Context) error {
		panic("kaboom")
	}}
	srv := New(Options{Logger: discardLogger()})
	if err := srv.Add(panicking); err != nil {
		t.Fatalf("Add: %v", err)
	}

	err := srv.Run(context.Background())
	if code, ok := CodeOf(err); !ok || code != ErrorCodeListenerPanicked {
		t.Fatalf("CodeOf(%v) = %q, %v; want %q", err, code, ok, ErrorCodeListenerPanicked)
	}
	if strings.Contains(err.Error(), "kaboom") {
		t.Errorf("panic value leaked into the returned error: %v", err)
	}
}

func TestRunGracefulShutdownOnSignal(t *testing.T) {
	started := make(chan struct{})
	srv := New(Options{Logger: discardLogger()})
	if err := srv.Add(blockingListener("test", started)); err != nil {
		t.Fatalf("Add: %v", err)
	}

	result := make(chan error, 1)
	go func() { result <- srv.Run(context.Background()) }()
	<-started

	signalSelf(t, syscall.SIGTERM)
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("Run returned error after graceful shutdown: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after SIGTERM")
	}
}

func TestRunForcedExitOnDoubleSignal(t *testing.T) {
	started := make(chan struct{})
	exitCh := make(chan int, 1)
	buf := &syncBuffer{}
	srv := New(Options{Logger: slog.New(slog.NewTextHandler(buf, nil)), ShutdownTimeout: 5 * time.Second})
	srv.exitFunc = func(code int) { exitCh <- code }
	if err := srv.Add(slowDrainListener("test", started, 500*time.Millisecond)); err != nil {
		t.Fatalf("Add: %v", err)
	}

	go func() { _ = srv.Run(context.Background()) }()
	<-started

	signalSelf(t, syscall.SIGTERM)
	waitForLog(t, buf, "received shutdown signal")
	signalSelf(t, syscall.SIGTERM)

	select {
	case code := <-exitCh:
		if code != 143 {
			t.Errorf("forced exit code = %d, want 143 (128+SIGTERM)", code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("forced exit was not triggered on double signal")
	}
}

func TestRunForcedExitSIGINT(t *testing.T) {
	started := make(chan struct{})
	exitCh := make(chan int, 1)
	buf := &syncBuffer{}
	srv := New(Options{Logger: slog.New(slog.NewTextHandler(buf, nil)), ShutdownTimeout: 5 * time.Second})
	srv.exitFunc = func(code int) { exitCh <- code }
	if err := srv.Add(slowDrainListener("test", started, 500*time.Millisecond)); err != nil {
		t.Fatalf("Add: %v", err)
	}

	go func() { _ = srv.Run(context.Background()) }()
	<-started

	signalSelf(t, syscall.SIGINT)
	waitForLog(t, buf, "received shutdown signal")
	signalSelf(t, syscall.SIGINT)

	select {
	case code := <-exitCh:
		if code != 130 {
			t.Errorf("forced exit code = %d, want 130 (128+SIGINT)", code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("forced exit was not triggered on double signal")
	}
}

func TestRunCancellationWithoutListeners(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	srv := New(Options{Logger: discardLogger()})

	result := make(chan error, 1)
	go func() { result <- srv.Run(ctx) }()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("Run returned error on cancellation: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return on parent context cancellation")
	}
}

func TestRunCancellationWithListeners(t *testing.T) {
	started := make(chan struct{})
	stopped := make(chan struct{})
	listener := &mockListener{name: "test", start: func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		close(stopped)
		return nil
	}}
	srv := New(Options{Logger: discardLogger()})
	if err := srv.Add(listener); err != nil {
		t.Fatalf("Add: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- srv.Run(ctx) }()
	<-started
	cancel()

	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("Run returned error on cancellation: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("listener was not stopped on cancellation")
	}
}

func TestAddNilListener(t *testing.T) {
	srv := New(Options{Logger: discardLogger()})
	err := srv.Add(nil)
	if code, ok := CodeOf(err); !ok || code != ErrorCodeInvalidArgument {
		t.Fatalf("CodeOf(%v) = %q, %v; want %q", err, code, ok, ErrorCodeInvalidArgument)
	}
}

func TestForceExitWatcherExitsOnDone(t *testing.T) {
	srv := New(Options{Logger: discardLogger()})
	exited := make(chan struct{})
	srv.exitFunc = func(code int) { close(exited) }

	sigCh := make(chan os.Signal, 1)
	done := make(chan struct{})
	close(done)

	go srv.forceExitOnSecondSignal(t.Context(), sigCh, done)
	select {
	case <-exited:
		t.Error("exitFunc called after done closed — watcher leaked")
	case <-time.After(300 * time.Millisecond):
	}
}

func TestRunShutdownTimeout(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	stubborn := &mockListener{name: "stubborn", start: func(ctx context.Context) error {
		close(started)
		<-release
		return nil
	}}

	srv := New(Options{Logger: discardLogger(), ShutdownTimeout: 50 * time.Millisecond})
	if err := srv.Add(stubborn); err != nil {
		t.Fatalf("Add: %v", err)
	}

	result := make(chan error, 1)
	go func() { result <- srv.Run(context.Background()) }()
	<-started

	signalSelf(t, syscall.SIGTERM)
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("Run returned error after timeout: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after shutdown timeout")
	}
}

// A listener that ignores cancellation must not be able to hang the process.
// Before the shutdown paths were unified, the context path skipped the drain
// timeout entirely and Run blocked here forever.
func TestRunCancellationHonorsShutdownTimeout(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	defer close(release)

	stuck := &mockListener{name: "stuck", start: func(ctx context.Context) error {
		close(started)
		<-release
		return nil
	}}
	srv := New(Options{Logger: discardLogger(), ShutdownTimeout: 100 * time.Millisecond})
	if err := srv.Add(stuck); err != nil {
		t.Fatalf("Add: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- srv.Run(ctx) }()
	<-started
	cancel()

	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("Run returned error on cancellation: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run ignored the shutdown timeout on parent context cancellation")
	}
}
