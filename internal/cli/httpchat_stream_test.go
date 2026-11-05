package cli

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

type tokenStreamModel struct {
	mu      sync.Mutex
	streams []bool
	chunks  []string
	finish  genai.FinishReason
	failure error
}

func (m *tokenStreamModel) Name() string { return "token-stream" }

func (m *tokenStreamModel) streamFlags() []bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.streams)
}

func hasToolResult(req *adkmodel.LLMRequest) bool {
	for _, content := range req.Contents {
		for _, part := range content.Parts {
			if part.FunctionResponse != nil {
				return true
			}
		}
	}
	return false
}

func (m *tokenStreamModel) GenerateContent(_ context.Context, req *adkmodel.LLMRequest, stream bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	m.mu.Lock()
	m.streams = append(m.streams, stream)
	m.mu.Unlock()
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		if !hasToolResult(req) {
			yield(&adkmodel.LLMResponse{
				Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{
					FunctionCall: &genai.FunctionCall{ID: "call-1", Name: "web_search", Args: map[string]any{"query": "x"}},
				}}},
				TurnComplete:  true,
				FinishReason:  genai.FinishReasonStop,
				UsageMetadata: &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: 4, CandidatesTokenCount: 1, TotalTokenCount: 5},
			}, nil)
			return
		}
		if stream {
			for _, chunk := range m.chunks {
				partial := &adkmodel.LLMResponse{
					Content:       &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: chunk}}},
					Partial:       true,
					UsageMetadata: &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: 10, CandidatesTokenCount: 5, TotalTokenCount: 15},
				}
				if !yield(partial, nil) {
					return
				}
			}
		}
		if m.failure != nil {
			yield(nil, m.failure)
			return
		}
		yield(&adkmodel.LLMResponse{
			Content:       &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: strings.Join(m.chunks, "")}}},
			TurnComplete:  true,
			FinishReason:  m.finish,
			UsageMetadata: &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: 10, CandidatesTokenCount: 5, TotalTokenCount: 15},
		}, nil)
	}
}

type streamedReply struct {
	content   []string
	finish    string
	usage     int64
	done      bool
	errorCode string
}

func decodeStreamedReply(t *testing.T, body string) streamedReply {
	t.Helper()
	var reply streamedReply
	for _, frame := range strings.Split(strings.TrimSuffix(body, "\n\n"), "\n\n") {
		payload, found := strings.CutPrefix(frame, "data: ")
		if !found {
			continue
		}
		if payload == "[DONE]" {
			reply.done = true
			continue
		}
		var decoded struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage *struct {
				TotalTokens int64 `json:"total_tokens"`
			} `json:"usage"`
			Error *struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
			t.Fatalf("decode frame %q: %v", payload, err)
		}
		if decoded.Error != nil {
			reply.errorCode = decoded.Error.Code
		}
		if decoded.Usage != nil {
			reply.usage = decoded.Usage.TotalTokens
		}
		for _, choice := range decoded.Choices {
			if choice.Delta.Content != "" {
				reply.content = append(reply.content, choice.Delta.Content)
			}
			if choice.FinishReason != nil {
				reply.finish = *choice.FinishReason
			}
		}
	}
	return reply
}

