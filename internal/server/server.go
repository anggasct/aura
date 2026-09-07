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

func (s *Server) Add(l Listener) error {
	if l == nil {
		return &Error{Code: ErrorCodeInvalidArgument, Detail: "listener must not be nil"}
	}
	s.listeners = append(s.listeners, l)
	return nil
}

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
	drained := false

	if len(s.listeners) == 0 {
		select {
		case sig := <-sigCh:
			s.logger.InfoContext(ctx, "received shutdown signal", "component", "server", "signal", sig.String())
		case <-ctx.Done():
			s.logger.InfoContext(ctx, "shutdown requested", "component", "server")
		}
	} else {
		select {
		case sig := <-sigCh:
			s.logger.InfoContext(ctx, "received shutdown signal", "component", "server", "signal", sig.String())
		case <-ctx.Done():
			s.logger.InfoContext(ctx, "shutdown requested", "component", "server")
		case firstErr = <-waitResult:
			drained = true
		}
	}

	cancel()

	if !drained {
		done := make(chan struct{})
		defer close(done)
		go s.forceExitOnSecondSignal(ctx, sigCh, done)

		select {
		case firstErr = <-waitResult:
		case <-time.After(s.shutdownTimeout):
			s.logger.WarnContext(ctx, "shutdown timed out; forcing exit", "component", "server", "timeout", s.shutdownTimeout)
		}
	}

	if firstErr != nil {
		s.logger.ErrorContext(ctx, "server stopped with error", "component", "server", "error", firstErr)
		return firstErr
	}
	s.logger.InfoContext(ctx, "server stopped", "component", "server")
	return nil
}

func (s *Server) runListener(ctx context.Context, l Listener) (err error) {
	defer func() {
		if r := recover(); r != nil {
			s.logger.ErrorContext(ctx, "listener panic",
				"component", "server",
				"listener", l.Name(),
				"panic", fmt.Sprint(r),
				"stack", string(debug.Stack()),
			)
			err = &Error{Code: ErrorCodeListenerPanicked, Detail: "listener " + l.Name() + " panicked"}
		}
	}()
	s.logger.InfoContext(ctx, "starting listener", "component", "server", "listener", l.Name())
	if err := l.Start(ctx); err != nil {
		s.logger.ErrorContext(ctx, "listener failed", "component", "server", "listener", l.Name(), "error", err)
		return err
	}
	return nil
}

func (s *Server) forceExitOnSecondSignal(ctx context.Context, sigCh <-chan os.Signal, done <-chan struct{}) {
	select {
	case <-done:
	case sig := <-sigCh:
		s.logger.WarnContext(ctx, "forced shutdown", "component", "server", "signal", sig.String())
		s.exitFunc(exitCodeForSignal(sig))
	}
}

func exitCodeForSignal(sig os.Signal) int {
	if s, ok := sig.(syscall.Signal); ok {
		return 128 + int(s)
	}
	return 128
}
