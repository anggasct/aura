package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"net/http"
	"strings"
	"time"

	"github.com/anggasct/aura/internal/config"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

const openaiDefaultBaseURL = "https://api.openai.com"

type openaiCodec struct{}

func (openaiCodec) protocol() string { return "openai_chat_compat" }

func (openaiCodec) endpoint(baseURL string, req *adkmodel.LLMRequest, stream bool) string {
	return baseURL + "/v1/chat/completions"
}

func (openaiCodec) buildRequest(req *adkmodel.LLMRequest, stream bool) ([]byte, error) {
	return buildOpenAIRequest(req, stream)
}

func (openaiCodec) decodeResponse(body []byte) (*adkmodel.LLMResponse, error) {
	return parseOpenAIResponse(body)
}

func (openaiCodec) setAuthHeaders(req *http.Request, apiKey string) {
	req.Header.Set("Authorization", "Bearer "+apiKey)
}

func (openaiCodec) decodeStreamEvent(data []byte) ([]streamOp, bool, error) {
	if strings.TrimSpace(string(data)) == "[DONE]" {
		return nil, true, nil
	}
	var chunk openaiStreamChunk
	if err := json.Unmarshal(data, &chunk); err != nil {
		return nil, false, fmt.Errorf("model: failed to parse openai stream chunk: %w", err)
	}
	var ops []streamOp
	if chunk.Usage != nil {
		ops = append(ops, streamOp{kind: opUsage, usageIn: chunk.Usage.PromptTokens, usageOut: chunk.Usage.CompletionTokens})
	}
	if len(chunk.Choices) == 0 {
		return ops, false, nil
	}
	choice := chunk.Choices[0]
	for _, tc := range choice.Delta.ToolCalls {
		if tc.Index < 0 {
			continue
		}
		if tc.ID != "" || tc.Function.Name != "" {
			ops = append(ops, streamOp{kind: opToolStart, idx: tc.Index, toolID: tc.ID, toolName: tc.Function.Name})
		}
		if tc.Function.Arguments != "" {
			ops = append(ops, streamOp{kind: opToolArgs, idx: tc.Index, toolArgs: tc.Function.Arguments})
		}
	}
	if choice.Delta.Content != "" {
		ops = append(ops, streamOp{kind: opText, text: choice.Delta.Content})
	}
	done := choice.FinishReason != "" && choice.FinishReason != "null"
	if done {
		ops = append(ops, streamOp{kind: opStop, stop: choice.FinishReason}, streamOp{kind: opDone})
	}
	return ops, done, nil
}

type OpenAIAdapter struct {
	core *coreClient
}

func NewOpenAIAdapter(name, baseURL, apiKey string, timeout time.Duration) (*OpenAIAdapter, error) {
	if baseURL != "" {
		if err := config.ValidateBaseURL(baseURL); err != nil {
			return nil, fmt.Errorf("model: invalid base_url %q: %w", baseURL, err)
		}
	}
	return newOpenAIAdapter(name, baseURL, apiKey, timeout, defaultStreamingIdleTimeout), nil
}

func newOpenAIAdapter(name, baseURL, apiKey string, timeout, idleTimeout time.Duration) *OpenAIAdapter {
	if baseURL == "" {
		baseURL = openaiDefaultBaseURL
	}
	return &OpenAIAdapter{core: newCoreClient(name, baseURL, apiKey, timeout, idleTimeout, openaiCodec{})}
}

func (a *OpenAIAdapter) Name() string { return a.core.name }

func (a *OpenAIAdapter) GenerateContent(ctx context.Context, req *adkmodel.LLMRequest, stream bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return a.core.GenerateContent(ctx, req, stream)
}

type openaiRequestBody struct {
	Model       string          `json:"model"`
	Messages    []openaiMessage `json:"messages"`
	Stream      bool            `json:"stream,omitempty"`
	Tools       []openaiTool    `json:"tools,omitempty"`
	MaxTokens   int32           `json:"max_tokens,omitempty"`
	Temperature *float32        `json:"temperature,omitempty"`
	TopP        *float32        `json:"top_p,omitempty"`
	Stop        []string        `json:"stop,omitempty"`
}

type openaiMessage struct {
	Role       string           `json:"role"`
	Content    string           `json:"content,omitempty"`
	ToolCalls  []openaiToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
}

