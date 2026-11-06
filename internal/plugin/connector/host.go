package connector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/anggasct/aura/internal/plugin/auth"
	"github.com/anggasct/aura/internal/tools"
)

const RestartInterval = 5 * time.Second

type Session interface {
	Request(ctx context.Context, payload []byte) ([]byte, error)
	Close(ctx context.Context) error
}

type SessionRequest struct {
	RequestID      string
	Name           string
	Executable     string
	Arguments      []string
	WorkingDir     string
	Timeout        time.Duration
	SetupTimeout   time.Duration
	MaxOutputBytes int64
}

type Starter interface {
	StartSession(ctx context.Context, req *SessionRequest) (Session, error)
}

type Tool struct {
	Name        string
	Description string
	Effect      bool
	Display     []string
}

type Config struct {
	ID              string
	Executable      string
	Arguments       []string
	WorkingDir      string
	StartupTimeout  time.Duration
	RequestTimeout  time.Duration
	MaxMessageBytes int64
	Capabilities    []string
	Egress          []string
	Tools           []Tool
	Auth            auth.Strategy
	AuthHosts       []string
}

type call struct {
	tool           *Tool
	arguments      json.RawMessage
	idempotencyKey string
}

type mutation int

const (
	mutationNone mutation = iota
	mutationRefused
	mutationAccepted
)

type callState struct {
	id           string
	steps        int
	sent         bool
	lastMutation mutation
}

type host struct {
	cfg       Config
	starter   Starter
	client    *http.Client
	logger    *slog.Logger
	allowed   map[string]bool
	credited  map[string]bool
	gate      chan struct{}
	session   Session
	described []describedTool
	agreed    []string
	lastStart time.Time
	closed    bool
	alive     atomic.Bool
	calls     atomic.Uint64
	starts    atomic.Uint64
}

func newHost(cfg *Config, starter Starter, client *http.Client, logger *slog.Logger) *host {
	clone := *client
	clone.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &host{
		cfg:      *cfg,
		starter:  starter,
		client:   &clone,
		logger:   logger,
		allowed:  egressSet(cfg.Egress),
		credited: egressSet(cfg.AuthHosts),
		gate:     make(chan struct{}, 1),
	}
}

func egressSet(entries []string) map[string]bool {
	allowed := make(map[string]bool, len(entries))
	for _, entry := range entries {
		entry = strings.ToLower(entry)
		if !strings.Contains(entry, ":") {
			entry += ":443"
		}
		allowed[entry] = true
	}
	return allowed
}

