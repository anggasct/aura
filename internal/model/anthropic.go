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

const anthropicDefaultBaseURL = "https://api.anthropic.com"
const anthropicVersion = "2023-06-01"

type AnthropicAdapter struct {
	name                 string
	baseURL              string
	apiKey               string
	httpClient           *http.Client
	retry                RetryConfig
	streamingIdleTimeout time.Duration
}

func NewAnthropicAdapter(name, baseURL, apiKey string, timeout time.Duration) (*AnthropicAdapter, error) {
	if baseURL != "" {
		if err := validateBaseURL(baseURL); err != nil {
			return nil, fmt.Errorf("model: invalid base_url %q: %w", baseURL, err)
		}
	}
	return newAnthropicAdapter(name, baseURL, apiKey, timeout, defaultStreamingIdleTimeout), nil
}

func newAnthropicAdapter(name, baseURL, apiKey string, timeout, idleTimeout time.Duration) *AnthropicAdapter {
	if baseURL == "" {
		baseURL = anthropicDefaultBaseURL
	}
	if timeout <= 0 {
		timeout = defaultRequestTimeout
	}
	if idleTimeout <= 0 {
		idleTimeout = defaultStreamingIdleTimeout
	}
	return &AnthropicAdapter{
		name:                 name,
		baseURL:              strings.TrimRight(baseURL, "/"),
		apiKey:               apiKey,
		httpClient:           &http.Client{Timeout: timeout},
		retry:                defaultRetryConfig(),
		streamingIdleTimeout: idleTimeout,
	}
}

func (a *AnthropicAdapter) Name() string { return a.name }

func (a *AnthropicAdapter) GenerateContent(ctx context.Context, req *adkmodel.LLMRequest, stream bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		resp, err := a.do(ctx, req, stream)
		if err != nil {
			yield(nil, err)
			return
		}
		defer resp.Body.Close()

		if cErr := classifyHTTPResponse(resp, "anthropic"); cErr != nil {
			yield(nil, cErr)
			return
		}

		if stream {
			a.stream(ctx, resp.Body, yield)
			return
		}
		payload, err := io.ReadAll(resp.Body)
		if err != nil {
			yield(nil, fmt.Errorf("model: failed to read anthropic response: %w", err))
			return
		}
		out, err := parseAnthropicResponse(payload)
		if err != nil {
			yield(nil, err)
			return
		}
		yield(out, nil)
	}
}

func (a *AnthropicAdapter) do(ctx context.Context, req *adkmodel.LLMRequest, stream bool) (*http.Response, error) {
	body, err := buildAnthropicRequest(req, stream)
	if err != nil {
		return nil, fmt.Errorf("model: failed to build anthropic request: %w", err)
	}
	url := a.baseURL + "/v1/messages"
	return retryHTTP(ctx, a.retry, func() (*http.Response, error) {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("model: failed to create anthropic request: %w", err)
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("x-api-key", a.apiKey)
		httpReq.Header.Set("anthropic-version", anthropicVersion)
		resp, err := a.httpClient.Do(httpReq)
		if err != nil {
			return nil, classifyRequestError(err)
		}
		return resp, nil
	})
}

func (a *AnthropicAdapter) stream(ctx context.Context, body io.ReadCloser, yield func(*adkmodel.LLMResponse, error) bool) {
	acc := &streamContentAccumulator{}

	err := scanSSE(ctx, body, a.streamingIdleTimeout, func(data []byte) error {
		var ev anthropicStreamEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			return fmt.Errorf("model: failed to parse anthropic stream event: %w", err)
		}
		switch ev.Type {
		case "message_start":
			if ev.Message != nil {
				acc.model = ev.Message.Model
				acc.inputTokens = ev.Message.Usage.InputTokens
			}
		case "content_block_start":
			if ev.ContentBlock != nil {
				if ev.ContentBlock.Type == "text" {
					acc.newBlock(ev.Index, blockText)
				} else {
					acc.newBlock(ev.Index, blockToolUse)
					if ev.ContentBlock.ID != "" {
						acc.blocks[ev.Index].id = ev.ContentBlock.ID
					}
					if ev.ContentBlock.Name != "" {
						acc.blocks[ev.Index].name = ev.ContentBlock.Name
					}
				}
			}
		case "content_block_delta":
			if ev.Delta != nil {
				text := ev.Delta.Text
				if ev.Delta.PartialJSON != "" {
					text = ev.Delta.PartialJSON
				}
				b := acc.blocks[ev.Index]
				if b != nil && text != "" {
					b.text.WriteString(text)
					if b.kind == blockText {
						partial := &adkmodel.LLMResponse{
							Content: &genai.Content{Role: "model", Parts: []*genai.Part{{Text: ev.Delta.Text}}},
							Partial: true,
						}
						if !yield(partial, nil) {
							return errStopYield
						}
					}
				}
			}
		case "message_delta":
			if ev.Delta != nil {
				acc.stopReason = ev.Delta.StopReason
			}
			if ev.Usage != nil {
				acc.outputTokens = ev.Usage.OutputTokens
			}
		case "message_stop":
			resp, err := acc.finalResponse()
			if err != nil {
				return err
			}
			if !yield(resp, nil) {
				return errStopYield
			}
		}
		return nil
	})
	if err != nil && !errors.Is(err, errStopYield) {
		yield(nil, err)
	}
}

