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

const openAIResponsesDefaultBaseURL = "https://api.openai.com"

type OpenAIResponsesAdapter struct {
	name                 string
	baseURL              string
	apiKey               string
	httpClient           *http.Client
	retry                RetryConfig
	streamingIdleTimeout time.Duration
}

func NewOpenAIResponsesAdapter(name, baseURL, apiKey string, timeout time.Duration) (*OpenAIResponsesAdapter, error) {
	if baseURL != "" {
		if err := validateBaseURL(baseURL); err != nil {
			return nil, fmt.Errorf("model: invalid base_url %q: %w", baseURL, err)
		}
	}
	return newOpenAIResponsesAdapter(name, baseURL, apiKey, timeout, defaultStreamingIdleTimeout), nil
}

func newOpenAIResponsesAdapter(name, baseURL, apiKey string, timeout, idleTimeout time.Duration) *OpenAIResponsesAdapter {
	if baseURL == "" {
		baseURL = openAIResponsesDefaultBaseURL
	}
	if timeout <= 0 {
		timeout = defaultRequestTimeout
	}
	if idleTimeout <= 0 {
		idleTimeout = defaultStreamingIdleTimeout
	}
	return &OpenAIResponsesAdapter{
		name:                 name,
		baseURL:              strings.TrimRight(baseURL, "/"),
		apiKey:               apiKey,
		httpClient:           &http.Client{Timeout: timeout, CheckRedirect: rejectCrossOriginRedirect},
		retry:                defaultRetryConfig(),
		streamingIdleTimeout: idleTimeout,
	}
}

func (a *OpenAIResponsesAdapter) Name() string { return a.name }

func (a *OpenAIResponsesAdapter) GenerateContent(ctx context.Context, req *adkmodel.LLMRequest, stream bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		telemetry := newModelTelemetry("openai_responses", a.name)
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
			yield(nil, fmt.Errorf("model: failed to read openai responses response: %w", err))
			return
		}
		out, err := parseOpenAIResponsesResponse(payload)
		if err != nil {
			telemetry.finish(ctx, nil, err)
			yield(nil, err)
			return
		}
		telemetry.finish(ctx, out, nil)
		yield(out, nil)
	}
}

