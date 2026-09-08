package restate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	restate "github.com/restatedev/sdk-go"

	"github.com/anggasct/aura/internal/durable"
)

const (
	runHandlerName    = "run"
	signalHandlerName = "signal"
	statusHandlerName = "status"

	runStateKey  = "run_state"
	runDetailKey = "run_detail"
)

type runEnvelope struct {
	Handler string          `json:"handler"`
	Payload json.RawMessage `json:"payload"`
}

type runOutput struct {
	State  string `json:"state"`
	Detail string `json:"detail"`
}

func buildService(ctx context.Context, serviceName string, registry *handlerRegistry, logger *slog.Logger) restate.ServiceDefinition {
	if logger == nil {
		logger = slog.Default()
	}
	logger.InfoContext(ctx, "binding durable handler service", "service", serviceName)
	definition := restate.NewWorkflow(serviceName)
	definition.Handler(runHandlerName, restate.NewWorkflowHandler(func(wctx restate.WorkflowContext, input runEnvelope) (runOutput, error) {
		return serveRun(wctx, registry, logger, input)
	}))
	definition.Handler(signalHandlerName, restate.NewWorkflowSharedHandler(func(wctx restate.WorkflowSharedContext, input signalRequest) (bool, error) {
		promise := restate.Promise[json.RawMessage](wctx, input.Name)
		if err := promise.Resolve(input.Payload); err != nil {
			return false, restate.ToTerminalError(fmt.Errorf("resolve signal %q: %w", input.Name, err), restate.WithErrorCode(http.StatusConflict))
		}
		return true, nil
	}))
	definition.Handler(statusHandlerName, restate.NewWorkflowSharedHandler(func(wctx restate.WorkflowSharedContext, _ json.RawMessage) (statusResponse, error) {
		state, err := restate.Get[string](wctx, runStateKey)
		if err != nil {
			return statusResponse{}, err
		}
		if state == "" {
			return statusResponse{State: unknownRunState}, nil
		}
		detail, err := restate.Get[string](wctx, runDetailKey)
		if err != nil {
			return statusResponse{}, err
		}
		return statusResponse{State: state, Detail: detail}, nil
	}))
	return definition
}

func serveRun(ctx context.Context, registry *handlerRegistry, logger *slog.Logger, input runEnvelope) (runOutput, error) {
	wctx, ok := ctx.(restate.WorkflowContext)
	if !ok {
		return runOutput{}, restate.ToTerminalError(errors.New("durable run requires a workflow context"), restate.WithErrorCode(http.StatusInternalServerError))
	}
	handler, ok := registry.get(input.Handler)
	if !ok {
		return runOutput{}, restate.ToTerminalError(fmt.Errorf("no handler registered for %q", input.Handler), restate.WithErrorCode(http.StatusBadRequest))
	}
	restate.Set(wctx, runStateKey, string(durable.RunRunning))
	restate.Set(wctx, runDetailKey, "")
	payload := []byte(input.Payload)
	if len(payload) == 0 {
		payload = []byte(`{}`)
	}
	inv := &invocation{runtime: wctx, ref: durable.RunRef{Key: restate.Key(wctx)}, payload: payload}
	if err := handler(ctx, inv); err != nil {
		detail := err.Error()
		restate.Set(wctx, runStateKey, string(durable.RunFailed))
		restate.Set(wctx, runDetailKey, detail)
		if ctx.Err() != nil {
			logger.InfoContext(ctx, "durable run cancelled", "key", restate.Key(wctx))
			return runOutput{State: string(durable.RunCancelled), Detail: detail}, ctx.Err()
		}
		logger.InfoContext(ctx, "durable run failed", "key", restate.Key(wctx))
		return runOutput{State: string(durable.RunFailed), Detail: detail}, nil
	}
	restate.Set(wctx, runStateKey, string(durable.RunSucceeded))
	return runOutput{State: string(durable.RunSucceeded)}, nil
}
