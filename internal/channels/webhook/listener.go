package webhook

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/anggasct/aura/internal/platform/server"
	"github.com/anggasct/aura/internal/platform/turn"
	platformwebhook "github.com/anggasct/aura/internal/platform/webhook"
)

type Deps struct {
	Config     *platformwebhook.Config
	Sessions   SessionStore
	Executions ExecutionStore
	Events     platformwebhook.EventLister
	Runtime    turn.AgentRuntime
	Keys       KeySource
	Handlers   []EventHandler
	Clock      func() time.Time
	Logger     *slog.Logger
}

type Listener struct {
	listen     string
	handler    http.Handler
	dispatcher *Dispatcher
}

var _ server.Listener = (*Listener)(nil)

func (l *Listener) Name() string { return "webhook" }

func (l *Listener) Start(ctx context.Context) error {
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", l.listen)
	if err != nil {
		return fmt.Errorf("webhook listener: bind %s: %w", l.listen, err)
	}
	srv := &http.Server{
		Handler:           l.handler,
		ReadHeaderTimeout: 5 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(listener) }()
	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("webhook listener: %w", err)
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("webhook listener shutdown: %w", err)
		}
		if err := l.dispatcher.Wait(shutdownCtx); err != nil {
			return fmt.Errorf("webhook listener drain: %w", err)
		}
		return nil
	}
}

type Channel struct {
	listener   *Listener
	handler    http.Handler
	dispatcher *Dispatcher
	reconcile  *platformwebhook.ReconcilePass
}

func New(deps *Deps) (*Channel, error) {
	if deps == nil {
		return nil, platformwebhook.Errorf(platformwebhook.ErrorCodeInvalidArgument, "webhook deps must not be nil")
	}
	if deps.Config == nil {
		return nil, platformwebhook.Errorf(platformwebhook.ErrorCodeInvalidArgument, "webhook config must not be nil")
	}
	if deps.Config.Listen == "" {
		return nil, platformwebhook.Errorf(platformwebhook.ErrorCodeInvalidArgument, "webhook listen address must not be empty")
	}
	if deps.Config.ReplayRetention <= 0 {
		return nil, platformwebhook.Errorf(platformwebhook.ErrorCodeInvalidArgument, "webhook replay retention must be positive")
	}
	if deps.Sessions == nil {
		return nil, platformwebhook.Errorf(platformwebhook.ErrorCodeInvalidArgument, "session store must not be nil")
	}
	if deps.Executions == nil {
		return nil, platformwebhook.Errorf(platformwebhook.ErrorCodeInvalidArgument, "execution store must not be nil")
	}
	if deps.Events == nil {
		return nil, platformwebhook.Errorf(platformwebhook.ErrorCodeInvalidArgument, "event lister must not be nil")
	}
	if deps.Runtime == nil {
		return nil, platformwebhook.Errorf(platformwebhook.ErrorCodeInvalidArgument, "runtime must not be nil")
	}
	if deps.Keys == nil {
		return nil, platformwebhook.Errorf(platformwebhook.ErrorCodeInvalidArgument, "key source must not be nil")
	}
	clock := deps.Clock
	if clock == nil {
		clock = time.Now
	}
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	entries := make([]KeyEntry, 0, len(deps.Config.Keys))
	for _, key := range deps.Config.Keys {
		var acceptUntil time.Time
		if key.AcceptUntil != "" {
			parsed, err := time.Parse(time.RFC3339, key.AcceptUntil)
			if err != nil {
				return nil, platformwebhook.Errorf(platformwebhook.ErrorCodeInvalidArgument, "key %q has an invalid rotation timestamp", key.ID)
			}
			acceptUntil = parsed
		}
		entries = append(entries, KeyEntry{ID: key.ID, SecretEnv: key.SecretEnv, AcceptUntil: acceptUntil})
	}
	ring, err := NewKeyRing(entries, deps.Keys)
	if err != nil {
		return nil, err
	}
	limiter, err := NewRateLimiter(deps.Config.RequestsPerMinute, clock)
	if err != nil {
		return nil, err
	}
	dispatcher, err := NewDispatcher(&DispatcherDeps{
		Sessions:   deps.Sessions,
		Events:     deps.Events,
		Executions: deps.Executions,
		Runtime:    deps.Runtime,
		Retention:  time.Duration(deps.Config.ReplayRetention),
		Clock:      clock,
		Logger:     logger,
		Handlers:   deps.Handlers,
	})
	if err != nil {
		return nil, err
	}
	reconcilePass, err := dispatcher.ReconcilePass()
	if err != nil {
		return nil, err
	}
	handler, err := NewHandler(Settings{
		MaxBodySize:        int64(deps.Config.MaxBodySize),
		TimestampTolerance: time.Duration(deps.Config.TimestampTolerance),
	}, ring, limiter, clock, logger, dispatcher)
	if err != nil {
		return nil, err
	}
	statusStore, err := NewExecutionStatusStore(deps.Executions)
	if err != nil {
		return nil, err
	}
	status, err := NewStatusHandler(ring, limiter, clock, time.Duration(deps.Config.TimestampTolerance), logger, statusStore)
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.Handle(platformwebhook.StatusPathPrefix, status)
	mux.Handle("/", handler)
	listener := &Listener{listen: deps.Config.Listen, handler: mux, dispatcher: dispatcher}
	return &Channel{
		listener:   listener,
		handler:    mux,
		dispatcher: dispatcher,
		reconcile:  reconcilePass,
	}, nil
}

func (c *Channel) Listener() server.Listener {
	if c == nil {
		return nil
	}
	return c.listener
}

func (c *Channel) RunReconcile(ctx context.Context) {
	if c == nil || c.reconcile == nil {
		return
	}
	c.reconcile.Run(ctx)
}

func (c *Channel) Handler() http.Handler {
	if c == nil {
		return nil
	}
	return c.handler
}

func EnvKeySource(envName string) (string, error) {
	value, ok := os.LookupEnv(envName)
	if !ok || value == "" {
		return "", fmt.Errorf("webhook: secret %s is unavailable", envName)
	}
	return value, nil
}
