package model

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

const (
	callIDA = "call_aaaaaaaaaaaaaaaaaaaaaaaaaa"
	callIDB = "call_bbbbbbbbbbbbbbbbbbbbbbbbbb"
)

type sentCall struct {
	ID        string
	Arguments string
}

type sentResult struct {
	ID     string
	Output string
}

func twoCallsReversedRequest() *adkmodel.LLMRequest {
	return &adkmodel.LLMRequest{
		Model: "model-x",
		Contents: []*genai.Content{
			{Role: genai.RoleUser, Parts: []*genai.Part{{Text: "look both up"}}},
			{Role: genai.RoleModel, Parts: []*genai.Part{
				{FunctionCall: &genai.FunctionCall{ID: callIDA, Name: "search", Args: map[string]any{"q": "a"}}},
				{FunctionCall: &genai.FunctionCall{ID: callIDB, Name: "search", Args: map[string]any{"q": "b"}}},
			}},
			{Role: genai.RoleUser, Parts: []*genai.Part{
				{FunctionResponse: &genai.FunctionResponse{ID: callIDB, Name: "search", Response: map[string]any{"hit": "b"}}},
				{FunctionResponse: &genai.FunctionResponse{ID: callIDA, Name: "search", Response: map[string]any{"hit": "a"}}},
			}},
		},
	}
}

func openAIChatSent(t *testing.T, body []byte) (calls []sentCall, results []sentResult) {
	t.Helper()
	var decoded struct {
		Messages []struct {
			Role       string `json:"role"`
			Content    string `json:"content"`
			ToolCallID string `json:"tool_call_id"`
			ToolCalls  []struct {
				ID       string `json:"id"`
				Function struct {
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode the chat request: %v", err)
	}
	for _, message := range decoded.Messages {
		for _, call := range message.ToolCalls {
			calls = append(calls, sentCall{ID: call.ID, Arguments: call.Function.Arguments})
		}
		if message.Role == "tool" {
			results = append(results, sentResult{ID: message.ToolCallID, Output: message.Content})
		}
	}
	return calls, results
}

func openAIResponsesSent(t *testing.T, body []byte) (calls []sentCall, results []sentResult) {
	t.Helper()
	var decoded struct {
		Input []struct {
			Type      string `json:"type"`
			CallID    string `json:"call_id"`
			Arguments string `json:"arguments"`
			Output    string `json:"output"`
		} `json:"input"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode the responses request: %v", err)
	}
	for _, item := range decoded.Input {
		switch item.Type {
		case "function_call":
			calls = append(calls, sentCall{ID: item.CallID, Arguments: item.Arguments})
		case "function_call_output":
			results = append(results, sentResult{ID: item.CallID, Output: item.Output})
		}
	}
	return calls, results
}

func anthropicSent(t *testing.T, body []byte) (calls []sentCall, results []sentResult) {
	t.Helper()
	var decoded struct {
		Messages []struct {
			Content []struct {
				Type      string          `json:"type"`
				ID        string          `json:"id"`
				Input     json.RawMessage `json:"input"`
				ToolUseID string          `json:"tool_use_id"`
				Content   string          `json:"content"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode the messages request: %v", err)
	}
	for _, message := range decoded.Messages {
		for _, block := range message.Content {
			switch block.Type {
			case "tool_use":
				calls = append(calls, sentCall{ID: block.ID, Arguments: string(block.Input)})
			case "tool_result":
				results = append(results, sentResult{ID: block.ToolUseID, Output: block.Content})
			}
		}
	}
	return calls, results
}

func idsOfCalls(calls []sentCall) []string {
	ids := make([]string, len(calls))
	for i, call := range calls {
		ids[i] = call.ID
	}
	return ids
}

func idsOfResults(results []sentResult) []string {
	ids := make([]string, len(results))
	for i, result := range results {
		ids[i] = result.ID
	}
	return ids
}

func TestEveryProtocolSendsTheCallIDBackWithItsResult(t *testing.T) {
	req := twoCallsReversedRequest()
	cases := []struct {
		name   string
		build  func() ([]byte, error)
		decode func(*testing.T, []byte) ([]sentCall, []sentResult)
	}{
		{name: "openai_chat_compat", build: func() ([]byte, error) { return buildOpenAIRequest(req, false) }, decode: openAIChatSent},
		{name: "openai_responses", build: func() ([]byte, error) { return buildOpenAIResponsesRequest(req, false) }, decode: openAIResponsesSent},
		{name: "anthropic_messages", build: func() ([]byte, error) { return buildAnthropicRequest(req, false) }, decode: anthropicSent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, err := tc.build()
			if err != nil {
				t.Fatalf("build the request: %v", err)
			}

			calls, results := tc.decode(t, body)

			if got, want := idsOfCalls(calls), []string{callIDA, callIDB}; !slices.Equal(got, want) {
				t.Fatalf("call ids = %+v, want %v", calls, want)
			}
			for i, letter := range []string{"a", "b"} {
				if want := `"q":"` + letter + `"`; !strings.Contains(calls[i].Arguments, want) {
					t.Errorf("call %d arguments = %s, want %s", i, calls[i].Arguments, want)
				}
			}
			if got, want := idsOfResults(results), []string{callIDB, callIDA}; !slices.Equal(got, want) {
				t.Fatalf("result ids = %+v, want %v", results, want)
			}
			for i, letter := range []string{"b", "a"} {
				if want := `"hit":"` + letter + `"`; !strings.Contains(results[i].Output, want) {
					t.Errorf("result %d output = %s, want %s", i, results[i].Output, want)
				}
			}
		})
	}
	t.Run("gemini", func(t *testing.T) {
		body, err := buildGeminiRequest(req)
		if err != nil {
			t.Fatalf("build the request: %v", err)
		}

		sent := string(body)

		for _, id := range []string{callIDA, callIDB} {
			if strings.Contains(sent, id) {
				t.Errorf("gemini request carries the id %s, want none", id)
			}
		}
		if n := strings.Count(sent, `"name":"search"`); n != 4 {
			t.Errorf("gemini request names the search tool %d times, want 4 (two calls, two responses)", n)
		}
	})
}
