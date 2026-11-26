package httpchat

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/health"
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

var _ health.HealthProvider = (*Server)(nil)

func NewServer(handler *Handler, listen string, grace time.Duration, logger *slog.Logger) (*Server, error) {
	switch {
	case handler == nil:
		return nil, fault.New(ErrorCodeInvalidArgument, "http handler must not be nil")
	case listen == "":
		return nil, fault.New(ErrorCodeInvalidArgument, "http listen address must not be empty")
	case grace <= 0:
		return nil, fault.New(ErrorCodeInvalidArgument, "http shutdown grace must be positive")
	case logger == nil:
		return nil, fault.New(ErrorCodeInvalidArgument, "logger must not be nil")
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
		return fault.Wrap(ErrorCodeConnectionFailed, "http listener did not drain in time", shutdownErr)
	case serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed):
		s.down.Store(true)
		return fault.Wrap(ErrorCodeConnectionFailed, "http listener stopped", serveErr)
	default:
		return nil
	}
}

func (s *Server) HealthChecks() []health.RegisteredCheck {
	if s == nil {
		return nil
	}
	return []health.RegisteredCheck{{
		ID:          healthComponent,
		Checker:     s,
		Remediation: RemediationReviewListener,
	}}
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
