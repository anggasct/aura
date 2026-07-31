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
	"net/url"
	"strings"
	"time"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

const geminiDefaultBaseURL = "https://generativelanguage.googleapis.com"

type GeminiAdapter struct {
	name                 string
	baseURL              string
	apiKey               string
	httpClient           *http.Client
	retry                RetryConfig
	streamingIdleTimeout time.Duration
}

func NewGeminiAdapter(name, baseURL, apiKey string, timeout time.Duration) (*GeminiAdapter, error) {
	if baseURL != "" {
		if err := validateBaseURL(baseURL); err != nil {
			return nil, fmt.Errorf("model: invalid base_url %q: %w", baseURL, err)
		}
	}
	return newGeminiAdapter(name, baseURL, apiKey, timeout, defaultStreamingIdleTimeout), nil
}

func newGeminiAdapter(name, baseURL, apiKey string, timeout, idleTimeout time.Duration) *GeminiAdapter {
	if baseURL == "" {
		baseURL = geminiDefaultBaseURL
	}
	if timeout <= 0 {
		timeout = defaultRequestTimeout
	}
	if idleTimeout <= 0 {
		idleTimeout = defaultStreamingIdleTimeout
	}
	return &GeminiAdapter{
		name:                 name,
		baseURL:              strings.TrimRight(baseURL, "/"),
		apiKey:               apiKey,
		httpClient:           &http.Client{Timeout: timeout, CheckRedirect: rejectCrossOriginRedirect},
		retry:                defaultRetryConfig(),
		streamingIdleTimeout: idleTimeout,
	}
}

func (a *GeminiAdapter) Name() string { return a.name }

func (a *GeminiAdapter) GenerateContent(ctx context.Context, req *adkmodel.LLMRequest, stream bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		telemetry := newModelTelemetry(ctx, "gemini_native", a.name)
		resp, retries, err := a.do(ctx, req, stream)
		telemetry.retries = retries
		if err != nil {
			telemetry.finish(nil, err)
			yield(nil, err)
			return
		}
		defer resp.Body.Close()
		if cErr := classifyHTTPResponse(resp, "gemini"); cErr != nil {
			telemetry.finish(nil, cErr)
			yield(nil, cErr)
			return
		}
		if stream {
			a.stream(ctx, resp.Body, func(response *adkmodel.LLMResponse, streamErr error) bool {
				if streamErr != nil || (response != nil && response.TurnComplete) {
					telemetry.finish(response, streamErr)
				}
				return yield(response, streamErr)
			})
			telemetry.finishIfNeeded()
			return
		}
		payload, err := io.ReadAll(resp.Body)
		if err != nil {
			telemetry.finish(nil, err)
			yield(nil, fmt.Errorf("model: failed to read gemini response: %w", err))
			return
		}
		out, err := parseGeminiResponse(payload)
		if err != nil {
			telemetry.finish(nil, err)
			yield(nil, err)
			return
		}
		telemetry.finish(out, nil)
		yield(out, nil)
	}
}

func (a *GeminiAdapter) do(ctx context.Context, req *adkmodel.LLMRequest, stream bool) (*http.Response, int, error) {
	retryCount := 0
	body, err := buildGeminiRequest(req)
	if err != nil {
		return nil, retryCount, fmt.Errorf("model: failed to build gemini request: %w", err)
	}
	action := "generateContent"
	query := ""
	if stream {
		action = "streamGenerateContent"
		query = "?alt=sse"
	}
	endpoint := a.baseURL + "/v1beta/models/" + url.PathEscape(req.Model) + ":" + action + query
	retry := retryConfigForRequest(a.retry, req)
	retry.RetryCount = &retryCount
	resp, err := retryHTTP(ctx, retry, func() (*http.Response, error) {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("model: failed to create gemini request: %w", err)
		}
		httpReq.Header.Set("Content-Type", "application/json")
		if a.apiKey != "" {
			httpReq.Header.Set("x-goog-api-key", a.apiKey)
		}
		resp, err := a.httpClient.Do(httpReq)
		if err != nil {
			return nil, classifyRequestError(err)
		}
		return resp, nil
	})
	return resp, retryCount, err
}

func (a *GeminiAdapter) stream(ctx context.Context, body io.ReadCloser, yield func(*adkmodel.LLMResponse, error) bool) {
	err := scanSSE(ctx, body, a.streamingIdleTimeout, func(data []byte) error {
		response, err := parseGeminiResponse(data)
		if err != nil {
			return err
		}
		response.Partial = !response.TurnComplete
		if !yield(response, nil) {
			return errStopYield
		}
		return nil
	})
	if err != nil && !errors.Is(err, errStopYield) && !errors.Is(err, io.EOF) {
		yield(nil, err)
	}
}

type geminiRequest struct {
	Contents          []geminiContent        `json:"contents"`
	SystemInstruction *geminiContent         `json:"systemInstruction,omitempty"`
	GenerationConfig  geminiGenerationConfig `json:"generationConfig,omitempty"`
	Tools             []geminiTool           `json:"tools,omitempty"`
}

type geminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []geminiPart `json:"parts"`
}

type geminiPart struct {
	Text             string                  `json:"text,omitempty"`
	FunctionCall     *geminiFunctionCall     `json:"functionCall,omitempty"`
	FunctionResponse *geminiFunctionResponse `json:"functionResponse,omitempty"`
}

