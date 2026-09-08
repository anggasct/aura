package restate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	restate "github.com/restatedev/sdk-go"
	"github.com/restatedev/sdk-go/ingress"

	"github.com/anggasct/aura/internal/durable"
)

const (
	defaultServiceName = "WorkflowRun"
	signalMethod       = "signal"
	statusMethod       = "status"

	unknownRunState = "unknown"

	defaultCallTimeout = 30 * time.Second
)

type Config struct {
	ServiceName string
	IngressURL  string
}

type signalRequest struct {
	Name    string          `json:"name"`
	Payload json.RawMessage `json:"payload"`
}

type statusResponse struct {
	State  string `json:"state"`
	Detail string `json:"detail"`
}

type Adapter struct {
	client  *ingress.Client
	baseURL string
	service string
	logger  *slog.Logger

	mu          sync.Mutex
	handlers    map[string]durable.Handler
	invocations map[string]string
}

func NewAdapter(cfg Config, logger *slog.Logger) (*Adapter, error) {
	if cfg.IngressURL == "" {
		return nil, errors.New("restate adapter requires an ingress URL")
	}
	service := cfg.ServiceName
	if service == "" {
		service = defaultServiceName
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Adapter{
		client:      ingress.NewClient(cfg.IngressURL),
		baseURL:     cfg.IngressURL,
		service:     service,
		logger:      logger,
		handlers:    map[string]durable.Handler{},
		invocations: map[string]string{},
	}, nil
}

func (a *Adapter) RegisterHandler(name string, fn durable.Handler) {
	if name == "" {
		panic("restate adapter requires a handler name")
	}
	if fn == nil {
		panic("restate adapter requires a handler function")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, duplicate := a.handlers[name]; duplicate {
		panic(fmt.Sprintf("restate adapter already has a handler for %q", name))
	}
	a.handlers[name] = fn
}

func (a *Adapter) registered(name string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	_, ok := a.handlers[name]
	return ok
}

func (a *Adapter) Start(ctx context.Context, req durable.StartRequest) (durable.RunRef, error) {
	if req.Key == "" {
		return durable.RunRef{}, errors.New("restate start requires a key")
	}
	if req.Handler == "" {
		return durable.RunRef{}, errors.New("restate start requires a handler")
	}
	if !a.registered(req.Handler) {
		return durable.RunRef{}, fmt.Errorf("restate start: no handler registered for %q", req.Handler)
	}
	if len(req.Payload) != 0 && !json.Valid(req.Payload) {
		return durable.RunRef{}, fmt.Errorf("restate start: payload for run %q is not valid JSON", req.Key)
	}
	payload := req.Payload
	if len(payload) == 0 {
		payload = []byte(`{}`)
	}
	ctx, cancel := withCallTimeout(ctx)
	defer cancel()
	response, err := ingress.Object[json.RawMessage, json.RawMessage](a.client, a.service, req.Key, req.Handler).
		Send(ctx, json.RawMessage(payload), restate.WithIdempotencyKey(req.Key))
	if err != nil {
		return durable.RunRef{}, fmt.Errorf("restate start run %q: %w", req.Key, mapIngressError(err))
	}
	a.mu.Lock()
	a.invocations[req.Key] = response.Id()
	a.mu.Unlock()
	return durable.RunRef{Key: req.Key}, nil
}

func (a *Adapter) Signal(ctx context.Context, run durable.RunRef, name string, payload []byte) error {
	if run.Key == "" {
		return errors.New("restate signal requires a run key")
	}
	if name == "" {
		return errors.New("restate signal requires a name")
	}
	if len(payload) != 0 && !json.Valid(payload) {
		return fmt.Errorf("restate signal %q on run %q: payload is not valid JSON", name, run.Key)
	}
	toSend := payload
	if len(toSend) == 0 {
		toSend = []byte(`{}`)
	}
	ctx, cancel := withCallTimeout(ctx)
	defer cancel()
	_, err := ingress.Object[signalRequest, json.RawMessage](a.client, a.service, run.Key, signalMethod).
		Request(ctx, signalRequest{Name: name, Payload: json.RawMessage(toSend)})
	if err != nil {
		return fmt.Errorf("restate signal %q on run %q: %w", name, run.Key, mapIngressError(err))
	}
	return nil
}

func (a *Adapter) Cancel(ctx context.Context, run durable.RunRef) error {
	if run.Key == "" {
		return errors.New("restate cancel requires a run key")
	}
	status, err := a.Status(ctx, run)
	if err != nil {
		return err
	}
	if status.State == durable.RunSucceeded || status.State == durable.RunFailed || status.State == durable.RunCancelled {
		return nil
	}
	a.mu.Lock()
	invocationID := a.invocations[run.Key]
	a.mu.Unlock()
	if invocationID == "" {
		return fmt.Errorf("%w: invocation handle for run %q is not available", durable.ErrUnknownRun, run.Key)
	}
	ctx, cancel := withCallTimeout(ctx)
	defer cancel()
	if err := cancelInvocation(ctx, a.baseURL, invocationID); err != nil {
		return fmt.Errorf("restate cancel run %q: %w", run.Key, err)
	}
	return nil
}

func (a *Adapter) Status(ctx context.Context, run durable.RunRef) (durable.RunStatus, error) {
	if run.Key == "" {
		return durable.RunStatus{}, errors.New("restate status requires a run key")
	}
	ctx, cancel := withCallTimeout(ctx)
	defer cancel()
	response, err := ingress.Object[json.RawMessage, statusResponse](a.client, a.service, run.Key, statusMethod).
		Request(ctx, json.RawMessage(`{}`))
	if err != nil {
		return durable.RunStatus{}, fmt.Errorf("restate status run %q: %w", run.Key, mapIngressError(err))
	}
	if response.State == unknownRunState {
		return durable.RunStatus{}, fmt.Errorf("%w: %s", durable.ErrUnknownRun, run.Key)
	}
	state, err := parseRunState(response.State)
	if err != nil {
		return durable.RunStatus{}, fmt.Errorf("restate status run %q: %w", run.Key, err)
	}
	return durable.RunStatus{State: state, Detail: response.Detail}, nil
}

func withCallTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, defaultCallTimeout)
}

func parseRunState(state string) (durable.RunState, error) {
	switch durable.RunState(state) {
	case durable.RunRunning, durable.RunSuspended, durable.RunSucceeded, durable.RunFailed, durable.RunCancelled:
		return durable.RunState(state), nil
	default:
		return "", fmt.Errorf("unknown run state %q", state)
	}
}

func mapIngressError(err error) error {
	if err == nil {
		return nil
	}
	var notFound *ingress.InvocationNotFoundError
	if errors.As(err, &notFound) {
		return fmt.Errorf("%w: %w", durable.ErrUnknownRun, err)
	}
	return err
}

func cancelInvocation(ctx context.Context, baseURL, invocationID string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodDelete, baseURL+"/restate/invocation/"+invocationID, http.NoBody)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: defaultCallTimeout}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode == http.StatusNotFound {
		return nil
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("cancel returned %s", response.Status)
	}
	return nil
}
