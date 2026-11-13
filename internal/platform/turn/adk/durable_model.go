package runtimeadk

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"

	adkmodel "google.golang.org/adk/v2/model"

	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/turn"
)

const adkFunctionCallIDPrefix = "adk-"

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
		stopped := false
		var runErr error
		defer func() {
			if recovered := durable.RecoveredPanic(); recovered != nil {
				runErr = recovered
			}
			if runErr != nil && !stopped {
				yield(nil, runErr)
			}
		}()
		key := scope.NextModelCall()
		callPrefix := adkFunctionCallIDPrefix + turnIDFromContext(ctx) + "-" + key
		raw, err := scope.Invocation().RunAction(ctx, key, func(ctx context.Context) ([]byte, error) {
			var chunks []*adkmodel.LLMResponse
			for response, err := range m.inner.GenerateContent(ctx, req, stream) {
				if err != nil {
					return nil, err
				}
				if response == nil || !response.Partial {
					chunks = append(chunks, response)
					continue
				}
				if !yield(response, nil) {
					stopped = true
					return nil, codedError(turn.ErrorCodeTurnCancelled, "model stream consumer stopped", nil)
				}
			}
			nameFunctionCalls(chunks, callPrefix)
			return json.Marshal(chunks)
		})
		if stopped {
			return
		}
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

func nameFunctionCalls(chunks []*adkmodel.LLMResponse, prefix string) {
	n := 0
	for _, chunk := range chunks {
		if chunk == nil || chunk.Content == nil {
			continue
		}
		for _, part := range chunk.Content.Parts {
			if part == nil || part.FunctionCall == nil {
				continue
			}
			if part.FunctionCall.ID == "" {
				part.FunctionCall.ID = fmt.Sprintf("%s-%d", prefix, n)
			}
			n++
		}
	}
}
