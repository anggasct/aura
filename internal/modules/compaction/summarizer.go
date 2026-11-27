package compaction

import (
	"context"
	"fmt"
	"strings"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/model"
)

const maxSummarizerOutputTokens = 1 << 20

func ResolveProducer(models model.Config, routes model.Routes) (Producer, error) {
	role := models.Routing["compression"]
	if strings.TrimSpace(role) == "" {
		role = "auxiliary"
	}
	route, ok := routes[role]
	if !ok {
		return Producer{}, fault.New(ErrorCodeInvalidArgument, fmt.Sprintf("compression route %q is not configured", role))
	}
	if len(route.Candidates) == 0 {
		return Producer{}, fault.New(ErrorCodeInvalidArgument, fmt.Sprintf("compression route %q names no candidate", role))
	}
	definition, ok := models.Definitions[route.Candidates[0]]
	if !ok {
		return Producer{}, fault.New(ErrorCodeInvalidArgument, fmt.Sprintf("compression candidate %q is not defined", route.Candidates[0]))
	}
	if strings.TrimSpace(definition.Protocol) == "" || strings.TrimSpace(definition.Model) == "" {
		return Producer{}, fault.New(ErrorCodeInvalidArgument, fmt.Sprintf("compression candidate %q carries no identity", route.Candidates[0]))
	}
	return Producer{Provider: definition.Protocol, Model: definition.Model}, nil
}

type LLMSummarizer struct {
	llm      adkmodel.LLM
	producer Producer
	bound    int
}

func NewLLMSummarizer(llm adkmodel.LLM, producer Producer, maxOutputTokens int) (*LLMSummarizer, error) {
	if llm == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "summarizer model must not be nil")
	}
	if maxOutputTokens <= 0 || maxOutputTokens > maxSummarizerOutputTokens {
		return nil, fault.New(ErrorCodeInvalidArgument, "summarizer output bound must be positive and within limit")
	}
	return &LLMSummarizer{llm: llm, producer: producer, bound: OutputByteBound(maxOutputTokens) + 1}, nil
}

func (s *LLMSummarizer) Summarize(ctx context.Context, req *SummarizeRequest) (*SummarizeResult, error) {
	if s == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "summarizer must not be nil")
	}
	if ctx == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "context must not be nil")
	}
	if req == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "summarize request must not be nil")
	}
	if s.llm == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "summarizer has no model")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if req.MaxOutputTokens <= 0 || req.MaxOutputTokens > maxSummarizerOutputTokens {
		return nil, fault.New(ErrorCodeInvalidArgument, "summarize request carries an invalid output bound")
	}
	message := req.Prompt + "\n\n" + strings.Join(req.Sources, "\n---\n")
	var collected strings.Builder
	oversized := false
	for response, err := range s.llm.GenerateContent(ctx, &adkmodel.LLMRequest{
		Contents: []*genai.Content{{Role: "user", Parts: []*genai.Part{{Text: message}}}},
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
			if collected.Len()+len(part.Text) > s.bound {
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
		return nil, fault.New(ErrorCodeSummaryInvalid, "summarizer output exceeds the output bound")
	}
	if strings.TrimSpace(collected.String()) == "" {
		return nil, fault.New(ErrorCodeSummaryUnavailable, "summarizer returned no text")
	}
	return &SummarizeResult{Text: collected.String(), Producer: s.producer}, nil
}