type blockKind int

const (
	blockText blockKind = iota
	blockToolUse
)

type streamBlock struct {
	kind blockKind
	id   string
	name string
	text strings.Builder
}

type streamContentAccumulator struct {
	model        string
	inputTokens  int32
	outputTokens int32
	stopReason   string
	blocks       map[int]*streamBlock
	order        []int
}

func (a *streamContentAccumulator) newBlock(idx int, kind blockKind) {
	if a.blocks == nil {
		a.blocks = map[int]*streamBlock{}
	}
	a.blocks[idx] = &streamBlock{kind: kind}
	a.order = append(a.order, idx)
}

func (a *streamContentAccumulator) finalResponse() (*adkmodel.LLMResponse, error) {
	c := &genai.Content{Role: "model"}
	for _, idx := range a.order {
		b := a.blocks[idx]
		switch b.kind {
		case blockText:
			c.Parts = append(c.Parts, &genai.Part{Text: b.text.String()})
		case blockToolUse:
			input := json.RawMessage(b.text.String())
			if len(bytes.TrimSpace(input)) == 0 {
				input = json.RawMessage("{}")
			}
			if !json.Valid(input) {
				return nil, fmt.Errorf("model: invalid tool call %q: %w", b.name, ErrInvalidToolCall)
			}
			tc := ToolCall{ID: b.id, Name: b.name, Arguments: input}
			fc, err := canonicalToFunctionCall(tc)
			if err != nil {
				return nil, err
			}
			c.Parts = append(c.Parts, &genai.Part{FunctionCall: fc})
		}
	}
	return &adkmodel.LLMResponse{
		Content:      c,
		ModelVersion: a.model,
		TurnComplete: true,
		UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
			PromptTokenCount:     a.inputTokens,
			CandidatesTokenCount: a.outputTokens,
			TotalTokenCount:      a.inputTokens + a.outputTokens,
		},
	}, nil
}

type anthropicRequest struct {
	Model         string             `json:"model"`
	MaxTokens     int32              `json:"max_tokens"`
	System        string             `json:"system,omitempty"`
	Messages      []anthropicMessage `json:"messages"`
	Tools         []anthropicTool    `json:"tools,omitempty"`
	Stream        bool               `json:"stream,omitempty"`
	Temperature   *float32           `json:"temperature,omitempty"`
	TopP          *float32           `json:"top_p,omitempty"`
	StopSequences []string           `json:"stop_sequences,omitempty"`
}

type anthropicMessage struct {
	Role    string                  `json:"role"`
	Content []anthropicContentBlock `json:"content"`
}

type anthropicContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   string          `json:"content,omitempty"`
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type anthropicResponse struct {
	Model      string                  `json:"model"`
	Content    []anthropicContentBlock `json:"content"`
	StopReason string                  `json:"stop_reason"`
	Usage      anthropicUsage          `json:"usage"`
}

type anthropicUsage struct {
	InputTokens  int32 `json:"input_tokens"`
	OutputTokens int32 `json:"output_tokens"`
}

type anthropicStreamEvent struct {
	Type         string                 `json:"type"`
	ContentBlock *anthropicContentBlock `json:"content_block,omitempty"`
	Delta        *anthropicDelta        `json:"delta,omitempty"`
	Index        int                    `json:"index"`
	Message      *anthropicStreamMsg    `json:"message,omitempty"`
	Usage        *anthropicUsage        `json:"usage,omitempty"`
}

type anthropicDelta struct {
	Type         string `json:"type,omitempty"`
	Text         string `json:"text,omitempty"`
	PartialJSON  string `json:"partial_json,omitempty"`
	StopReason   string `json:"stop_reason,omitempty"`
	StopSequence string `json:"stop_sequence,omitempty"`
}