func (a *OpenAIResponsesAdapter) do(ctx context.Context, req *adkmodel.LLMRequest, stream bool) (*http.Response, int, error) {
	retryCount := 0
	body, err := buildOpenAIResponsesRequest(req, stream)
	if err != nil {
		return nil, retryCount, fmt.Errorf("model: failed to build openai responses request: %w", err)
	}
	retry := retryConfigForRequest(a.retry, req)
	retry.RetryCount = &retryCount
	resp, err := retryHTTP(ctx, retry, func() (*http.Response, error) {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+"/v1/responses", bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("model: failed to create openai responses request: %w", err)
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

func (a *OpenAIResponsesAdapter) stream(ctx context.Context, body io.ReadCloser, yield func(*adkmodel.LLMResponse, error) bool) {
	err := scanSSE(ctx, body, a.streamingIdleTimeout, func(data []byte) error {
		var event responsesStreamEvent
		if err := json.Unmarshal(data, &event); err != nil {
			return fmt.Errorf("model: failed to parse openai responses stream event: %w", err)
		}
		switch event.Type {
		case "response.output_text.delta":
			if event.Delta == "" {
				return nil
			}
			response := &adkmodel.LLMResponse{
				Content: &genai.Content{Role: "model", Parts: []*genai.Part{{Text: event.Delta}}},
				Partial: true,
			}
			if !yield(response, nil) {
				return errStopYield
			}
		case "response.completed":
			if event.Response == nil {
				return nil
			}
			response, err := parseOpenAIResponsesValue(*event.Response)
			if err != nil {
				return err
			}
			if !yield(response, nil) {
				return errStopYield
			}
		}
		return nil
	})
	if err != nil && !errors.Is(err, errStopYield) && !errors.Is(err, io.EOF) {
		yield(nil, err)
	}
}

type openAIResponsesRequest struct {
	Model           string                 `json:"model"`
	Instructions    string                 `json:"instructions,omitempty"`
	Input           []openAIResponsesInput `json:"input,omitempty"`
	Stream          bool                   `json:"stream,omitempty"`
	Tools           []openAIResponsesTool  `json:"tools,omitempty"`
	MaxOutputTokens int32                  `json:"max_output_tokens,omitempty"`
	Temperature     *float32               `json:"temperature,omitempty"`
	TopP            *float32               `json:"top_p,omitempty"`
	Stop            []string               `json:"stop,omitempty"`
}

type openAIResponsesInput struct {
	Type      string                   `json:"type,omitempty"`
	Role      string                   `json:"role,omitempty"`
	Content   []openAIResponsesContent `json:"content,omitempty"`
	CallID    string                   `json:"call_id,omitempty"`
	Name      string                   `json:"name,omitempty"`
	Arguments string                   `json:"arguments,omitempty"`
	Output    string                   `json:"output,omitempty"`
}

type openAIResponsesContent struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

type openAIResponsesTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

func buildOpenAIResponsesRequest(req *adkmodel.LLMRequest, stream bool) ([]byte, error) {
	body := openAIResponsesRequest{Model: req.Model, Stream: stream}
	if req.Config != nil {
		body.MaxOutputTokens = req.Config.MaxOutputTokens
		body.Temperature = req.Config.Temperature
		body.TopP = req.Config.TopP
		body.Stop = req.Config.StopSequences
		if req.Config.SystemInstruction != nil {
			body.Instructions = contentText(req.Config.SystemInstruction)
		}
		for _, tool := range req.Config.Tools {
			if tool == nil {
				continue
			}
			for _, declaration := range tool.FunctionDeclarations {
				if declaration == nil {
					continue
				}
				body.Tools = append(body.Tools, openAIResponsesTool{
					Type:        "function",
					Name:        declaration.Name,
					Description: declaration.Description,
					Parameters:  toolParams(declaration),
				})
			}
		}
	}
	for _, content := range req.Contents {
		body.Input = append(body.Input, contentToOpenAIResponsesInput(content)...)
	}
	return json.Marshal(body)
}

func contentToOpenAIResponsesInput(content *genai.Content) []openAIResponsesInput {
	if content == nil {
		return nil
	}
	role := "user"
	if content.Role == "model" {
		role = "assistant"
	}
	var text []openAIResponsesContent
	var items []openAIResponsesInput
	for _, part := range content.Parts {
		switch {
		case part.Text != "":
			contentType := "input_text"
			if role == "assistant" {
				contentType = "output_text"
			}
			text = append(text, openAIResponsesContent{Type: contentType, Text: part.Text})
		case part.FunctionCall != nil:
			tc := toCanonicalToolCall(part.FunctionCall)
			items = append(items, openAIResponsesInput{Type: "function_call", CallID: tc.ID, Name: tc.Name, Arguments: string(tc.Arguments)})
		case part.FunctionResponse != nil:
			result := "{}"
			if len(part.FunctionResponse.Response) > 0 {
				if encoded, err := json.Marshal(part.FunctionResponse.Response); err == nil {
					result = string(encoded)
				}
			}
			items = append(items, openAIResponsesInput{Type: "function_call_output", CallID: part.FunctionResponse.ID, Output: result})
		}
	}
	if len(text) > 0 {
		items = append([]openAIResponsesInput{{Role: role, Content: text}}, items...)
	}
	return items
}

type openAIResponsesResponse struct {
	Model  string                  `json:"model"`
	Output []openAIResponsesOutput `json:"output"`
	Usage  *openAIResponsesUsage   `json:"usage,omitempty"`
}

type openAIResponsesOutput struct {
	Type      string                   `json:"type"`
	Role      string                   `json:"role,omitempty"`
	Content   []openAIResponsesContent `json:"content,omitempty"`
	CallID    string                   `json:"call_id,omitempty"`
	Name      string                   `json:"name,omitempty"`
	Arguments string                   `json:"arguments,omitempty"`
}

type openAIResponsesUsage struct {
	InputTokens  int32 `json:"input_tokens"`
	OutputTokens int32 `json:"output_tokens"`
	TotalTokens  int32 `json:"total_tokens"`
}

type responsesStreamEvent struct {
	Type     string                   `json:"type"`
	Delta    string                   `json:"delta,omitempty"`
	Response *openAIResponsesResponse `json:"response,omitempty"`
}

func parseOpenAIResponsesResponse(body []byte) (*adkmodel.LLMResponse, error) {
	var response openAIResponsesResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("model: failed to parse openai responses response: %w", err)
	}
	return parseOpenAIResponsesValue(response)
}

func parseOpenAIResponsesValue(response openAIResponsesResponse) (*adkmodel.LLMResponse, error) {
	content := &genai.Content{Role: "model"}
	for _, item := range response.Output {
		switch item.Type {
		case "message":
			for _, part := range item.Content {
				if part.Text != "" {
					content.Parts = append(content.Parts, &genai.Part{Text: part.Text})
				}
			}
		case "function_call":
			args := json.RawMessage(item.Arguments)
			if len(bytes.TrimSpace(args)) == 0 {
				args = json.RawMessage("{}")
			}
			fc, err := canonicalToFunctionCall(ToolCall{ID: item.CallID, Name: item.Name, Arguments: args})
			if err != nil {
				return nil, err
			}
			content.Parts = append(content.Parts, &genai.Part{FunctionCall: fc})
		}
	}
	if len(content.Parts) == 0 {
		return nil, errors.New("model: openai responses output was empty")
	}
	var usage *genai.GenerateContentResponseUsageMetadata
	if response.Usage != nil {
		usage = &genai.GenerateContentResponseUsageMetadata{
			PromptTokenCount:     response.Usage.InputTokens,
			CandidatesTokenCount: response.Usage.OutputTokens,
			TotalTokenCount:      response.Usage.TotalTokens,
		}
	}
	return &adkmodel.LLMResponse{Content: content, ModelVersion: response.Model, UsageMetadata: usage, TurnComplete: true}, nil
}
