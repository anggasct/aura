package runtimeadk

import (
	"context"
	"iter"
	"slices"
	"strings"
	"sync"
	"testing"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/anggasct/aura/internal/runtime"
	"github.com/anggasct/aura/internal/runtime/engine"
	"github.com/anggasct/aura/internal/runtime/ingress"
	"github.com/anggasct/aura/internal/store"
)

type streamingADKModel struct {
	mu            sync.Mutex
	streams       []bool
	chunks        []string
	finish        genai.FinishReason
	finalTokens   int32
	partialTokens int32
}

func (m *streamingADKModel) Name() string { return "streaming-model" }

func (m *streamingADKModel) GenerateContent(_ context.Context, _ *adkmodel.LLMRequest, stream bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	m.mu.Lock()
	m.streams = append(m.streams, stream)
	m.mu.Unlock()
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		if stream {
			for _, chunk := range m.chunks {
				partial := &adkmodel.LLMResponse{
					Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: chunk}}},
					Partial: true,
					UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
						TotalTokenCount: m.partialTokens,
					},
				}
				if !yield(partial, nil) {
					return
				}
			}
		}
		yield(&adkmodel.LLMResponse{
			Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: strings.Join(m.chunks, "")}}},
			TurnComplete: true,
			FinishReason: m.finish,
			UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
				TotalTokenCount: m.finalTokens,
			},
		}, nil)
	}
}

func registerStreamingModel(t *testing.T, model *streamingADKModel) string {
	t.Helper()
	name := "streaming-model-" + runtimeengine.NewTurnID()
	adkmodel.Register("^"+name+"$", func(context.Context, string) (adkmodel.LLM, error) {
		return model, nil
	})
	return name
}

func streamTurn(turnID string, streamPartial bool, budget int64) *runtime.TurnRequest {
	return &runtime.TurnRequest{
		TurnID: turnID, SessionID: "session-1", PrincipalID: "user-1", Origin: runtime.OriginHTTP,
		Parts:         []runtimeingress.InputPart{{Text: "hello"}},
		StreamPartial: streamPartial,
		Budget:        runtime.Budget{MaxTokens: budget},
	}
}

func runStreamTurn(t *testing.T, executor *ADKExecutor, req *runtime.TurnRequest) ([]store.RuntimeEvent, error) {
	t.Helper()
	var events []store.RuntimeEvent
	for ev, err := range executor.Execute(context.Background(), req) {
		if err != nil {
			return events, err
		}
		events = append(events, ev)
	}
	return events, nil
}

func partialTexts(t *testing.T, events []store.RuntimeEvent) (partials []string, finals int) {
	t.Helper()
	for i := range events {
		if events[i].Kind != store.EventKindADK {
			continue
		}
		adk, err := store.RuntimeEventToADK(&events[i])
		if err != nil {
			t.Fatalf("RuntimeEventToADK: %v", err)
		}
		switch {
		case adk.Partial:
			partials = append(partials, adk.Content.Parts[0].Text)
		case adk.Content != nil && adk.Content.Role == genai.RoleModel:
			finals++
		}
	}
	return partials, finals
}

func TestADKExecutorStreamsOnlyWhenRequested(t *testing.T) {
	tests := []struct {
		name         string
		streamTurn   bool
		wantStream   bool
		wantPartials []string
	}{
		{name: "requested", streamTurn: true, wantStream: true, wantPartials: []string{"par", "tial", " text"}},
		{name: "not requested", streamTurn: false, wantStream: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			model := &streamingADKModel{chunks: []string{"par", "tial", " text"}, finalTokens: 4, partialTokens: 4}
			executor, db, _ := newADKTestExecutor(t, registerStreamingModel(t, model), &fakeBroker{})
			mustCreateSession(t, db, "session-1")

			events, err := runStreamTurn(t, executor, streamTurn("turn-1", tc.streamTurn, 0))
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}

			partials, finals := partialTexts(t, events)
			if !slices.Equal(partials, tc.wantPartials) {
				t.Errorf("partials = %v, want %v", partials, tc.wantPartials)
			}
			if finals != 1 {
				t.Errorf("completed model messages = %d, want 1", finals)
			}
			model.mu.Lock()
			defer model.mu.Unlock()
			if !slices.Equal(model.streams, []bool{tc.wantStream}) {
				t.Errorf("model stream flags = %v, want [%v]", model.streams, tc.wantStream)
			}
		})
	}
}

func TestADKExecutorStreamingKeepsTurnBudgetTotals(t *testing.T) {
	tests := []struct {
		name       string
		streamTurn bool
	}{
		{name: "streaming", streamTurn: true},
		{name: "not streaming", streamTurn: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			model := &streamingADKModel{chunks: []string{"a", "b", "c"}, finalTokens: 10, partialTokens: 9}
			executor, db, _ := newADKTestExecutor(t, registerStreamingModel(t, model), &fakeBroker{})
			mustCreateSession(t, db, "session-1")

			events, err := runStreamTurn(t, executor, streamTurn("turn-1", tc.streamTurn, 12))
			if err != nil {
				t.Fatalf("Execute under a 12 token budget: %v", err)
			}

			var counted int64
			for i := range events {
				adk, err := store.RuntimeEventToADK(&events[i])
				if err != nil {
					t.Fatalf("RuntimeEventToADK: %v", err)
				}
				if !adk.Partial && adk.UsageMetadata != nil {
					counted += int64(adk.UsageMetadata.TotalTokenCount)
				}
			}
			if counted != 10 {
				t.Errorf("settled tokens = %d, want 10", counted)
			}
		})
	}
}

func TestADKExecutorRecordsTheFinishReason(t *testing.T) {
	tests := []struct {
		name       string
		streamTurn bool
	}{
		{name: "streaming", streamTurn: true},
		{name: "not streaming", streamTurn: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			model := &streamingADKModel{chunks: []string{"cut"}, finish: genai.FinishReasonMaxTokens, finalTokens: 4}
			executor, db, _ := newADKTestExecutor(t, registerStreamingModel(t, model), &fakeBroker{})
			mustCreateSession(t, db, "session-1")

			events, err := runStreamTurn(t, executor, streamTurn("turn-1", tc.streamTurn, 0))
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}

			var reasons []genai.FinishReason
			for i := range events {
				if events[i].Kind != store.EventKindADK {
					continue
				}
				adk, err := store.RuntimeEventToADK(&events[i])
				if err != nil {
					t.Fatalf("RuntimeEventToADK: %v", err)
				}
				if !adk.Partial && adk.FinishReason != "" {
					reasons = append(reasons, adk.FinishReason)
				}
			}
			if !slices.Equal(reasons, []genai.FinishReason{genai.FinishReasonMaxTokens}) {
				t.Errorf("finish reasons = %v, want [MAX_TOKENS]", reasons)
			}
		})
	}
}