func (h *host) acquire(ctx context.Context) error {
	select {
	case h.gate <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (h *host) release() {
	<-h.gate
}

func (h *host) ensureSession(ctx, callCtx context.Context) (Session, *Error) {
	if h.closed {
		return nil, Errorf(ErrBackendUnavailable, "the connector is shut down")
	}
	if h.session != nil {
		return h.session, nil
	}
	if !h.lastStart.IsZero() && time.Since(h.lastStart) < RestartInterval {
		return nil, Errorf(ErrBackendUnavailable, "the connector restarted too recently")
	}
	h.lastStart = time.Now()
	session, err := h.starter.StartSession(context.WithoutCancel(callCtx), &SessionRequest{
		RequestID:      fmt.Sprintf("connector-%s-%d", h.cfg.ID, h.starts.Add(1)),
		Name:           h.cfg.ID,
		Executable:     h.cfg.Executable,
		Arguments:      append([]string(nil), h.cfg.Arguments...),
		WorkingDir:     h.cfg.WorkingDir,
		Timeout:        h.cfg.RequestTimeout,
		SetupTimeout:   h.cfg.StartupTimeout,
		MaxOutputBytes: h.cfg.MaxMessageBytes,
	})
	if err != nil || session == nil {
		return nil, Wrap(ErrBackendUnavailable, err, "the connector session could not start")
	}
	h.session = session
	h.alive.Store(true)
	raw, fail := h.exchange(ctx, callCtx, session, &describeMessage{V: Version, Type: typeDescribe, HostCapabilities: slices.Clone(hostCapabilities)})
	var described *description
	if fail == nil {
		described, fail = decodeTools(raw)
	}
	if fail != nil {
		h.drop(ctx)
		return nil, fail
	}
	h.described = described.tools
	h.agreed = agreedCapabilities(described.capabilities)
	return session, nil
}

func (h *host) drop(ctx context.Context) {
	if h.session == nil {
		return
	}
	session := h.session
	h.session = nil
	h.alive.Store(false)
	if err := session.Close(context.WithoutCancel(ctx)); err != nil {
		h.logger.WarnContext(ctx, "connector session close failed", "component", "plugin_connector", "plugin", h.cfg.ID)
	}
}

func (h *host) close(ctx context.Context) error {
	if err := h.acquire(ctx); err != nil {
		return err
	}
	defer h.release()
	h.closed = true
	h.drop(ctx)
	return nil
}

func (h *host) exchange(ctx, callCtx context.Context, session Session, message any) ([]byte, *Error) {
	payload, err := json.Marshal(message)
	if err != nil {
		return nil, Wrap(ErrRPCInvalid, err, "the host message cannot be encoded")
	}
	raw, err := session.Request(callCtx, payload)
	if err != nil {
		return nil, h.requestFailure(ctx, callCtx, err)
	}
	return raw, nil
}

func (h *host) requestFailure(ctx, callCtx context.Context, err error) *Error {
	var coded *Error
	switch {
	case ctx.Err() != nil:
		return Wrap(ErrBackendUnavailable, ctx.Err(), "the call was cancelled")
	case callCtx.Err() != nil:
		return Errorf(ErrRPCLimit, "the call exceeded its time limit")
	case errors.As(err, &coded):
		return &Error{Code: coded.Code, Detail: coded.Detail}
	default:
		return Errorf(ErrBackendUnavailable, "the connector session ended")
	}
}

func (h *host) describe(ctx context.Context) ([]describedTool, *Error) {
	callCtx, cancel := context.WithTimeout(ctx, h.cfg.RequestTimeout)
	defer cancel()
	if err := h.acquire(callCtx); err != nil {
		return nil, Wrap(ErrBackendUnavailable, err, "the connector is busy")
	}
	defer h.release()
	if _, fail := h.ensureSession(ctx, callCtx); fail != nil {
		return nil, fail
	}
	return slices.Clone(h.described), nil
}

func (h *host) describes(tool *Tool) bool {
	index := slices.IndexFunc(h.described, func(described describedTool) bool { return described.Name == tool.Name })
	return index >= 0 && tools.CheckDisplaySchema(tool.Display, h.described[index].InputSchema) == nil
}

func (h *host) invoke(ctx context.Context, request *call) (json.RawMessage, *Error) {
	callCtx, cancel := context.WithTimeout(ctx, h.cfg.RequestTimeout)
	defer cancel()
	if err := h.acquire(callCtx); err != nil {
		if ctx.Err() != nil {
			return nil, Wrap(ErrBackendUnavailable, ctx.Err(), "the call was cancelled")
		}
		return nil, Errorf(ErrRPCLimit, "the call exceeded its time limit")
	}
	defer h.release()
	session, fail := h.ensureSession(ctx, callCtx)
	if fail != nil {
		return nil, fail
	}
	if !h.describes(request.tool) {
		h.drop(ctx)
		return nil, Errorf(ErrToolDrift, "the connector no longer describes this tool as approved")
	}
	state := &callState{id: fmt.Sprintf("%s-%d", h.cfg.ID, h.calls.Add(1))}
	arguments := request.arguments
	if len(arguments) == 0 {
		arguments = json.RawMessage(`{}`)
	}
	var message any = &invokeMessage{
		V: Version, Type: typeInvoke, Call: state.id, Tool: request.tool.Name, Arguments: arguments, IdempotencyKey: request.idempotencyKey,
	}
	for {
		raw, fail := h.exchange(ctx, callCtx, session, message)
		var parsed *reply
		if fail == nil {
			parsed, fail = decodeReply(raw, state.id, h.cfg.MaxMessageBytes)
		}
		if fail != nil {
			return nil, h.abort(ctx, state, fail)
		}
		switch {
		case parsed.result != nil:
			if request.tool.Effect && state.lastMutation != mutationAccepted {
				return nil, h.finish(state, Errorf(ErrEffectNotPerformed, "the connector ended an effect without a confirmed state-changing request"))
			}
			return parsed.result, nil
		case parsed.http == nil:
			return nil, h.finish(state, &Error{Code: ErrConnector, Reported: parsed.code})
		}
		state.steps++
		if state.steps > maxHTTPSteps {
			return nil, h.abort(ctx, state, Errorf(ErrRPCLimit, "the connector exceeded the request step bound"))
		}
		result, fail := h.perform(callCtx, ctx, request.tool, state, parsed.http)
		if fail != nil {
			return nil, h.abort(ctx, state, fail)
		}
		message = result
	}
}

func (h *host) finish(state *callState, fail *Error) *Error {
	fail.RequestSent = state.sent
	return fail
}

func (h *host) abort(ctx context.Context, state *callState, fail *Error) *Error {
	h.drop(ctx)
	h.logger.WarnContext(ctx, "connector call ended", "component", "plugin_connector", "plugin", h.cfg.ID, "code", string(fail.Code))
	return h.finish(state, fail)
}
