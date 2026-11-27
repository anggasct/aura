package profile

import (
	"context"
	"fmt"
	"strings"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/model"
)

type ModelSource interface {
	LLM(ctx context.Context, name string) (adkmodel.LLM, error)
}

func ProfilingRole(models model.Config) string {
	if role := strings.TrimSpace(models.Routing["profiling"]); role != "" {
		return role
	}
	return "auxiliary"
}

func ResolveProducer(models model.Config, routes model.Routes) (ModelProducer, error) {
	role := ProfilingRole(models)
	route, ok := routes[role]
	if !ok {
		return ModelProducer{}, fault.New(ErrorCodeProfileUnavailable, fmt.Sprintf("profiling route %q is not configured", role))
	}
	if len(route.Candidates) == 0 {
		return ModelProducer{}, fault.New(ErrorCodeProfileUnavailable, fmt.Sprintf("profiling route %q names no candidate", role))
	}
	definition, ok := models.Definitions[route.Candidates[0]]
	if !ok {
		return ModelProducer{}, fault.New(ErrorCodeProfileUnavailable, fmt.Sprintf("profiling candidate %q is not defined", route.Candidates[0]))
	}
	if strings.TrimSpace(definition.Protocol) == "" || strings.TrimSpace(definition.Model) == "" {
		return ModelProducer{}, fault.New(ErrorCodeProfileUnavailable, fmt.Sprintf("profiling candidate %q carries no identity", route.Candidates[0]))
	}
	return ModelProducer{Provider: definition.Protocol, Model: definition.Model}, nil
}

type llmCaller struct {
	llm      adkmodel.LLM
	producer ModelProducer
	bound    int
}

func NewModelCaller(llm adkmodel.LLM, producer ModelProducer, maxOutputTokens int) ModelCaller {
	return &llmCaller{
		llm:      llm,
		producer: producer,
		bound:    maxOutputTokens*4 + 1,
	}
}

func (c *llmCaller) Extract(ctx context.Context, req *ExtractRequest) (*ExtractResult, error) {
	if ctx == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "context must not be nil")
	}
	if req == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "extraction request must not be nil")
	}
	if c.llm == nil {
		return nil, fault.New(ErrorCodeProfileUnavailable, "extractor has no model")
	}
	if req.MaxOutputTokens <= 0 || req.MaxOutputTokens > 1<<20 {
		return nil, fault.New(ErrorCodeInvalidArgument, "extraction request carries an invalid output bound")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var message strings.Builder
	message.WriteString(req.Prompt)
	for _, source := range req.Sources {
		message.WriteString("\n---\n")
		message.WriteString(source.Text)
	}
	var collected strings.Builder
	oversized := false
	for response, err := range c.llm.GenerateContent(ctx, &adkmodel.LLMRequest{
		Contents: []*genai.Content{{Role: "user", Parts: []*genai.Part{{Text: message.String()}}}},
		Config:   &genai.GenerateContentConfig{MaxOutputTokens: int32(req.MaxOutputTokens)},
	}, false) {
		if err != nil {
			return nil, err
		}
		if response == nil || response.Content == nil {
			continue
		}
		for _, part := range response.Content.Parts {
			if part == nil || strings.TrimSpace(part.Text) == "" {
				continue
			}
			if collected.Len()+len(part.Text) > c.bound {
				oversized = true
				break
			}
			collected.WriteString(part.Text)
		}
		if oversized {
			break
		}
	}
	if oversized {
		return nil, fault.New(ErrorCodeProfileInvalid, "profiling output exceeds the configured bound")
	}
	producer := c.producer
	if producer.Model == "" {
		producer.Model = "unknown"
	}
	return &ExtractResult{Text: collected.String(), Producer: producer}, nil
}
