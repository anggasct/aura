package httpchat

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/anggasct/aura/internal/health"
)

const (
	RemediationReviewListener = "review-http-listener"
	FindingListenerDown       = "http_listener_down"
	healthComponent           = "http"
	serverName                = "http-chat"

	headerTimeout = 5 * time.Second
	readTimeout   = 30 * time.Second
	idleTimeout   = time.Minute
	cancelGrace   = 5 * time.Second
)

type Server struct {
	handler http.Handler
	listen  string
	grace   time.Duration
	logger  *slog.Logger
	down    atomic.Bool
}

func NewServer(handler *Handler, listen string, grace time.Duration, logger *slog.Logger) (*Server, error) {
	switch {
	case handler == nil:
		return nil, Errorf(ErrorCodeInvalidArgument, "http handler must not be nil")
	case listen == "":
		return nil, Errorf(ErrorCodeInvalidArgument, "http listen address must not be empty")
	case grace <= 0:
		return nil, Errorf(ErrorCodeInvalidArgument, "http shutdown grace must be positive")
	case logger == nil:
		return nil, Errorf(ErrorCodeInvalidArgument, "logger must not be nil")
	}
	return &Server{handler: handler, listen: listen, grace: grace, logger: logger}, nil
}

func (s *Server) Name() string { return serverName }

func (s *Server) Start(ctx context.Context) error {
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", s.listen)
	if err != nil {
		s.down.Store(true)
		s.logger.ErrorContext(ctx, "http listener is not bindable", "component", "http", "error", err)
		<-ctx.Done()
		return nil
	}
	return s.Serve(ctx, listener)
}

func (s *Server) Serve(ctx context.Context, listener net.Listener) error {
	s.down.Store(false)
	deliveries, cancelDeliveries := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelDeliveries()
	server := &http.Server{
		Handler:           s.handler,
		ReadHeaderTimeout: headerTimeout,
		ReadTimeout:       readTimeout,
		IdleTimeout:       idleTimeout,
		BaseContext:       func(net.Listener) context.Context { return deliveries },
	}
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	var serveErr error
	select {
	case serveErr = <-served:
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.grace)
	defer cancel()
	shutdownErr := server.Shutdown(shutdownCtx)
	if shutdownErr != nil {
		cancelDeliveries()
		cancelCtx, cancelFinal := context.WithTimeout(context.WithoutCancel(ctx), cancelGrace)
		defer cancelFinal()
		if server.Shutdown(cancelCtx) != nil {
			_ = server.Close()
		}
	}
	if serveErr == nil {
		serveErr = <-served
	}
	switch {
	case shutdownErr != nil:
		return &Error{Code: ErrorCodeConnectionFailed, Detail: "http listener did not drain in time", Cause: shutdownErr}
	case serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed):
		s.down.Store(true)
		return &Error{Code: ErrorCodeConnectionFailed, Detail: "http listener stopped", Cause: serveErr}
	default:
		return nil
	}
}

func (s *Server) Check(context.Context) []health.Finding {
	if !s.down.Load() {
		return nil
	}
	return []health.Finding{{
		Component:   healthComponent,
		Code:        FindingListenerDown,
		Status:      health.StatusDown,
		Detail:      "http chat listener is not serving",
		Remediation: RemediationReviewListener,
	}}
}
