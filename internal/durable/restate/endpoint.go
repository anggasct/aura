package restate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/restatedev/sdk-go/server"

	"github.com/anggasct/aura/internal/durable"
)

const defaultHandlerAddr = "127.0.0.1:9080"

type EndpointConfig struct {
	ServiceName string
	HandlerAddr string
}

type Endpoint struct {
	runtime  *server.Restate
	registry *handlerRegistry
	logger   *slog.Logger
	addr     string
	service  string

	mu       sync.Mutex
	server   *http.Server
	bound    net.Addr
	sessions *SessionStores
	inflight map[*inflightCall]struct{}
}

type inflightCall struct {
	cancel context.CancelFunc
}

func NewEndpoint(cfg EndpointConfig, logger *slog.Logger) (*Endpoint, error) {
	addr := cfg.HandlerAddr
	if addr == "" {
		addr = defaultHandlerAddr
	}
	service := cfg.ServiceName
	if service == "" {
		service = defaultServiceName
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Endpoint{
		runtime:  server.NewRestate().WithLogger(logger.Handler(), true),
		registry: newHandlerRegistry(),
		logger:   logger,
		addr:     addr,
		service:  service,
		inflight: map[*inflightCall]struct{}{},
	}, nil
}

func (e *Endpoint) RegisterHandler(name string, fn durable.Handler) {
	e.registry.register(name, fn)
}

func (e *Endpoint) RegisterSessionTurns(stores SessionStores) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.sessions = &stores
}

func (e *Endpoint) Addr() string {
	return e.addr
}

func (e *Endpoint) BoundAddr() net.Addr {
	for range 100 {
		e.mu.Lock()
		bound := e.bound
		e.mu.Unlock()
		if bound != nil {
			return bound
		}
		time.Sleep(10 * time.Millisecond)
	}
	return nil
}

func (e *Endpoint) Start(ctx context.Context) error {
	handler, err := e.boundHandler(ctx)
	if err != nil {
		return err
	}
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", e.addr)
	if err != nil {
		return fmt.Errorf("restate handler listen on %s: %w", e.addr, err)
	}
	var protocols http.Protocols
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)
	e.mu.Lock()
	e.server = &http.Server{
		Handler:           handler,
		Protocols:         &protocols,
		ReadHeaderTimeout: 5 * time.Second,
	}
	e.bound = listener.Addr()
	srv := e.server
	e.mu.Unlock()
	go func() {
		<-ctx.Done()
		e.cancelInflight()
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	if err := srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("restate handler serve: %w", err)
	}
	return nil
}

func (e *Endpoint) cancelInflight() {
	e.mu.Lock()
	defer e.mu.Unlock()
	for call := range e.inflight {
		call.cancel()
	}
}

func (e *Endpoint) Stop(ctx context.Context) error {
	e.cancelInflight()
	e.mu.Lock()
	srv := e.server
	e.mu.Unlock()
	if srv == nil {
		return errors.New("restate handler is not running")
	}
	return srv.Shutdown(ctx)
}

func (e *Endpoint) boundHandler(ctx context.Context) (http.HandlerFunc, error) {
	e.runtime.Bind(buildService(ctx, e.service, e.registry, e.logger))
	e.mu.Lock()
	binding := e.sessions
	e.mu.Unlock()
	if binding != nil {
		e.runtime.Bind(buildSessionService(*binding, e.logger))
	}
	inner, err := e.runtime.Handler()
	if err != nil {
		return nil, err
	}
	return func(w http.ResponseWriter, r *http.Request) {
		callCtx, cancel := context.WithCancel(r.Context())
		call := &inflightCall{cancel: cancel}
		e.mu.Lock()
		e.inflight[call] = struct{}{}
		e.mu.Unlock()
		defer func() {
			cancel()
			e.mu.Lock()
			delete(e.inflight, call)
			e.mu.Unlock()
		}()
		inner.ServeHTTP(w, r.WithContext(callCtx))
	}, nil
}
