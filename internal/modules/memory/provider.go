package memory

import (
	"context"
	"github.com/anggasct/aura/internal/platform/fault"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/anggasct/aura/internal/platform/turn"
	"github.com/anggasct/aura/internal/platform/turn/ingress"
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

type RecallScopes interface {
	RecallScope(agentID string) ([]string, bool)
}

type ProviderConfig struct {
	Scopes         RecallScopes
	Model          *SummaryModel
	Summarizer     Summarizer
	TaintMarks     TaintMarksReader
	TracerProvider trace.TracerProvider
	Now            func() time.Time
}

type Provider struct {
	service    *Service
	scopes     RecallScopes
	model      *SummaryModel
	summarizer Summarizer
	taintMarks TaintMarksReader
	tracer     trace.Tracer
	now        func() time.Time
}

func NewProvider(service *Service, config *ProviderConfig) (*Provider, error) {
	if service == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "memory service must not be nil")
	}
	if config == nil || config.Scopes == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "memory recall scopes must not be nil")
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
		scopes:     config.Scopes,
		model:      config.Model,
		summarizer: config.Summarizer,
		taintMarks: config.TaintMarks,
		tracer:     tp.Tracer(SpanMemoryRecall),
		now:        now,
	}, nil
}

func (p *Provider) TurnContext(ctx context.Context, req *turn.TurnRequest) ([]turn.ContextBlock, error) {
	if req != nil && req.UntrustedContext != nil {
		return nil, nil
	}
	evidence, err := p.RecallTurn(ctx, req)
	if err != nil {
		return nil, err
	}
	if evidence == nil {
		return nil, nil
	}
	block := turn.ContextBlock{
		Order:     turn.ContextOrderRecall,
		Placement: turn.ContextPlacementUserPart,
		Content:   turn.RenderUntrustedRecall(evidence),
		Caveat:    turn.RecallEvidenceCaveat,
	}
	if evidence.Tainted {
		block.Taint = &turn.TaintMark{Source: turn.TaintSourceRecall}
	}
	return []turn.ContextBlock{block}, nil
}

func (p *Provider) Required(req *turn.TurnRequest) bool {
	return req != nil && req.RequireRecall
}

func (p *Provider) RecallTurn(ctx context.Context, req *turn.TurnRequest) (*turn.UntrustedRecall, error) {
	if ctx == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "context must not be nil")
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

func (p *Provider) recall(ctx context.Context, req *turn.TurnRequest) (*turn.UntrustedRecall, *Observation, error) {
	observation := &Observation{}
	if req == nil {
		observation.Outcome = string(ErrorCodeInvalidArgument)
		return nil, observation, fault.New(ErrorCodeInvalidArgument, "turn request must not be nil")
	}
	if strings.TrimSpace(req.PrincipalID) == "" || strings.TrimSpace(req.SessionID) == "" {
		observation.Outcome = string(ErrorCodeInvalidArgument)
		return nil, observation, fault.New(ErrorCodeInvalidArgument, "recall owner and session must not be empty")
	}
	query := runtimeingress.QueryText(req.Parts)
	if query == "" {
		observation.Outcome = OutcomeEmpty
		return nil, observation, nil
	}
	readable, known := p.scopes.RecallScope(req.AgentID)
	if !known {
		observation.Outcome = string(ErrorCodeQueryInvalid)
		return nil, observation, fault.New(ErrorCodeQueryInvalid, "recall persona is not registered")
	}
	result, err := p.service.RecallWithSummary(ctx, &RecallRequest{
		OwnerID:   req.PrincipalID,
		AgentIDs:  readable,
		SessionID: req.SessionID,
		Query:     query,
		Before:    p.now().UTC(),
	}, p.model, p.summarizer)
	if err != nil {
		observation.Outcome = string(ErrorCodeUnavailable)
		if code, ok := fault.CodeOf[ErrorCode](err); ok {
			observation.Outcome = string(code)
		}
		return nil, observation, err
	}
	p.annotateTaint(ctx, &result)
	summaryTainted := result.Tainted
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
	anyKeptTainted := false
	for i := range result.Documents {
		if result.Documents[i].Tainted {
			anyKeptTainted = true
			break
		}
	}
	result.Tainted = anyKeptTainted || (result.Summary != "" && summaryTainted)
	observation.Documents = len(result.Documents)
	observation.Outcome = OutcomeOK
	return result.Evidence(), observation, nil
}

func (p *Provider) annotateTaint(ctx context.Context, result *RecallContext) {
	if result == nil || (len(result.Documents) == 0 && result.Summary == "") {
		return
	}
	if p.taintMarks == nil {
		result.Tainted = true
		for i := range result.Documents {
			result.Documents[i].Tainted = true
		}
		return
	}
	seen := map[string]struct{}{}
	sessionIDs := make([]string, 0, len(result.Documents))
	for i := range result.Documents {
		sid := result.Documents[i].SessionID
		if sid == "" {
			continue
		}
		if _, ok := seen[sid]; !ok {
			seen[sid] = struct{}{}
			sessionIDs = append(sessionIDs, sid)
		}
	}
	marks, err := p.taintMarks.SessionMarks(ctx, sessionIDs)
	if err != nil {
		result.Tainted = true
		for i := range result.Documents {
			result.Documents[i].Tainted = true
		}
		return
	}
	anyTainted := false
	for i := range result.Documents {
		firstSeq, marked := marks[result.Documents[i].SessionID]
		if marked && firstSeq <= result.Documents[i].ToSequence {
			result.Documents[i].Tainted = true
			anyTainted = true
		}
	}
	result.Tainted = anyTainted
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
