package model

import (
	"context"
	"errors"
	"iter"
	"strings"
	"testing"
	"time"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/anggasct/aura/internal/config"
)

type mockCandidateLLM struct {
	name             string
	responses        []*adkmodel.LLMResponse
	streamResponses  []*adkmodel.LLMResponse
	generateErr      error
	errAfterChunk    int
	recordedRequests []*adkmodel.LLMRequest
}

func (m *mockCandidateLLM) Name() string { return m.name }

func (m *mockCandidateLLM) GenerateContent(_ context.Context, req *adkmodel.LLMRequest, stream bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	m.recordedRequests = append(m.recordedRequests, req)
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		if !stream {
			if m.generateErr != nil {
				yield(nil, m.generateErr)
				return
			}
			for _, resp := range m.responses {
				if !yield(resp, nil) {
					return
				}
			}
			return
		}

		// Streaming mode
		if m.errAfterChunk == 0 && m.generateErr != nil {
			// Immediate error before any chunks
			yield(nil, m.generateErr)
			return
		}

		for idx, resp := range m.streamResponses {
			if !yield(resp, nil) {
				return
			}
			if m.errAfterChunk > 0 && idx+1 == m.errAfterChunk && m.generateErr != nil {
				yield(nil, m.generateErr)
				return
			}
		}
	}
}

func TestFallback_TransientAndRateLimitAdvances(t *testing.T) {
	primaryDef := validDefinition(config.ProtocolOpenAIChatCompat, config.ModelCapabilities{Streaming: true, Tools: true})
	backupDef := validDefinition(config.ProtocolAnthropicMessages, config.ModelCapabilities{Streaming: true, Tools: true})

	defs := map[string]config.ModelDefinition{
		"primary": primaryDef,
		"backup":  backupDef,
	}

	route := config.ModelRoute{
		Candidates:          []string{"primary", "backup"},
		MaxProviderAttempts: 4,
		RetryDelayBudget:    config.Duration(20 * time.Second),
	}

	primaryMock := &mockCandidateLLM{
		name:        "primary",
		generateErr: codedError(ErrorCodeRateLimited, ErrRateLimited, "http 429"),
	}
	backupMock := &mockCandidateLLM{
		name: "backup",
		responses: []*adkmodel.LLMResponse{
			{Content: &genai.Content{Parts: []*genai.Part{{Text: "success from backup"}}}},
		},
	}

	resolver := MapAdapterResolver{
		"primary": primaryMock,
		"backup":  backupMock,
	}

	fallback := NewFallbackAdapter("primary-route", route, defs, nil, resolver)

	ctx := context.Background()
	req := &adkmodel.LLMRequest{
		Contents: []*genai.Content{{
			Role:  "user",
			Parts: []*genai.Part{{Text: "hello"}},
		}},
	}

	var gotResp *adkmodel.LLMResponse
	var gotErr error
	for resp, err := range fallback.GenerateContent(ctx, req, false) {
		if err != nil {
			gotErr = err
			break
		}
		gotResp = resp
	}

	if gotErr != nil {
		t.Fatalf("unexpected fallback error: %v", gotErr)
	}
	if gotResp == nil || gotResp.Content.Parts[0].Text != "success from backup" {
		t.Fatalf("unexpected response: %+v", gotResp)
	}

	// Verify both models were attempted in strict sequence
	if len(primaryMock.recordedRequests) != 1 {
		t.Errorf("primary should be attempted once")
	}
	if len(backupMock.recordedRequests) != 1 {
		t.Errorf("backup should be attempted once")
	}
}

func TestFallback_PolicyRejectionDoesNotFallback(t *testing.T) {
	primaryDef := validDefinition(config.ProtocolOpenAIChatCompat, config.ModelCapabilities{Streaming: true, Tools: true})
	backupDef := validDefinition(config.ProtocolAnthropicMessages, config.ModelCapabilities{Streaming: true, Tools: true})

	defs := map[string]config.ModelDefinition{
		"primary": primaryDef,
		"backup":  backupDef,
	}

	route := config.ModelRoute{Candidates: []string{"primary", "backup"}}

	primaryMock := &mockCandidateLLM{
		name:        "primary",
		generateErr: codedError(ErrorCodeContentFiltered, ErrContentFiltered, "blocked by safety filter"),
	}
	backupMock := &mockCandidateLLM{name: "backup"}

	resolver := MapAdapterResolver{
		"primary": primaryMock,
		"backup":  backupMock,
	}

	fallback := NewFallbackAdapter("primary-route", route, defs, nil, resolver)

	ctx := context.Background()
	req := &adkmodel.LLMRequest{Contents: []*genai.Content{{Role: "user", Parts: []*genai.Part{{Text: "prompt"}}}}}

	var gotErr error
	for _, err := range fallback.GenerateContent(ctx, req, false) {
		if err != nil {
			gotErr = err
			break
		}
	}

	wantCode(t, gotErr, ErrorCodeContentFiltered)

	// Backup model must NEVER be attempted when primary hits a policy filter
	if len(backupMock.recordedRequests) != 0 {
		t.Errorf("backup model should not be attempted on policy filter rejection")
	}
}

