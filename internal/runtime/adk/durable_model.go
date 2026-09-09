package runtimeadk

import (
	"context"
	"encoding/json"
	"iter"

	adkmodel "google.golang.org/adk/v2/model"

	"github.com/anggasct/aura/internal/durable"
)

func runTasksSequential(ctx context.Context, tasks []func(context.Context)) {
	for _, task := range tasks {
		task(ctx)
	}
}

type journaledModel struct {
	inner adkmodel.LLM
}

func journalModel(inner adkmodel.LLM) adkmodel.LLM {
	return &journaledModel{inner: inner}
}

func (m *journaledModel) Name() string { return m.inner.Name() }

func (m *journaledModel) GenerateContent(ctx context.Context, req *adkmodel.LLMRequest, stream bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	scope, ok := durable.TurnScopeFrom(ctx)
	if !ok {
		return m.inner.GenerateContent(ctx, req, stream)
	}
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		var runErr error
		defer func() {
			if recovered := durable.RecoveredPanic(); recovered != nil {
				runErr = recovered
			}
			if runErr != nil {
				yield(nil, runErr)
			}
		}()
		raw, err := scope.Invocation().RunAction(ctx, scope.NextOp(), func(ctx context.Context) ([]byte, error) {
			var chunks []*adkmodel.LLMResponse
			for response, err := range m.inner.GenerateContent(ctx, req, false) {
				if err != nil {
					return nil, err
				}
				chunks = append(chunks, response)
			}
			return json.Marshal(chunks)
		})
		if err != nil {
			yield(nil, err)
			return
		}
		var chunks []*adkmodel.LLMResponse
		if err := json.Unmarshal(raw, &chunks); err != nil {
			yield(nil, err)
			return
		}
		for _, chunk := range chunks {
			if !yield(chunk, nil) {
				return
			}
		}
	}
}
