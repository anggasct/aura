package runtimeadk

import (
	"context"
	"crypto/rand"
	"iter"
	"strings"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

const callIDPrefix = "call_"

type namedCallsModel struct {
	inner adkmodel.LLM
}

func nameCalls(inner adkmodel.LLM) adkmodel.LLM {
	return &namedCallsModel{inner: inner}
}

func (m *namedCallsModel) Name() string { return m.inner.Name() }

func (m *namedCallsModel) GenerateContent(ctx context.Context, req *adkmodel.LLMRequest, stream bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		for response, err := range m.inner.GenerateContent(ctx, req, stream) {
			if err == nil {
				response = withNamedCalls(response)
			}
			if !yield(response, err) {
				return
			}
		}
	}
}

func withNamedCalls(response *adkmodel.LLMResponse) *adkmodel.LLMResponse {
	if response == nil || response.Partial || response.Content == nil || !hasFunctionCall(response.Content.Parts) {
		return response
	}
	content := *response.Content
	content.Parts = make([]*genai.Part, len(response.Content.Parts))
	for i, part := range response.Content.Parts {
		content.Parts[i] = part
		if part == nil || part.FunctionCall == nil {
			continue
		}
		call := *part.FunctionCall
		call.ID = newCallID()
		named := *part
		named.FunctionCall = &call
		content.Parts[i] = &named
	}
	named := *response
	named.Content = &content
	return &named
}

func hasFunctionCall(parts []*genai.Part) bool {
	for _, part := range parts {
		if part != nil && part.FunctionCall != nil {
			return true
		}
	}
	return false
}

func newCallID() string {
	return callIDPrefix + strings.ToLower(rand.Text())
}
