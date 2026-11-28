package compaction

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/turn"
)

const (
	summaryAuthor       = "runtime"
	maxSummaryCacheLoad = 512
)

type EventLister interface {
	ListEvents(ctx context.Context, sessionID string, afterSequence uint64, limit int) ([]turn.Event, error)
}

type EventKindLister interface {
	LatestByKind(ctx context.Context, sessionID, kind string, limit int) ([]turn.Event, error)
}

type EventUpserter interface {
	UpsertEvent(ctx context.Context, event *turn.Event) (uint64, bool, error)
}

type EventSummaryStore struct {
	sessions EventLister
	byKind   EventKindLister
	events   EventUpserter
}

func NewEventSummaryStore(sessions EventLister, events EventUpserter) (*EventSummaryStore, error) {
	if sessions == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "event lister must not be nil")
	}
	if events == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "event upserter must not be nil")
	}
	byKind, _ := sessions.(EventKindLister)
	return &EventSummaryStore{sessions: sessions, byKind: byKind, events: events}, nil
}

func NewIndexedEventSummaryStore(byKind EventKindLister, events EventUpserter) (*EventSummaryStore, error) {
	if byKind == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "event kind lister must not be nil")
	}
	if events == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "event upserter must not be nil")
	}
	return &EventSummaryStore{byKind: byKind, events: events}, nil
}

func (s *EventSummaryStore) ListSummaries(ctx context.Context, sessionID string) ([]SummaryRecord, error) {
	if s == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "summary store must not be nil")
	}
	if ctx == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "context must not be nil")
	}
	if s.byKind != nil {
		rows, err := s.byKind.LatestByKind(ctx, sessionID, SummaryKind, maxSummaryCacheLoad)
		if err != nil {
			return nil, err
		}
		return summaryRecordsFromRows(rows), nil
	}
	const page = 512
	var records []SummaryRecord
	var after uint64
	for {
		rows, err := s.sessions.ListEvents(ctx, sessionID, after, page)
		if err != nil {
			return nil, err
		}
		if len(rows) == 0 {
			break
		}
		for i := range rows {
			after = rows[i].Sequence
		}
		records = append(records, summaryRecordsFromRows(rows)...)
		if len(rows) < page {
			break
		}
	}
	return records, nil
}

func summaryRecordsFromRows(rows []turn.Event) []SummaryRecord {
	var records []SummaryRecord
	for i := range rows {
		row := &rows[i]
		if row.Kind != SummaryKind {
			continue
		}
		var envelope struct {
			Source struct {
				StartSequence uint64 `json:"start_sequence"`
				EndSequence   uint64 `json:"end_sequence"`
			} `json:"source"`
		}
		if err := json.Unmarshal(row.Payload, &envelope); err != nil {
			continue
		}
		records = append(records, SummaryRecord{
			ID:            row.ID,
			SessionID:     row.SessionID,
			StartSequence: envelope.Source.StartSequence,
			EndSequence:   envelope.Source.EndSequence,
			TurnID:        row.TurnID,
			Payload:       append([]byte(nil), row.Payload...),
			CreatedAt:     row.CreatedAt,
		})
	}
	return records
}

func (s *EventSummaryStore) UpsertSummary(ctx context.Context, record *SummaryRecord) error {
	if s == nil {
		return fault.New(ErrorCodeInvalidArgument, "summary store must not be nil")
	}
	if ctx == nil {
		return fault.New(ErrorCodeInvalidArgument, "context must not be nil")
	}
	if record == nil {
		return fault.New(ErrorCodeInvalidArgument, "summary record must not be nil")
	}
	if strings.TrimSpace(record.TurnID) == "" {
		return fault.New(ErrorCodeInvalidArgument, "summary record carries no anchor turn")
	}
	created := record.CreatedAt.UTC()
	if created.IsZero() {
		created = time.Now().UTC()
	}
	_, _, err := s.events.UpsertEvent(ctx, &turn.Event{
		ID:            record.ID,
		SessionID:     record.SessionID,
		TurnID:        record.TurnID,
		Author:        summaryAuthor,
		Kind:          SummaryKind,
		SchemaVersion: SummarySchema,
		Payload:       append([]byte(nil), record.Payload...),
		CreatedAt:     created,
	})
	return err
}
