package app

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

type agentChatServer struct {
	url   string
	steps []agentScriptStep
	log   *agentModelLog
}

type agentChatMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type agentChatRequest struct {
	Stream   bool               `json:"stream"`
	Messages []agentChatMessage `json:"messages"`
	Tools    []struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	} `json:"tools"`
}

func newAgentChatServer(t *testing.T, steps []agentScriptStep) *agentChatServer {
	t.Helper()
	chat := &agentChatServer{steps: steps, log: &agentModelLog{}}
	srv := httptest.NewServer(http.HandlerFunc(chat.serve))
	t.Cleanup(func() {
		srv.CloseClientConnections()
		srv.Close()
	})
	chat.url = srv.URL
	return chat
}

func (c *agentChatServer) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
		http.NotFound(w, r)
		return
	}
	var request agentChatRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, stubRequestLimit)).Decode(&request); err != nil {
		http.Error(w, "malformed chat request", http.StatusBadRequest)
		return
	}
	described := &agentModelRequest{Tools: []string{}, Results: []string{}}
	var text strings.Builder
	for _, message := range request.Messages {
		content, _ := message.Content.(string)
		text.WriteString(content + "\n")
		if message.Role == "tool" {
			described.Results = append(described.Results, content)
		}
	}
	described.Text = text.String()
	for _, declared := range request.Tools {
		described.Tools = append(described.Tools, declared.Function.Name)
	}
	slices.Sort(described.Tools)
	position, err := c.log.arrive(described)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	step := agentScriptStep{text: "done"}
	if position < len(c.steps) {
		step = c.steps[position]
	}
	c.answer(w, request.Stream, position, step)
}

func (c *agentChatServer) answer(w http.ResponseWriter, stream bool, position int, step agentScriptStep) {
	usage := `{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}`
	message := map[string]any{"role": "assistant", "content": step.text}
	finish := "stop"
	delta := map[string]any{"role": "assistant", "content": step.text}
	if step.call != nil {
		arguments, err := json.Marshal(step.call.Args)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		calls := []map[string]any{{
			"id": fmt.Sprintf("call-%d", position+1), "type": "function",
			"function": map[string]any{"name": step.call.Name, "arguments": string(arguments)},
		}}
		message = map[string]any{"role": "assistant", "content": nil, "tool_calls": calls}
		streamed := []map[string]any{{
			"index": 0, "id": calls[0]["id"], "type": "function", "function": calls[0]["function"],
		}}
		delta = map[string]any{"role": "assistant", "tool_calls": streamed}
		finish = "tool_calls"
	}
	if !stream {
		body, err := json.Marshal(map[string]any{
			"id": "chat-1", "object": "chat.completion", "model": "test-model",
			"choices": []map[string]any{{"index": 0, "message": message, "finish_reason": finish}},
			"usage":   json.RawMessage(usage),
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
		return
	}
	first, err := json.Marshal(map[string]any{"id": "chat-1", "model": "test-model", "choices": []map[string]any{{"index": 0, "delta": delta, "finish_reason": nil}}})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	last, err := json.Marshal(map[string]any{"id": "chat-1", "model": "test-model", "choices": []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": finish}}, "usage": json.RawMessage(usage)})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = fmt.Fprintf(w, "data: %s\n\ndata: %s\n\ndata: [DONE]\n\n", first, last)
}
