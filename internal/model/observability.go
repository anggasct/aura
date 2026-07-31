package model

import (
	"context"
	"log/slog"
	"strings"
	"time"

	adkmodel "google.golang.org/adk/v2/model"
)

type taskContextKey struct{}

func WithTask(ctx context.Context, task string) context.Context {
	return context.WithValue(ctx, taskContextKey{}, task)
}

type modelTelemetry struct {
	ctx      context.Context
	protocol string
	model    string
	started  time.Time
	retries  int
	logged   bool
}

func newModelTelemetry(ctx context.Context, protocol, model string) *modelTelemetry {
	return &modelTelemetry{ctx: ctx, protocol: protocol, model: model, started: time.Now()}
}

func (t *modelTelemetry) finish(response *adkmodel.LLMResponse, err error) {
	if t.logged {
		return
	}
	t.logged = true
	status := "ok"
	if err != nil {
		status = "error"
		if code, ok := CodeOf(err); ok {
			status = string(code)
		} else if strings.Contains(err.Error(), "context canceled") {
			status = "canceled"
		} else if strings.Contains(err.Error(), "context deadline exceeded") {
			status = "deadline_exceeded"
		}
	}
	var inputTokens, outputTokens int32
	if response != nil && response.UsageMetadata != nil {
		inputTokens = response.UsageMetadata.PromptTokenCount
		outputTokens = response.UsageMetadata.CandidatesTokenCount
	}
	task := "unknown"
	if value, ok := t.ctx.Value(taskContextKey{}).(string); ok && value != "" {
		task = value
	}
	slog.Info("model request",
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

func (t *modelTelemetry) finishIfNeeded() {
	t.finish(nil, nil)
}