func (h *httpChatHarness) sessionEventStats(sessionID string) (partials, deltas int, tokens int64) {
	h.t.Helper()
	rows, err := h.db.QueryContext(h.t.Context(),
		`SELECT kind, payload_json, COALESCE(provider_usage_json, '') FROM runtime_event WHERE session_id = ?`, sessionID)
	if err != nil {
		h.t.Fatalf("query events: %v", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var kind, payload, usage string
		if err := rows.Scan(&kind, &payload, &usage); err != nil {
			h.t.Fatalf("scan event: %v", err)
		}
		if kind == "model.delta" {
			deltas++
		}
		if strings.Contains(payload, `"partial":true`) {
			partials++
		}
		if usage != "" {
			var reported struct {
				Total int64 `json:"totalTokenCount"`
			}
			if err := json.Unmarshal([]byte(usage), &reported); err != nil {
				h.t.Fatalf("decode usage %q: %v", usage, err)
			}
			tokens += reported.Total
		}
	}
	if err := rows.Err(); err != nil {
		h.t.Fatalf("rows: %v", err)
	}
	return partials, deltas, tokens
}

func streamRequestBody(prompt string) string {
	return `{"stream":true,"stream_options":{"include_usage":true},"messages":[` + chatMessage("user", prompt) + `]}`
}

func TestHTTPChatStreamsTokenChunksThroughTheRealRuntime(t *testing.T) {
	model := &tokenStreamModel{chunks: []string{"The ", "answer ", "is 42"}, finish: genai.FinishReasonStop}
	h := newHTTPChatHarnessWithModel(t, "50", model)
	key, _ := h.key("lobby", "public")

	whole := h.post(key, chatJSON(chatMessage("user", "question")))
	if whole.status != http.StatusOK {
		t.Fatalf("non-streaming reply = %d %s", whole.status, whole.body)
	}
	wholeFlags := model.streamFlags()
	streamed := h.post(key, streamRequestBody("question"))
	if streamed.status != http.StatusOK {
		t.Fatalf("streaming reply = %d %s", streamed.status, streamed.body)
	}

	if slices.Contains(wholeFlags, true) {
		t.Errorf("a non-streaming turn asked the model to stream: %v", wholeFlags)
	}
	flags := model.streamFlags()[len(wholeFlags):]
	if len(flags) != 2 || slices.Contains(flags, false) {
		t.Errorf("streaming turn model stream flags = %v, want all true", flags)
	}
	reply := decodeStreamedReply(t, streamed.body)
	if !slices.Equal(reply.content, []string{"The ", "answer ", "is 42"}) {
		t.Errorf("content chunks = %q, want one per provider chunk", reply.content)
	}
	if got := strings.Join(reply.content, ""); got != whole.content(t) {
		t.Errorf("streamed %q, non-streaming %q", got, whole.content(t))
	}
	if !reply.done || reply.finish != "stop" || reply.errorCode != "" {
		t.Errorf("stream end = %+v", reply)
	}

	wholePartials, wholeDeltas, wholeTokens := h.sessionEventStats(whole.session)
	streamPartials, streamDeltas, streamTokens := h.sessionEventStats(streamed.session)
	if wholePartials+wholeDeltas+streamPartials+streamDeltas != 0 {
		t.Errorf("partial events were persisted: whole %d/%d, streamed %d/%d", wholePartials, wholeDeltas, streamPartials, streamDeltas)
	}
	if wholeTokens == 0 || wholeTokens != streamTokens {
		t.Errorf("settled tokens differ: non-streaming %d, streaming %d", wholeTokens, streamTokens)
	}
	if reply.usage != streamTokens {
		t.Errorf("reported usage %d, settled %d", reply.usage, streamTokens)
	}
}

func TestHTTPChatReportsTruncationInBothModes(t *testing.T) {
	model := &tokenStreamModel{chunks: []string{"cut ", "off"}, finish: genai.FinishReasonMaxTokens}
	h := newHTTPChatHarnessWithModel(t, "50", model)
	key, _ := h.key("lobby", "public")

	whole := h.post(key, chatJSON(chatMessage("user", "question")))
	streamed := h.post(key, streamRequestBody("question"))

	var decoded struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(whole.body), &decoded); err != nil || len(decoded.Choices) != 1 {
		t.Fatalf("decode completion %q: %v", whole.body, err)
	}
	if decoded.Choices[0].FinishReason != "length" {
		t.Errorf("non-streaming finish_reason = %q, want length", decoded.Choices[0].FinishReason)
	}
	if reply := decodeStreamedReply(t, streamed.body); reply.finish != "length" || !reply.done {
		t.Errorf("streaming reply = %+v, want finish_reason length", reply)
	}
}

func TestHTTPChatStreamFailureAfterPartialTextEndsWithOneError(t *testing.T) {
	model := &tokenStreamModel{chunks: []string{"The ", "answer"}, failure: errors.New("provider stream broke")}
	h := newHTTPChatHarnessWithModel(t, "50", model)
	key, _ := h.key("lobby", "public")

	streamed := h.post(key, streamRequestBody("question"))

	if streamed.status != http.StatusOK {
		t.Fatalf("status = %d, want 200 once the stream started", streamed.status)
	}
	reply := decodeStreamedReply(t, streamed.body)
	if !slices.Equal(reply.content, []string{"The ", "answer"}) || reply.errorCode != "internal_error" || reply.done || reply.finish != "" {
		t.Errorf("reply = %+v", reply)
	}
	if strings.Count(streamed.body, `"error"`) != 1 || strings.Contains(streamed.body, "provider stream broke") {
		t.Errorf("body = %s", streamed.body)
	}
}