type geminiFunctionCall struct {
	Name string         `json:"name"`
	Args map[string]any `json:"args,omitempty"`
}

type geminiFunctionResponse struct {
	Name     string         `json:"name"`
	Response map[string]any `json:"response"`
}

type geminiGenerationConfig struct {
	MaxOutputTokens int32    `json:"maxOutputTokens,omitempty"`
	Temperature     *float32 `json:"temperature,omitempty"`
	TopP            *float32 `json:"topP,omitempty"`
	StopSequences   []string `json:"stopSequences,omitempty"`
}

type geminiTool struct {
	FunctionDeclarations []geminiFunctionDeclaration `json:"functionDeclarations"`
}

type geminiFunctionDeclaration struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

func buildGeminiRequest(req *adkmodel.LLMRequest) ([]byte, error) {
	body := geminiRequest{}
	if req.Config != nil {
		body.GenerationConfig = geminiGenerationConfig{
			MaxOutputTokens: req.Config.MaxOutputTokens,
			Temperature:     req.Config.Temperature,
			TopP:            req.Config.TopP,
			StopSequences:   req.Config.StopSequences,
		}
		if req.Config.SystemInstruction != nil {
			body.SystemInstruction = &geminiContent{Parts: []geminiPart{{Text: contentText(req.Config.SystemInstruction)}}}
		}
		for _, tool := range req.Config.Tools {
			if tool == nil {
				continue
			}
			geminiToolSpec := geminiTool{}
			for _, declaration := range tool.FunctionDeclarations {
				if declaration == nil {
					continue
				}
				geminiToolSpec.FunctionDeclarations = append(geminiToolSpec.FunctionDeclarations, geminiFunctionDeclaration{
					Name:        declaration.Name,
					Description: declaration.Description,
					Parameters:  toolParams(declaration),
				})
			}
			if len(geminiToolSpec.FunctionDeclarations) > 0 {
				body.Tools = append(body.Tools, geminiToolSpec)
			}
		}
	}
	for _, content := range req.Contents {
		converted := contentToGeminiContent(content)
		if converted.Parts != nil {
			body.Contents = append(body.Contents, converted)
		}
	}
	return json.Marshal(body)
}

func contentToGeminiContent(content *genai.Content) geminiContent {
	if content == nil {
		return geminiContent{}
	}
	role := "user"
	if content.Role == "model" {
		role = "model"
	}
	converted := geminiContent{Role: role}
	for _, part := range content.Parts {
		switch {
		case part.Text != "":
			converted.Parts = append(converted.Parts, geminiPart{Text: part.Text})
		case part.FunctionCall != nil:
			converted.Parts = append(converted.Parts, geminiPart{FunctionCall: &geminiFunctionCall{Name: part.FunctionCall.Name, Args: part.FunctionCall.Args}})
		case part.FunctionResponse != nil:
			converted.Parts = append(converted.Parts, geminiPart{FunctionResponse: &geminiFunctionResponse{Name: part.FunctionResponse.Name, Response: part.FunctionResponse.Response}})
		}
	}
	return converted
}

type geminiResponse struct {
	ModelVersion  string               `json:"modelVersion,omitempty"`
	Candidates    []geminiCandidate    `json:"candidates"`
	UsageMetadata *geminiUsageMetadata `json:"usageMetadata,omitempty"`
}

type geminiCandidate struct {
	Content      geminiContent `json:"content"`
	FinishReason string        `json:"finishReason,omitempty"`
}

type geminiUsageMetadata struct {
	PromptTokenCount     int32 `json:"promptTokenCount"`
	CandidatesTokenCount int32 `json:"candidatesTokenCount"`
	TotalTokenCount      int32 `json:"totalTokenCount"`
}

func parseGeminiResponse(body []byte) (*adkmodel.LLMResponse, error) {
	var response geminiResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("model: failed to parse gemini response: %w", err)
	}
	if len(response.Candidates) == 0 {
		return nil, fmt.Errorf("model: gemini response had no candidates")
	}
	content := &genai.Content{Role: "model"}
	for _, part := range response.Candidates[0].Content.Parts {
		if part.Text != "" {
			content.Parts = append(content.Parts, &genai.Part{Text: part.Text})
		}
		if part.FunctionCall != nil {
			content.Parts = append(content.Parts, &genai.Part{FunctionCall: &genai.FunctionCall{Name: part.FunctionCall.Name, Args: part.FunctionCall.Args}})
		}
	}
	if len(content.Parts) == 0 {
		return nil, fmt.Errorf("model: gemini response had no content")
	}
	var usage *genai.GenerateContentResponseUsageMetadata
	if response.UsageMetadata != nil {
		usage = &genai.GenerateContentResponseUsageMetadata{
			PromptTokenCount:     response.UsageMetadata.PromptTokenCount,
			CandidatesTokenCount: response.UsageMetadata.CandidatesTokenCount,
			TotalTokenCount:      response.UsageMetadata.TotalTokenCount,
		}
	}
	complete := response.Candidates[0].FinishReason != "" && response.Candidates[0].FinishReason != "FINISH_REASON_UNSPECIFIED"
	return &adkmodel.LLMResponse{Content: content, ModelVersion: response.ModelVersion, UsageMetadata: usage, TurnComplete: complete}, nil
}
