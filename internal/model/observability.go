package model

import (
	"context"
	"errors"
	"log/slog"
	"time"

	adkmodel "google.golang.org/adk/v2/model"
)

type taskContextKey struct{}

func WithTask(ctx context.Context, task string) context.Context {
	return context.WithValue(ctx, taskContextKey{}, task)
}

type modelTelemetry struct {
	logger   *slog.Logger
	protocol string
	model    string
	started  time.Time
	retries  int
	logged   bool
}

func newModelTelemetry(logger *slog.Logger, protocol, model string) *modelTelemetry {
	if logger == nil {
		logger = slog.Default()
	}
	return &modelTelemetry{logger: logger, protocol: protocol, model: model, started: time.Now()}
}

func (t *modelTelemetry) finish(ctx context.Context, response *adkmodel.LLMResponse, err error) {
	if t.logged {
		return
	}
	t.logged = true
	status := "ok"
	if err != nil {
		status = "error"
		if code, ok := CodeOf(err); ok {
			status = string(code)
		} else if errors.Is(err, context.Canceled) {
			status = "canceled"
		} else if errors.Is(err, context.DeadlineExceeded) {
			status = "deadline_exceeded"
		}
	}
	var inputTokens, outputTokens int32
	if response != nil && response.UsageMetadata != nil {
		inputTokens = response.UsageMetadata.PromptTokenCount
		outputTokens = response.UsageMetadata.CandidatesTokenCount
	}
	task := "unknown"
	if value, ok := ctx.Value(taskContextKey{}).(string); ok && value != "" {
		task = value
	}
	t.logger.InfoContext(ctx, "model request",
		"protocol", t.protocol,
		"model", t.model,
		"task", task,
		"latency_ms", time.Since(t.started).Milliseconds(),
		"status", status,
		"input_tokens", inputTokens,
		"output_tokens", outputTokens,
		"retry_count", t.retries,
	)
}

func (t *modelTelemetry) finishIfNeeded(ctx context.Context) {
	t.finish(ctx, nil, nil)
}