func TestFallback_AuthFailureDoesNotFallback(t *testing.T) {
	defs := map[string]config.ModelDefinition{
		"primary": validDefinition(config.ProtocolOpenAIChatCompat, config.ModelCapabilities{}),
		"backup":  validDefinition(config.ProtocolAnthropicMessages, config.ModelCapabilities{}),
	}
	route := config.ModelRoute{Candidates: []string{"primary", "backup"}}

	primaryMock := &mockCandidateLLM{
		name:        "primary",
		generateErr: codedError(ErrorCodeAuthFailed, ErrAuthFailed, "http 401"),
	}
	backupMock := &mockCandidateLLM{name: "backup"}

	resolver := MapAdapterResolver{"primary": primaryMock, "backup": backupMock}
	fallback := NewFallbackAdapter("route", route, defs, nil, resolver)

	var gotErr error
	for _, err := range fallback.GenerateContent(context.Background(), &adkmodel.LLMRequest{}, false) {
		if err != nil {
			gotErr = err
			break
		}
	}

	wantCode(t, gotErr, ErrorCodeAuthFailed)
	if len(backupMock.recordedRequests) != 0 {
		t.Errorf("backup must not be attempted on auth failure")
	}
}

func TestFallback_ObservableBoundaryStreamPreventsRestart(t *testing.T) {
	defs := map[string]config.ModelDefinition{
		"primary": validDefinition(config.ProtocolOpenAIChatCompat, config.ModelCapabilities{Streaming: true}),
		"backup":  validDefinition(config.ProtocolAnthropicMessages, config.ModelCapabilities{Streaming: true}),
	}
	route := config.ModelRoute{Candidates: []string{"primary", "backup"}}

	// Primary emits 1 chunk, then fails mid-stream
	primaryMock := &mockCandidateLLM{
		name: "primary",
		streamResponses: []*adkmodel.LLMResponse{
			{Content: &genai.Content{Parts: []*genai.Part{{Text: "partial response"}}}},
			{Content: &genai.Content{Parts: []*genai.Part{{Text: "more text"}}}},
		},
		errAfterChunk: 1,
		generateErr:   errors.New("connection reset by peer"),
	}
	backupMock := &mockCandidateLLM{name: "backup"}

	resolver := MapAdapterResolver{"primary": primaryMock, "backup": backupMock}
	fallback := NewFallbackAdapter("route", route, defs, nil, resolver)

	var chunks []*adkmodel.LLMResponse
	var gotErr error
	for resp, err := range fallback.GenerateContent(context.Background(), &adkmodel.LLMRequest{}, true) {
		if err != nil {
			gotErr = err
			break
		}
		chunks = append(chunks, resp)
	}

	// First chunk was received
	if len(chunks) != 1 || chunks[0].Content.Parts[0].Text != "partial response" {
		t.Fatalf("expected 1 partial chunk, got %v", chunks)
	}

	// Mid-stream error crosses boundary and returns typed boundary error
	wantCode(t, gotErr, ErrorCodeFallbackBoundary)

	// Backup model MUST NOT be invoked because boundary was crossed!
	if len(backupMock.recordedRequests) != 0 {
		t.Errorf("backup model was invoked after boundary was crossed!")
	}
}

