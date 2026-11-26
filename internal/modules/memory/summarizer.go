package memory

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/model"
)

const (
	memorySummaryOutputBound = 1 << 16
	memorySummaryMaxTokens   = 1 << 14
)

type ModelSource interface {
	LLM(ctx context.Context, name string) (adkmodel.LLM, error)
}

func BuildSummary(ctx context.Context, modelConfig *model.Config, modelRoutes map[string]model.ModelRoute, models ModelSource, logger *slog.Logger, observer Observer) (*SummaryModel, Summarizer) {
	if modelConfig == nil || models == nil {
		observeUnavailableSummary(ctx, observer)
		return nil, nil
	}
	role := strings.TrimSpace(modelConfig.Routing["summarize"])
	if role == "" {
		role = "auxiliary"
	}
	candidate := role
	if route, ok := modelRoutes[role]; ok && len(route.Candidates) > 0 {
		candidate = route.Candidates[0]
	}
	definition, ok := modelConfig.Definitions[candidate]
	if !ok || strings.TrimSpace(definition.Model) == "" {
		observeUnavailableSummary(ctx, observer)
		return nil, nil
	}
	modelName := definition.Model
	if _, routed := modelRoutes[role]; routed {
		modelName = role
	}
	llm, err := models.LLM(ctx, modelName)
	if err != nil {
		if logger != nil {
			logger.WarnContext(ctx, "memory range summarizer unavailable", "component", "memory", "route", role, "model", definition.Model, "error", err)
		}
		observeUnavailableSummary(ctx, observer)
		return nil, nil
	}
	summaryModel := &SummaryModel{
		Protocol:         definition.Protocol,
		Name:             definition.Model,
		Tokenizer:        definition.Capabilities.Tokenizer,
		ContextTokens:    definition.Capabilities.ContextTokens,
		StructuredOutput: definition.Capabilities.StructuredOutput,
	}
	return summaryModel, &llmSummarizer{llm: llm, bound: memorySummaryOutputBound}
}

func observeUnavailableSummary(ctx context.Context, observer Observer) {
	if observer == nil {
		return
	}
	observer(ctx, &Observation{Outcome: string(ErrorCodeUnavailable)})
}

type llmSummarizer struct {
	llm   adkmodel.LLM
	bound int
}

func (s *llmSummarizer) Summarize(ctx context.Context, prompt *SummaryPrompt) (string, error) {
	if ctx == nil {
		return "", fault.New(ErrorCodeInvalidArgument, "context must not be nil")
	}
	if s == nil || s.llm == nil {
		return "", fault.New(ErrorCodeInvalidArgument, "memory summarizer has no model")
	}
	if prompt == nil {
		return "", fault.New(ErrorCodeInvalidArgument, "summary prompt must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	maxTokens := prompt.MaxTokens
	if maxTokens <= 0 || maxTokens > memorySummaryMaxTokens {
		maxTokens = memorySummaryMaxTokens
	}
	var request strings.Builder
	request.WriteString("Summarize the closed session range below into a compact factual digest. Do not invent details.\n\n")
	for i := range prompt.Documents {
		fmt.Fprintf(&request, "[seq %d-%d trust=%s]\n%s\n\n",
			prompt.Documents[i].FromSequence, prompt.Documents[i].ToSequence,
			prompt.Documents[i].Trust, prompt.Documents[i].Content)
	}
	var collected strings.Builder
	for response, err := range s.llm.GenerateContent(ctx, &adkmodel.LLMRequest{
		Contents: []*genai.Content{{Role: "user", Parts: []*genai.Part{{Text: request.String()}}}},
		Config:   &genai.GenerateContentConfig{MaxOutputTokens: int32(maxTokens)},
	}, false) {
		if err != nil {
			return "", err
		}
		if response == nil || response.Content == nil {
			continue
		}
		for _, part := range response.Content.Parts {
			if part == nil || strings.TrimSpace(part.Text) == "" {
				continue
			}
			if collected.Len()+len(part.Text) > s.bound {
				return collected.String(), nil
			}
			collected.WriteString(part.Text)
		}
	}
	if strings.TrimSpace(collected.String()) == "" {
		return "", fault.New(ErrorCodeUnavailable, "summarizer returned no usable content")
	}
	return collected.String(), nil
}