type anthropicStreamMsg struct {
	ID    string         `json:"id"`
	Model string         `json:"model"`
	Usage anthropicUsage `json:"usage"`
}

func buildAnthropicRequest(req *adkmodel.LLMRequest, stream bool) ([]byte, error) {
	body := anthropicRequest{Model: req.Model, Stream: stream, MaxTokens: 1024}
	if req.Config != nil {
		if req.Config.MaxOutputTokens > 0 {
			body.MaxTokens = req.Config.MaxOutputTokens
		}
		body.Temperature = req.Config.Temperature
		body.TopP = req.Config.TopP
		body.StopSequences = req.Config.StopSequences
		if req.Config.SystemInstruction != nil {
			body.System = contentText(req.Config.SystemInstruction)
		}
		for _, t := range req.Config.Tools {
			if t == nil {
				continue
			}
			for _, fd := range t.FunctionDeclarations {
				if fd == nil {
					continue
				}
				params := toolParams(fd)
				if params == nil {
					params = json.RawMessage(`{"type":"object"}`)
				}
				body.Tools = append(body.Tools, anthropicTool{
					Name:        fd.Name,
					Description: fd.Description,
					InputSchema: params,
				})
			}
		}
	}
	for _, c := range req.Contents {
		msgs := contentToAnthropicMessages(c)
		body.Messages = append(body.Messages, msgs...)
	}
	return json.Marshal(body)
}

func contentToAnthropicMessages(c *genai.Content) []anthropicMessage {
	if c == nil {
		return nil
	}
	role := "user"
	if c.Role == "model" {
		role = "assistant"
	}
	var blocks []anthropicContentBlock
	for _, p := range c.Parts {
		switch {
		case p.Text != "":
			blocks = append(blocks, anthropicContentBlock{Type: "text", Text: p.Text})
		case p.FunctionCall != nil:
			tc := toCanonicalToolCall(p.FunctionCall)
			input := tc.Arguments
			if len(input) == 0 {
				input = json.RawMessage("{}")
			}
			blocks = append(blocks, anthropicContentBlock{
				Type:  "tool_use",
				ID:    tc.ID,
				Name:  tc.Name,
				Input: input,
			})
		case p.FunctionResponse != nil:
			fr := p.FunctionResponse
			resp := ""
			if len(fr.Response) > 0 {
				if b, err := json.Marshal(fr.Response); err == nil {
					resp = string(b)
				}
			}
			blocks = append(blocks, anthropicContentBlock{
				Type:      "tool_result",
				ToolUseID: fr.ID,
				Content:   resp,
			})
		}
	}
	if len(blocks) == 0 {
		return nil
	}
	return []anthropicMessage{{Role: role, Content: blocks}}
}

func parseAnthropicResponse(body []byte) (*adkmodel.LLMResponse, error) {
	var resp anthropicResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("model: failed to parse anthropic response: %w", err)
	}
	c, err := anthropicContentToGenai(resp.Content)
	if err != nil {
		return nil, err
	}
	return &adkmodel.LLMResponse{
		Content:       c,
		ModelVersion:  resp.Model,
		TurnComplete:  true,
		UsageMetadata: anthropicUsageToMetadata(&resp.Usage),
	}, nil
}

func anthropicContentToGenai(blocks []anthropicContentBlock) (*genai.Content, error) {
	c := &genai.Content{Role: "model"}
	for _, b := range blocks {
		switch b.Type {
		case "text":
			c.Parts = append(c.Parts, &genai.Part{Text: b.Text})
		case "tool_use":
			input := b.Input
			if len(bytes.TrimSpace(input)) == 0 {
				input = json.RawMessage("{}")
			}
			tc := ToolCall{ID: b.ID, Name: b.Name, Arguments: input}
			fc, err := canonicalToFunctionCall(tc)
			if err != nil {
				return nil, err
			}
			c.Parts = append(c.Parts, &genai.Part{FunctionCall: fc})
		}
	}
	return c, nil
}

func anthropicUsageToMetadata(u *anthropicUsage) *genai.GenerateContentResponseUsageMetadata {
	if u == nil {
		return nil
	}
	return &genai.GenerateContentResponseUsageMetadata{
		PromptTokenCount:     u.InputTokens,
		CandidatesTokenCount: u.OutputTokens,
		TotalTokenCount:      u.InputTokens + u.OutputTokens,
	}
}