type openaiToolCall struct {
	ID       string         `json:"id"`
	Type     string         `json:"type"`
	Function openaiToolFunc `json:"function"`
}

type openaiToolFunc struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type openaiTool struct {
	Type     string         `json:"type"`
	Function openaiToolDecl `json:"function"`
}

type openaiToolDecl struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type openaiResponse struct {
	Model   string         `json:"model"`
	Choices []openaiChoice `json:"choices"`
	Usage   *openaiUsage   `json:"usage,omitempty"`
}

type openaiChoice struct {
	Message      openaiMessage `json:"message"`
	FinishReason string        `json:"finish_reason"`
}

type openaiUsage struct {
	PromptTokens     int32 `json:"prompt_tokens"`
	CompletionTokens int32 `json:"completion_tokens"`
	TotalTokens      int32 `json:"total_tokens"`
}

type openaiStreamChunk struct {
	Choices []openaiStreamChoice `json:"choices"`
	Usage   *openaiUsage         `json:"usage,omitempty"`
}

type openaiStreamChoice struct {
	Delta        openaiStreamDelta `json:"delta"`
	FinishReason string            `json:"finish_reason"`
}

type openaiStreamDelta struct {
	Role      string                 `json:"role,omitempty"`
	Content   string                 `json:"content,omitempty"`
	ToolCalls []openaiStreamToolCall `json:"tool_calls,omitempty"`
}

type openaiStreamToolCall struct {
	Index    int            `json:"index"`
	ID       string         `json:"id,omitempty"`
	Type     string         `json:"type,omitempty"`
	Function openaiToolFunc `json:"function"`
}

func buildOpenAIRequest(req *adkmodel.LLMRequest, stream bool) ([]byte, error) {
	body := openaiRequestBody{Model: req.Model, Stream: stream}
	if req.Config != nil {
		body.MaxTokens = req.Config.MaxOutputTokens
		body.Temperature = req.Config.Temperature
		body.TopP = req.Config.TopP
		body.Stop = req.Config.StopSequences
		if req.Config.SystemInstruction != nil {
			body.Messages = append(body.Messages, openaiMessage{Role: "system", Content: contentText(req.Config.SystemInstruction)})
		}
		for _, t := range req.Config.Tools {
			if t == nil {
				continue
			}
			for _, fd := range t.FunctionDeclarations {
				if fd == nil {
					continue
				}
				body.Tools = append(body.Tools, openaiTool{
					Type: "function",
					Function: openaiToolDecl{
						Name:        fd.Name,
						Description: fd.Description,
						Parameters:  toolParams(fd),
					},
				})
			}
		}
	}
	for _, c := range req.Contents {
		body.Messages = append(body.Messages, contentToOpenAIMessages(c)...)
	}
	return json.Marshal(body)
}

func contentToOpenAIMessages(c *genai.Content) []openaiMessage {
	if c == nil {
		return nil
	}
	role := "user"
	if c.Role == "model" {
		role = "assistant"
	}
	var texts []string
	var toolCalls []openaiToolCall
	var toolResults []openaiMessage
	for _, p := range c.Parts {
		switch {
		case p.Text != "":
			texts = append(texts, p.Text)
		case p.FunctionCall != nil:
			toolCalls = append(toolCalls, canonicalToOpenAIToolCall(toCanonicalToolCall(p.FunctionCall)))
		case p.FunctionResponse != nil:
			toolResults = append(toolResults, functionResponseToToolMessage(p.FunctionResponse))
		}
	}
	msgs := append([]openaiMessage{}, toolResults...)
	content := strings.Join(texts, "")
	if role == "assistant" {
		if content != "" || len(toolCalls) > 0 {
			m := openaiMessage{Role: "assistant"}
			if content != "" {
				m.Content = content
			}
			m.ToolCalls = toolCalls
			msgs = append(msgs, m)
		}
	} else if content != "" {
		msgs = append(msgs, openaiMessage{Role: role, Content: content})
	}
	return msgs
}

func functionResponseToToolMessage(fr *genai.FunctionResponse) openaiMessage {
	resp := []byte("{}")
	if len(fr.Response) > 0 {
		if b, err := json.Marshal(fr.Response); err == nil {
			resp = b
		}
	}
	return openaiMessage{Role: "tool", ToolCallID: fr.ID, Content: string(resp)}
}

