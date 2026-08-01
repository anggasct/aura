package server

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"
)

const defaultShutdownTimeout = 30 * time.Second

type Listener interface {
	Name() string
	Start(ctx context.Context) error
}

type Options struct {
	ShutdownTimeout time.Duration
	Logger          *slog.Logger
}

type Server struct {
	listeners       []Listener
	shutdownTimeout time.Duration
	logger          *slog.Logger
	exitFunc        func(code int)
}

func New(opts Options) *Server {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	timeout := opts.ShutdownTimeout
	if timeout <= 0 {
		timeout = defaultShutdownTimeout
	}
	return &Server{
		shutdownTimeout: timeout,
		logger:          logger,
		exitFunc:        os.Exit,
	}
}

// Add registers a listener. Add must be called before Run; the listener
// slice is not synchronized against concurrent calls.
func (s *Server) Add(l Listener) error {
	if l == nil {
		return &Error{Code: ErrorCodeInvalidArgument, Detail: "listener must not be nil"}
	}
	s.listeners = append(s.listeners, l)
	return nil
}

// Run starts all registered listeners and blocks until a shutdown signal, a
// listener failure, or ctx cancellation. The first signal drains listeners
// within the shutdown timeout; a second signal forces an immediate exit with
// the signal's 128+signum code. It returns the first listener error, or nil
// on a clean shutdown.
func (s *Server) Run(ctx context.Context) error {
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(sigCh)

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	g, gctx := errgroup.WithContext(runCtx)
	for _, l := range s.listeners {
		g.Go(func() error { return s.runListener(gctx, l) })
	}

	waitResult := make(chan error, 1)
	go func() { waitResult <- g.Wait() }()

	var firstErr error
	signalled := false
	if len(s.listeners) == 0 {
		select {
		case sig := <-sigCh:
			s.logger.Info("received shutdown signal", "component", "server", "signal", sig.String())
			signalled = true
		case <-gctx.Done():
			return nil
		}
	} else {
		select {
		case sig := <-sigCh:
			s.logger.Info("received shutdown signal", "component", "server", "signal", sig.String())
			signalled = true
		case <-gctx.Done():
			firstErr = <-waitResult
		}
	}

	cancel()
	done := make(chan struct{})
	defer close(done)
	if signalled {
		go s.forceExitOnSecondSignal(sigCh, done)
	}

	if firstErr == nil && signalled {
		select {
		case firstErr = <-waitResult:
		case <-time.After(s.shutdownTimeout):
			s.logger.Warn("shutdown timed out; forcing exit", "component", "server", "timeout", s.shutdownTimeout)
		}
	}

	if firstErr != nil {
		s.logger.Error("server stopped with error", "component", "server", "error", firstErr)
		return firstErr
	}
	s.logger.Info("server stopped", "component", "server")
	return nil
}

func (s *Server) runListener(ctx context.Context, l Listener) (err error) {
	defer func() {
		if r := recover(); r != nil {
			s.logger.Error("listener panic",
				"component", "server",
				"listener", l.Name(),
				"panic", fmt.Sprint(r),
				"stack", string(debug.Stack()),
			)
			err = &Error{Code: ErrorCodeListenerPanicked, Detail: "listener " + l.Name() + " panicked"}
		}
	}()
	s.logger.Info("starting listener", "component", "server", "listener", l.Name())
	if err := l.Start(ctx); err != nil {
		s.logger.Error("listener failed", "component", "server", "listener", l.Name(), "error", err)
		return err
	}
	return nil
}

// forceExitOnSecondSignal exits with the signal's 128+signum code on a
// second signal, or returns when done closes so no goroutine outlives Run.
func (s *Server) forceExitOnSecondSignal(sigCh <-chan os.Signal, done <-chan struct{}) {
	select {
	case <-done:
	case sig := <-sigCh:
		s.logger.Warn("forced shutdown", "component", "server", "signal", sig.String())
		s.exitFunc(exitCodeForSignal(sig))
	}
}

func exitCodeForSignal(sig os.Signal) int {
	if s, ok := sig.(syscall.Signal); ok {
		return 128 + int(s)
	}
	return 128
}
