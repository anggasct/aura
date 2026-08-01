package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http"
	"strings"
	"time"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

const openaiDefaultBaseURL = "https://api.openai.com"

type OpenAIAdapter struct {
	name                 string
	baseURL              string
	apiKey               string
	httpClient           *http.Client
	retry                RetryConfig
	streamingIdleTimeout time.Duration
}

func NewOpenAIAdapter(name, baseURL, apiKey string, timeout time.Duration) (*OpenAIAdapter, error) {
	if baseURL != "" {
		if err := validateBaseURL(baseURL); err != nil {
			return nil, fmt.Errorf("model: invalid base_url %q: %w", baseURL, err)
		}
	}
	return newOpenAIAdapter(name, baseURL, apiKey, timeout, defaultStreamingIdleTimeout), nil
}

func newOpenAIAdapter(name, baseURL, apiKey string, timeout, idleTimeout time.Duration) *OpenAIAdapter {
	if baseURL == "" {
		baseURL = openaiDefaultBaseURL
	}
	if timeout <= 0 {
		timeout = defaultRequestTimeout
	}
	if idleTimeout <= 0 {
		idleTimeout = defaultStreamingIdleTimeout
	}
	return &OpenAIAdapter{
		name:                 name,
		baseURL:              strings.TrimRight(baseURL, "/"),
		apiKey:               apiKey,
		httpClient:           &http.Client{Timeout: timeout, CheckRedirect: rejectCrossOriginRedirect},
		retry:                defaultRetryConfig(),
		streamingIdleTimeout: idleTimeout,
	}
}

func (a *OpenAIAdapter) Name() string { return a.name }

func (a *OpenAIAdapter) GenerateContent(ctx context.Context, req *adkmodel.LLMRequest, stream bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		telemetry := newModelTelemetry("openai_chat_compat", a.name)
		resp, retries, err := a.do(ctx, req, stream)
		telemetry.retries = retries
		if err != nil {
			telemetry.finish(ctx, nil, err)
			yield(nil, err)
			return
		}
		defer resp.Body.Close()

		if cErr := classifyHTTPResponse(resp, "openai"); cErr != nil {
			telemetry.finish(ctx, nil, cErr)
			yield(nil, cErr)
			return
		}

		if stream {
			a.stream(ctx, resp.Body, func(response *adkmodel.LLMResponse, streamErr error) bool {
				if streamErr != nil || (response != nil && response.TurnComplete) {
					telemetry.finish(ctx, response, streamErr)
				}
				return yield(response, streamErr)
			})
			telemetry.finishIfNeeded(ctx)
			return
		}
		payload, err := io.ReadAll(resp.Body)
		if err != nil {
			telemetry.finish(ctx, nil, err)
			yield(nil, fmt.Errorf("model: failed to read openai response: %w", err))
			return
		}
		out, err := parseOpenAIResponse(payload)
		if err != nil {
			telemetry.finish(ctx, nil, err)
			yield(nil, err)
			return
		}
		telemetry.finish(ctx, out, nil)
		yield(out, nil)
	}
}

func (a *OpenAIAdapter) do(ctx context.Context, req *adkmodel.LLMRequest, stream bool) (*http.Response, int, error) {
	retryCount := 0
	body, err := buildOpenAIRequest(req, stream)
	if err != nil {
		return nil, retryCount, fmt.Errorf("model: failed to build openai request: %w", err)
	}
	url := a.baseURL + "/v1/chat/completions"
	retry := retryConfigForRequest(a.retry, req)
	retry.RetryCount = &retryCount
	resp, err := retryHTTP(ctx, retry, func() (*http.Response, error) {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("model: failed to create openai request: %w", err)
		}
		httpReq.Header.Set("Content-Type", "application/json")
		if a.apiKey != "" {
			httpReq.Header.Set("Authorization", "Bearer "+a.apiKey)
		}
		resp, err := a.httpClient.Do(httpReq)
		if err != nil {
			return nil, classifyRequestError(err)
		}
		return resp, nil
	})
	return resp, retryCount, err
}

func (a *OpenAIAdapter) stream(ctx context.Context, body io.ReadCloser, yield func(*adkmodel.LLMResponse, error) bool) {
	var lastUsage *genai.GenerateContentResponseUsageMetadata
	tools := &streamToolAccumulator{}

	err := scanSSE(ctx, body, a.streamingIdleTimeout, func(data []byte) error {
		if strings.TrimSpace(string(data)) == "[DONE]" {
			return io.EOF
		}
		var chunk openaiStreamChunk
		if err := json.Unmarshal(data, &chunk); err != nil {
			return fmt.Errorf("model: failed to parse openai stream chunk: %w", err)
		}
		if chunk.Usage != nil {
			lastUsage = openaiUsageToMetadata(chunk.Usage)
		}
		if len(chunk.Choices) == 0 {
			return nil
		}
		ch := chunk.Choices[0]
		for _, tc := range ch.Delta.ToolCalls {
			tools.add(tc)
		}
		c := &genai.Content{Role: "model"}
		if ch.Delta.Content != "" {
			c.Parts = append(c.Parts, &genai.Part{Text: ch.Delta.Content})
		}
		done := ch.FinishReason != "" && ch.FinishReason != "null"
		if done {
			parts, err := tools.parts()
			if err != nil {
				return err
			}
			c.Parts = append(c.Parts, parts...)
		}
		if len(c.Parts) == 0 && !done {
			return nil
		}
		resp := &adkmodel.LLMResponse{Content: c, Partial: !done, TurnComplete: done}
		if done {
			resp.UsageMetadata = lastUsage
		}
		if !yield(resp, nil) {
			return errStopYield
		}
		return nil
	})
	if err != nil && !errors.Is(err, io.EOF) {
		yield(nil, err)
	}
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

type streamToolAccumulator struct {
	calls map[int]*openaiStreamToolCall
	args  map[int]*strings.Builder
	order []int
}

func (s *streamToolAccumulator) add(tc openaiStreamToolCall) {
	if s.calls == nil {
		s.calls = map[int]*openaiStreamToolCall{}
		s.args = map[int]*strings.Builder{}
	}
	if _, ok := s.calls[tc.Index]; !ok {
		s.calls[tc.Index] = &openaiStreamToolCall{Index: tc.Index, Type: "function"}
		s.args[tc.Index] = &strings.Builder{}
		s.order = append(s.order, tc.Index)
	}
	existing := s.calls[tc.Index]
	if tc.ID != "" {
		existing.ID = tc.ID
	}
	if tc.Function.Name != "" {
		existing.Function.Name = tc.Function.Name
	}
	if tc.Function.Arguments != "" {
		s.args[tc.Index].WriteString(tc.Function.Arguments)
	}
}

func (s *streamToolAccumulator) parts() ([]*genai.Part, error) {
	parts := make([]*genai.Part, 0, len(s.order))
	for _, idx := range s.order {
		args := json.RawMessage(s.args[idx].String())
		if len(bytes.TrimSpace(args)) == 0 {
			args = json.RawMessage("{}")
		}
		if !json.Valid(args) {
			return nil, fmt.Errorf("model: invalid tool call %q: %w", s.calls[idx].Function.Name, ErrInvalidToolCall)
		}
		tc := ToolCall{ID: s.calls[idx].ID, Name: s.calls[idx].Function.Name, Arguments: args}
		fc, err := canonicalToFunctionCall(tc)
		if err != nil {
			return nil, err
		}
		parts = append(parts, &genai.Part{FunctionCall: fc})
	}
	return parts, nil
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
