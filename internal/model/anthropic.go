package model

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"net/http"
	"time"

	"github.com/anggasct/aura/internal/config"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

const anthropicDefaultBaseURL = "https://api.anthropic.com"
const anthropicVersion = "2023-06-01"

type anthropicCodec struct{}

func (anthropicCodec) protocol() string { return "anthropic_messages" }

func (anthropicCodec) endpoint(baseURL string, req *adkmodel.LLMRequest, stream bool) string {
	return baseURL + "/v1/messages"
}

func (anthropicCodec) buildRequest(req *adkmodel.LLMRequest, stream bool) ([]byte, error) {
	return buildAnthropicRequest(req, stream)
}

func (anthropicCodec) decodeResponse(body []byte) (*adkmodel.LLMResponse, error) {
	return parseAnthropicResponse(body)
}

func (anthropicCodec) setAuthHeaders(req *http.Request, apiKey string) {
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", anthropicVersion)
}

func (anthropicCodec) decodeStreamEvent(data []byte) ([]streamOp, bool, error) {
	var event anthropicStreamEvent
	if err := json.Unmarshal(data, &event); err != nil {
		return nil, false, fmt.Errorf("model: failed to parse anthropic stream event: %w", err)
	}
	var ops []streamOp
	switch event.Type {
	case "message_start":
		if event.Message != nil {
			ops = append(ops,
				streamOp{kind: opModel, model: event.Message.Model},
				streamOp{kind: opUsage, usageIn: event.Message.Usage.InputTokens},
			)
		}
	case "content_block_start":
		if event.ContentBlock == nil {
			return nil, false, nil
		}
		if event.ContentBlock.Type == "tool_use" {
			ops = append(ops, streamOp{kind: opToolStart, idx: -1, toolID: event.ContentBlock.ID, toolName: event.ContentBlock.Name})
		}
	case "content_block_delta":
		if event.Delta == nil {
			return nil, false, nil
		}
		if event.Delta.Text != "" {
			ops = append(ops, streamOp{kind: opText, text: event.Delta.Text})
		}
		if event.Delta.PartialJSON != "" {
			ops = append(ops, streamOp{kind: opToolArgs, idx: -1, toolArgs: event.Delta.PartialJSON})
		}
	case "message_delta":
		if event.Delta != nil && event.Delta.StopReason != "" {
			ops = append(ops, streamOp{kind: opStop, stop: event.Delta.StopReason})
		}
		if event.Usage != nil {
			ops = append(ops, streamOp{kind: opUsage, usageOut: event.Usage.OutputTokens})
		}
	case "message_stop":
		ops = append(ops, streamOp{kind: opDone})
	}
	return ops, event.Type == "message_stop", nil
}

type AnthropicAdapter struct {
	core *coreClient
}

func NewAnthropicAdapter(name, baseURL, apiKey string, timeout time.Duration) (*AnthropicAdapter, error) {
	if baseURL != "" {
		if err := config.ValidateBaseURL(baseURL); err != nil {
			return nil, fmt.Errorf("model: invalid base_url %q: %w", baseURL, err)
		}
	}
	return newAnthropicAdapter(name, baseURL, apiKey, timeout, defaultStreamingIdleTimeout), nil
}

func newAnthropicAdapter(name, baseURL, apiKey string, timeout, idleTimeout time.Duration) *AnthropicAdapter {
	if baseURL == "" {
		baseURL = anthropicDefaultBaseURL
	}
	return &AnthropicAdapter{core: newCoreClient(name, baseURL, apiKey, timeout, idleTimeout, anthropicCodec{})}
}

func (a *AnthropicAdapter) Name() string { return a.core.name }

func (a *AnthropicAdapter) GenerateContent(ctx context.Context, req *adkmodel.LLMRequest, stream bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return a.core.GenerateContent(ctx, req, stream)
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
