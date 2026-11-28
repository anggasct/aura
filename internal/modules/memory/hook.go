package memory

import (
	"context"
	"log/slog"
	"time"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/turn"
)

func runAfterTurn(ctx context.Context, service *Service, summaryModel *SummaryModel, summarizer Summarizer, logger *slog.Logger, req *turn.TurnRequest, events []turn.Event) error {
	if service == nil {
		return fault.New(ErrorCodeInvalidArgument, "memory service must not be nil")
	}
	if req == nil {
		return fault.New(ErrorCodeInvalidArgument, "turn request must not be nil")
	}
	converted := make([]*Event, 0, len(events))
	for i := range events {
		converted = append(converted, &Event{
			ID:        events[i].ID,
			SessionID: events[i].SessionID,
			Sequence:  events[i].Sequence,
			TurnID:    events[i].TurnID,
			Author:    events[i].Author,
			Kind:      events[i].Kind,
			Payload:   events[i].Payload,
			CreatedAt: events[i].CreatedAt,
		})
	}
	result, err := projectTurn(ctx, service, req, converted)
	if result.Dropped > 0 && logger != nil {
		logger.DebugContext(ctx, "memory projection dropped secrets", "dropped", result.Dropped, "turn_id", req.TurnID)
	}
	if _, purgeErr := service.PurgeExpired(ctx, time.Now().UTC(), 100); purgeErr != nil && logger != nil {
		logger.WarnContext(ctx, "memory expiry failed", "error", purgeErr, "turn_id", req.TurnID)
	}
	if summaryModel != nil && summarizer != nil {
		if _, _, summaryErr := service.SummarizeClosedRange(ctx, req.PrincipalID, req.SessionID, summaryModel, summarizer); summaryErr != nil && logger != nil {
			logger.WarnContext(ctx, "memory range summary failed", "error", summaryErr, "turn_id", req.TurnID)
		}
	}
	return err
}

func projectTurn(ctx context.Context, service *Service, req *turn.TurnRequest, events []*Event) (BatchResult, error) {
	switch {
	case req.Origin != turn.OriginSchedule:
		return service.ProjectBatch(ctx, req.PrincipalID, events)
	case req.WebOnly || req.JobTaint != "":
		return BatchResult{Skipped: len(events)}, nil
	}
	return service.ProjectRunOutput(ctx, req.PrincipalID, events)
}

func (m *Module) AfterTurn(ctx context.Context, req *turn.TurnRequest, events []turn.Event) error {
	if m == nil {
		return fault.New(ErrorCodeInvalidArgument, "memory module must not be nil")
	}
	return runAfterTurn(ctx, m.service, m.summaryModel, m.summarizer, m.logger, req, events)
}

func NewTurnHook(service *Service, summaryModel *SummaryModel, summarizer Summarizer, logger *slog.Logger) turn.TurnHook {
	return &turnHook{
		service:      service,
		summaryModel: summaryModel,
		summarizer:   summarizer,
		logger:       logger,
	}
}

type turnHook struct {
	service      *Service
	summaryModel *SummaryModel
	summarizer   Summarizer
	logger       *slog.Logger
}

func (h *turnHook) AfterTurn(ctx context.Context, req *turn.TurnRequest, events []turn.Event) error {
	return runAfterTurn(ctx, h.service, h.summaryModel, h.summarizer, h.logger, req, events)
}
