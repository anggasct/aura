package memory

import (
	"context"
	"strings"
	"time"
	"unicode/utf8"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/anggasct/aura/internal/runtime"
	"github.com/anggasct/aura/internal/runtime/ingress"
)

const SpanMemoryRecall = "memory_recall"

const (
	AttrRecallOutcome        = "aura.recall.outcome"
	AttrRecallSummarized     = "aura.recall.summarized"
	AttrRecallDocuments      = "aura.recall.documents"
	AttrRecallScreened       = "aura.recall.screened"
	AttrRecallScores         = "aura.recall.scores"
	AttrRecallSources        = "aura.recall.sources"
	AttrRecallModelSystem    = "aura.recall.model_system"
	AttrRecallModelName      = "aura.recall.model_name"
	AttrRecallSummaryVersion = "aura.recall.summary_version"
)

type ProviderConfig struct {
	Model          *SummaryModel
	Summarizer     Summarizer
	TracerProvider trace.TracerProvider
	Now            func() time.Time
}

type Provider struct {
	service    *Service
	model      *SummaryModel
	summarizer Summarizer
	tracer     trace.Tracer
	now        func() time.Time
}

func NewProvider(service *Service, config ProviderConfig) (*Provider, error) {
	if service == nil {
		return nil, Errorf(ErrorCodeInvalidArgument, "memory service must not be nil")
	}
	tp := config.TracerProvider
	if tp == nil {
		tp = otel.GetTracerProvider()
	}
	now := config.Now
	if now == nil {
		now = time.Now
	}
	return &Provider{
		service:    service,
		model:      config.Model,
		summarizer: config.Summarizer,
		tracer:     tp.Tracer(SpanMemoryRecall),
		now:        now,
	}, nil
}

func boundedRecallQuery(parts []runtimeingress.InputPart) string {
	texts := make([]string, 0, len(parts))
	for i := range parts {
		if trimmed := strings.TrimSpace(parts[i].Text); trimmed != "" {
			texts = append(texts, trimmed)
		}
	}
	query := strings.Join(texts, "\n")
	if utf8.RuneCountInString(query) > maxQueryRunes {
		query = string([]rune(query)[:maxQueryRunes])
	}
	return query
}

func (p *Provider) RecallTurn(ctx context.Context, req *runtime.TurnRequest) (*runtime.UntrustedRecall, error) {
	if ctx == nil {
		return nil, Errorf(ErrorCodeInvalidArgument, "context must not be nil")
	}
	start := p.now().UTC()
	evidence, observation, err := p.recall(ctx, req)
	observation.Duration = p.now().UTC().Sub(start)
	p.service.observe(ctx, observation)
	p.emitSpan(ctx, observation)
	if err != nil {
		return nil, err
	}
	return evidence, nil
}

func (p *Provider) recall(ctx context.Context, req *runtime.TurnRequest) (*runtime.UntrustedRecall, *Observation, error) {
	observation := &Observation{}
	if req == nil {
		observation.Outcome = string(ErrorCodeInvalidArgument)
		return nil, observation, Errorf(ErrorCodeInvalidArgument, "turn request must not be nil")
	}
	if strings.TrimSpace(req.PrincipalID) == "" || strings.TrimSpace(req.SessionID) == "" {
		observation.Outcome = string(ErrorCodeInvalidArgument)
		return nil, observation, Errorf(ErrorCodeInvalidArgument, "recall owner and session must not be empty")
	}
	query := boundedRecallQuery(req.Parts)
	if query == "" {
		observation.Outcome = OutcomeEmpty
		return nil, observation, nil
	}
	result, err := p.service.RecallWithSummary(ctx, &RecallRequest{
		OwnerID:   req.PrincipalID,
		SessionID: req.SessionID,
		Query:     query,
		Before:    p.now().UTC(),
	}, p.model, p.summarizer)
	if err != nil {
		observation.Outcome = string(ErrorCodeUnavailable)
		if code, ok := CodeOf(err); ok {
			observation.Outcome = string(code)
		}
		return nil, observation, err
	}
	screened := p.screen(&result)
	observation.Screened = screened
	observation.Summarized = result.Summary != ""
	if p.model != nil {
		observation.ModelProtocol = p.model.Protocol
		observation.ModelName = p.model.Name
	}
	if result.Summary != "" {
		observation.SummaryVersion = p.service.summaryPrompt
	}
	for i := range result.Documents {
		if len(observation.Scores) < maxObservedScores {
			observation.Scores = append(observation.Scores, result.Documents[i].Score)
		}
		if len(observation.SourceIDs) < maxObservedSources {
			observation.SourceIDs = append(observation.SourceIDs, result.Documents[i].ID)
		}
	}
	if len(result.Documents) == 0 && result.Summary == "" {
		observation.Outcome = OutcomeEmpty
		return nil, observation, nil
	}
	observation.Documents = len(result.Documents)
	observation.Outcome = OutcomeOK
	return result.Evidence(), observation, nil
}

func (p *Provider) screen(result *RecallContext) int {
	if p.service.secrets == nil {
		return 0
	}
	screened := 0
	kept := result.Documents[:0]
	for i := range result.Documents {
		if p.service.secrets.Contains(result.Documents[i].Content) {
			screened++
			continue
		}
		kept = append(kept, result.Documents[i])
	}
	for i := len(kept); i < len(result.Documents); i++ {
		result.Documents[i] = RecallDocument{}
	}
	result.Documents = kept
	result.Provenance = provenanceFor(kept)
	if result.Summary != "" && p.service.secrets.Contains(result.Summary) {
		result.Summary = ""
		screened++
	}
	return screened
}

func (p *Provider) emitSpan(ctx context.Context, observation *Observation) {
	if p.tracer == nil {
		return
	}
	_, span := p.tracer.Start(ctx, SpanMemoryRecall)
	defer span.End()
	span.SetAttributes(
		attribute.String(AttrRecallOutcome, observation.Outcome),
		attribute.Bool(AttrRecallSummarized, observation.Summarized),
		attribute.Int(AttrRecallDocuments, observation.Documents),
		attribute.Int(AttrRecallScreened, observation.Screened),
	)
	if len(observation.Scores) > 0 {
		span.SetAttributes(attribute.Float64Slice(AttrRecallScores, observation.Scores))
	}
	if len(observation.SourceIDs) > 0 {
		span.SetAttributes(attribute.StringSlice(AttrRecallSources, observation.SourceIDs))
	}
	if observation.ModelProtocol != "" {
		span.SetAttributes(attribute.String(AttrRecallModelSystem, observation.ModelProtocol))
	}
	if observation.ModelName != "" {
		span.SetAttributes(attribute.String(AttrRecallModelName, observation.ModelName))
	}
	if observation.SummaryVersion != "" {
		span.SetAttributes(attribute.String(AttrRecallSummaryVersion, observation.SummaryVersion))
	}
	if observation.Outcome != OutcomeOK && observation.Outcome != OutcomeEmpty {
		span.SetStatus(codes.Error, "")
	} else {
		span.SetStatus(codes.Ok, "")
	}
}