func TestFallback_StreamErrorBeforeOutputFallsBack(t *testing.T) {
	defs := map[string]config.ModelDefinition{
		"primary": validDefinition(config.ProtocolOpenAIChatCompat, config.ModelCapabilities{Streaming: true}),
		"backup":  validDefinition(config.ProtocolAnthropicMessages, config.ModelCapabilities{Streaming: true}),
	}
	route := config.ModelRoute{Candidates: []string{"primary", "backup"}}

	// Primary fails immediately before emitting ANY chunk
	primaryMock := &mockCandidateLLM{
		name:          "primary",
		errAfterChunk: 0,
		generateErr:   codedError(ErrorCodeOverloaded, ErrOverloaded, "service overloaded"),
	}
	backupMock := &mockCandidateLLM{
		name: "backup",
		streamResponses: []*adkmodel.LLMResponse{
			{Content: &genai.Content{Parts: []*genai.Part{{Text: "streamed from backup"}}}},
		},
	}

	resolver := MapAdapterResolver{"primary": primaryMock, "backup": backupMock}
	fallback := NewFallbackAdapter("route", route, defs, nil, resolver)

	var chunks []*adkmodel.LLMResponse
	var gotErr error
	for resp, err := range fallback.GenerateContent(context.Background(), &adkmodel.LLMRequest{}, true) {
		if err != nil {
			gotErr = err
			break
		}
		chunks = append(chunks, resp)
	}

	if gotErr != nil {
		t.Fatalf("unexpected error: %v", gotErr)
	}
	if len(chunks) != 1 || chunks[0].Content.Parts[0].Text != "streamed from backup" {
		t.Fatalf("expected backup stream, got %v", chunks)
	}
	if len(backupMock.recordedRequests) != 1 {
		t.Errorf("backup should have been invoked when primary failed before boundary")
	}
}

func TestFallback_AllCandidatesExhaustedReturnsNormalizedError(t *testing.T) {
	defs := map[string]config.ModelDefinition{
		"m1": validDefinition(config.ProtocolOpenAIChatCompat, config.ModelCapabilities{}),
		"m2": validDefinition(config.ProtocolAnthropicMessages, config.ModelCapabilities{}),
	}
	route := config.ModelRoute{Candidates: []string{"m1", "m2"}}

	m1 := &mockCandidateLLM{name: "m1", generateErr: codedError(ErrorCodeRateLimited, ErrRateLimited, "http 429")}
	m2 := &mockCandidateLLM{name: "m2", generateErr: codedError(ErrorCodeOverloaded, ErrOverloaded, "http 503")}

	resolver := MapAdapterResolver{"m1": m1, "m2": m2}
	fallback := NewFallbackAdapter("exhaustion-route", route, defs, nil, resolver)

	var gotErr error
	for _, err := range fallback.GenerateContent(context.Background(), &adkmodel.LLMRequest{}, false) {
		if err != nil {
			gotErr = err
			break
		}
	}

	wantCode(t, gotErr, ErrorCodeFallbackExhausted)

	// Verify error message contains safe aliases and normalized classes, no secrets/raw details
	errMsg := gotErr.Error()
	if !strings.Contains(errMsg, "m1 (rate_limited)") || !strings.Contains(errMsg, "m2 (overloaded)") {
		t.Errorf("expected safe aliases and normalized classes in %q", errMsg)
	}
}

func TestFallback_CanonicalRequestIsolation(t *testing.T) {
	defs := map[string]config.ModelDefinition{
		"primary": validDefinition(config.ProtocolOpenAIChatCompat, config.ModelCapabilities{Tools: true}),
	}
	route := config.ModelRoute{Candidates: []string{"primary"}}

	primaryMock := &mockCandidateLLM{
		name: "primary",
		responses: []*adkmodel.LLMResponse{
			{Content: &genai.Content{Parts: []*genai.Part{{Text: "ok"}}}},
		},
	}
	resolver := MapAdapterResolver{"primary": primaryMock}
	fallback := NewFallbackAdapter("test-route", route, defs, nil, resolver)

	origReq := &adkmodel.LLMRequest{
		Contents: []*genai.Content{{
			Role: "user",
			Parts: []*genai.Part{{
				Text: "user query",
				FunctionCall: &genai.FunctionCall{
					ID:   "call_1",
					Name: "search",
					Args: map[string]any{"query": "aura"},
				},
			}},
		}},
	}

	for range fallback.GenerateContent(context.Background(), origReq, false) {
	}

	if len(primaryMock.recordedRequests) != 1 {
		t.Fatalf("expected 1 recorded request")
	}

	dispatched := primaryMock.recordedRequests[0]
	if dispatched == origReq {
		t.Errorf("dispatched request should be a cloned copy, not pointer identical")
	}

	// Mutate dispatched request to prove isolation
	dispatched.Contents[0].Parts[0].FunctionCall.Args["query"] = "mutated"
	if origReq.Contents[0].Parts[0].FunctionCall.Args["query"] != "aura" {
		t.Errorf("original request was mutated by candidate adapter modification!")
	}
}
