package model

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/anggasct/aura/internal/config"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

const geminiDefaultBaseURL = "https://generativelanguage.googleapis.com"

type geminiCodec struct{}

func (geminiCodec) protocol() string { return "gemini_native" }

func (geminiCodec) endpoint(baseURL string, req *adkmodel.LLMRequest, stream bool) string {
	action := "generateContent"
	query := ""
	if stream {
		action = "streamGenerateContent"
		query = "?alt=sse"
	}
	return baseURL + "/v1beta/models/" + url.PathEscape(req.Model) + ":" + action + query
}

func (geminiCodec) buildRequest(req *adkmodel.LLMRequest, stream bool) ([]byte, error) {
	return buildGeminiRequest(req)
}

func (geminiCodec) decodeResponse(body []byte) (*adkmodel.LLMResponse, error) {
	return parseGeminiResponse(body)
}

func (geminiCodec) setAuthHeaders(req *http.Request, apiKey string) {
	req.Header.Set("x-goog-api-key", apiKey)
}

func (geminiCodec) decodeStreamEvent(data []byte) ([]streamOp, bool, error) {
	response, err := parseGeminiResponse(data)
	if err != nil {
		return nil, false, err
	}
	complete := response.TurnComplete
	var ops []streamOp
	for _, part := range response.Content.Parts {
		if part.Text != "" {
			ops = append(ops, streamOp{kind: opText, text: part.Text, usageIn: usageIn(response), usageOut: usageOut(response)})
		}
		if part.FunctionCall != nil {
			args, err := json.Marshal(part.FunctionCall.Args)
			if err != nil {
				return nil, false, fmt.Errorf("model: failed to marshal gemini tool call args: %w", err)
			}
			ops = append(ops,
				streamOp{kind: opToolStart, idx: -1, toolID: part.FunctionCall.ID, toolName: part.FunctionCall.Name},
				streamOp{kind: opToolArgs, idx: -1, toolArgs: string(args)},
			)
		}
	}
	if complete {
		ops = append(ops, streamOp{kind: opDone, final: response})
	}
	return ops, complete, nil
}

func usageIn(response *adkmodel.LLMResponse) int32 {
	if response.UsageMetadata == nil {
		return 0
	}
	return response.UsageMetadata.PromptTokenCount
}

func usageOut(response *adkmodel.LLMResponse) int32 {
	if response.UsageMetadata == nil {
		return 0
	}
	return response.UsageMetadata.CandidatesTokenCount
}

type GeminiAdapter struct {
	core *coreClient
}

func NewGeminiAdapter(name, baseURL, apiKey string, timeout time.Duration) (*GeminiAdapter, error) {
	if baseURL != "" {
		if err := config.ValidateBaseURL(baseURL); err != nil {
			return nil, fmt.Errorf("model: invalid base_url: %w", err)
		}
	}
	return newGeminiAdapter(nil, name, baseURL, apiKey, timeout, defaultStreamingIdleTimeout), nil
}

func newGeminiAdapter(logger *slog.Logger, name, baseURL, apiKey string, timeout, idleTimeout time.Duration) *GeminiAdapter {
	if baseURL == "" {
		baseURL = geminiDefaultBaseURL
	}
	return &GeminiAdapter{core: newCoreClient(logger, name, baseURL, apiKey, timeout, idleTimeout, geminiCodec{})}
}

func (a *GeminiAdapter) Name() string { return a.core.name }

func (a *GeminiAdapter) GenerateContent(ctx context.Context, req *adkmodel.LLMRequest, stream bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return a.core.GenerateContent(ctx, req, stream)
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
				params, err := toolParams(declaration)
				if err != nil {
					return nil, err
				}
				geminiToolSpec.FunctionDeclarations = append(geminiToolSpec.FunctionDeclarations, geminiFunctionDeclaration{
					Name:        declaration.Name,
					Description: declaration.Description,
					Parameters:  params,
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
		return nil, codedError(ErrorCodeProtocolInvalid, nil, "model: gemini response had no candidates")
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
		return nil, codedError(ErrorCodeProtocolInvalid, nil, "model: gemini response had no content")
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