func parseOpenAIResponse(body []byte) (*adkmodel.LLMResponse, error) {
	var resp openaiResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("model: failed to parse openai response: %w", err)
	}
	if len(resp.Choices) == 0 {
		return nil, errors.New("model: openai response had no choices")
	}
	msg := resp.Choices[0].Message
	c := &genai.Content{Role: "model"}
	if msg.Content != "" {
		c.Parts = append(c.Parts, &genai.Part{Text: msg.Content})
	}
	for _, tc := range msg.ToolCalls {
		ct, err := fromOpenAIToolCall(tc)
		if err != nil {
			return nil, err
		}
		fc, err := canonicalToFunctionCall(ct)
		if err != nil {
			return nil, err
		}
		c.Parts = append(c.Parts, &genai.Part{FunctionCall: fc})
	}
	return &adkmodel.LLMResponse{
		Content:       c,
		UsageMetadata: openaiUsageToMetadata(resp.Usage),
		ModelVersion:  resp.Model,
		TurnComplete:  true,
	}, nil
}

func openaiUsageToMetadata(u *openaiUsage) *genai.GenerateContentResponseUsageMetadata {
	if u == nil {
		return nil
	}
	return &genai.GenerateContentResponseUsageMetadata{
		PromptTokenCount:     u.PromptTokens,
		CandidatesTokenCount: u.CompletionTokens,
		TotalTokenCount:      u.TotalTokens,
	}
}

func toCanonicalToolCall(fc *genai.FunctionCall) ToolCall {
	args := json.RawMessage("{}")
	if len(fc.Args) > 0 {
		if b, err := json.Marshal(fc.Args); err == nil {
			args = b
		}
	}
	return ToolCall{ID: fc.ID, Name: fc.Name, Arguments: args}
}

func canonicalToFunctionCall(tc ToolCall) (*genai.FunctionCall, error) {
	args := map[string]any{}
	if len(bytes.TrimSpace(tc.Arguments)) > 0 {
		if err := json.Unmarshal(tc.Arguments, &args); err != nil || args == nil {
			return nil, fmt.Errorf("model: invalid tool call %q: %w", tc.Name, ErrInvalidToolCall)
		}
	}
	return &genai.FunctionCall{ID: tc.ID, Name: tc.Name, Args: args}, nil
}

func canonicalToOpenAIToolCall(tc ToolCall) openaiToolCall {
	return openaiToolCall{ID: tc.ID, Type: "function", Function: openaiToolFunc{Name: tc.Name, Arguments: string(tc.Arguments)}}
}

func fromOpenAIToolCall(tc openaiToolCall) (ToolCall, error) {
	raw := json.RawMessage(tc.Function.Arguments)
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = json.RawMessage("{}")
	}
	if !json.Valid(raw) {
		return ToolCall{}, fmt.Errorf("openai tool_call %q: %w", tc.Function.Name, ErrInvalidToolCall)
	}
	return ToolCall{ID: tc.ID, Name: tc.Function.Name, Arguments: raw}, nil
}

func toolParams(fd *genai.FunctionDeclaration) json.RawMessage {
	if fd.ParametersJsonSchema != nil {
		if b, err := json.Marshal(fd.ParametersJsonSchema); err == nil {
			return b
		}
	}
	if fd.Parameters != nil {
		if b, err := json.Marshal(fd.Parameters); err == nil {
			return b
		}
	}
	return nil
}

func contentText(c *genai.Content) string {
	if c == nil {
		return ""
	}
	var parts []string
	for _, p := range c.Parts {
		if p.Text != "" {
			parts = append(parts, p.Text)
		}
	}
	return strings.Join(parts, "")
}

func classifyHTTPStatus(status int, provider string) error {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return codedError(ErrorCodeAuthFailed, ErrAuthFailed, fmt.Sprintf("%s: http %d", provider, status))
	case status == http.StatusNotFound:
		return codedError(ErrorCodeNotFound, ErrModelNotFound, fmt.Sprintf("%s: http %d", provider, status))
	case status == http.StatusTooManyRequests:
		return codedError(ErrorCodeRateLimited, ErrRateLimited, fmt.Sprintf("%s: http %d", provider, status))
	case status >= 500:
		return codedError(ErrorCodeOverloaded, ErrOverloaded, fmt.Sprintf("%s: http %d", provider, status))
	}
	return nil
}

func classifyRequestError(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return codedError(ErrorCodeConnectionFailed, ErrConnectionFailed, fmt.Sprintf("request failed: %v", err))
}
