package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
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
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func signalSelf(t *testing.T, sig syscall.Signal) {
	t.Helper()
	if err := syscall.Kill(syscall.Getpid(), sig); err != nil {
		t.Fatalf("send signal: %v", err)
	}
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
	srv.Add(failing)
	srv.Add(other)

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
	srv.Add(panicking)

	err := srv.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "panicked") {
		t.Fatalf("Run error = %v, want panic error", err)
	}
}

func TestRunGracefulShutdownOnSignal(t *testing.T) {
	started := make(chan struct{})
	srv := New(Options{Logger: discardLogger()})
	srv.Add(blockingListener("test", started))

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
	srv := New(Options{Logger: discardLogger(), ShutdownTimeout: 5 * time.Second})
	srv.exitFunc = func(code int) { exitCh <- code }
	srv.Add(slowDrainListener("test", started, 500*time.Millisecond))

	go func() { _ = srv.Run(context.Background()) }()
	<-started

	signalSelf(t, syscall.SIGTERM)
	time.Sleep(50 * time.Millisecond)
	signalSelf(t, syscall.SIGTERM)

	select {
	case code := <-exitCh:
		if code != forcedExitCode {
			t.Errorf("forced exit code = %d, want %d", code, forcedExitCode)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("forced exit was not triggered on double signal")
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
	srv.Add(stubborn)

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
