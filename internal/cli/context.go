package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/anggasct/aura/internal/config"
	contextpkg "github.com/anggasct/aura/internal/context"
	"github.com/anggasct/aura/internal/store"
)

const contextSummaryAuthor = "runtime"

func compressionProducer(cfg *config.Config) (contextpkg.Producer, error) {
	if cfg == nil {
		return contextpkg.Producer{}, errors.New("cli: config must not be nil")
	}
	role := cfg.Models.Routing["compression"]
	if strings.TrimSpace(role) == "" {
		role = "auxiliary"
	}
	route, ok := cfg.ModelRoutes[role]
	if !ok {
		return contextpkg.Producer{}, fmt.Errorf("cli: compression route %q is not configured", role)
	}
	if len(route.Candidates) == 0 {
		return contextpkg.Producer{}, fmt.Errorf("cli: compression route %q names no candidate", role)
	}
	definition, ok := cfg.Models.Definitions[route.Candidates[0]]
	if !ok {
		return contextpkg.Producer{}, fmt.Errorf("cli: compression candidate %q is not defined", route.Candidates[0])
	}
	if strings.TrimSpace(definition.Protocol) == "" || strings.TrimSpace(definition.Model) == "" {
		return contextpkg.Producer{}, fmt.Errorf("cli: compression candidate %q carries no identity", route.Candidates[0])
	}
	return contextpkg.Producer{Provider: definition.Protocol, Model: definition.Model}, nil
}

type contextContentPart struct {
	Text             string `json:"text"`
	FunctionResponse *struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"functionResponse"`
	InlineData *struct {
		MIMEType string `json:"mimeType"`
	} `json:"inlineData"`
	FileData *struct {
		MIMEType string `json:"mimeType"`
	} `json:"fileData"`
}

type contextEventPayload struct {
	Content *struct {
		Parts []contextContentPart `json:"parts"`
	} `json:"content"`
	Text           string `json:"text"`
	Partial        bool   `json:"partial"`
	ToolCallID     string `json:"tool_call_id"`
	EffectIntentID string `json:"effect_intent_id"`
	ApprovalID     string `json:"approval_id"`
}

func mapContextEvent(row *store.RuntimeEvent) contextpkg.Event {
	if row == nil {
		return contextpkg.Event{}
	}
	event := contextpkg.Event{
		ID:           row.ID,
		Sequence:     row.Sequence,
		TurnID:       row.TurnID,
		InvocationID: row.InvocationID,
		Kind:         row.Kind,
	}
	var payload contextEventPayload
	if err := json.Unmarshal(row.Payload, &payload); err != nil {
		return event
	}
	if payload.Partial {
		return event
	}
	var texts []string
	if payload.Content != nil {
		for _, part := range payload.Content.Parts {
			if strings.TrimSpace(part.Text) != "" {
				texts = append(texts, part.Text)
			}
			if part.FunctionResponse != nil {
				event.ToolResult = true
				if event.CorrelationID == "" {
					event.CorrelationID = part.FunctionResponse.ID
				}
			}
			if event.MediaType == "" {
				switch {
				case part.InlineData != nil && strings.TrimSpace(part.InlineData.MIMEType) != "":
					event.MediaType = part.InlineData.MIMEType
				case part.FileData != nil && strings.TrimSpace(part.FileData.MIMEType) != "":
					event.MediaType = part.FileData.MIMEType
				}
			}
		}
	}
	if len(texts) == 0 && strings.TrimSpace(payload.Text) != "" {
		texts = append(texts, payload.Text)
	}
	event.Text = strings.Join(texts, "")
	for _, candidate := range []string{payload.ToolCallID, payload.EffectIntentID, payload.ApprovalID} {
		if strings.TrimSpace(candidate) != "" {
			event.CorrelationID = candidate
			break
		}
	}
	if row.Kind == "tool.completed" {
		event.ToolResult = true
	}
	return event
}

type contextSummaryStore struct {
	sessions store.SessionService
	events   store.EventStore
}

func newContextSummaryStore(sessions store.SessionService, events store.EventStore) *contextSummaryStore {
	return &contextSummaryStore{sessions: sessions, events: events}
}

func (s *contextSummaryStore) ListSummaries(ctx context.Context, sessionID string) ([]contextpkg.SummaryRecord, error) {
	if ctx == nil {
		return nil, errors.New("cli: context must not be nil")
	}
	const page = 512
	var records []contextpkg.SummaryRecord
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
			row := &rows[i]
			after = row.Sequence
			if row.Kind != contextpkg.SummaryKind {
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
			records = append(records, contextpkg.SummaryRecord{
				ID:            row.ID,
				SessionID:     row.SessionID,
				StartSequence: envelope.Source.StartSequence,
				EndSequence:   envelope.Source.EndSequence,
				TurnID:        row.TurnID,
				Payload:       append([]byte(nil), row.Payload...),
				CreatedAt:     row.CreatedAt,
			})
		}
		if len(rows) < page {
			break
		}
	}
	return records, nil
}

func (s *contextSummaryStore) UpsertSummary(ctx context.Context, record *contextpkg.SummaryRecord) error {
	if ctx == nil {
		return errors.New("cli: context must not be nil")
	}
	if record == nil {
		return errors.New("cli: summary record must not be nil")
	}
	if strings.TrimSpace(record.TurnID) == "" {
		return errors.New("cli: summary record carries no anchor turn")
	}
	created := record.CreatedAt.UTC()
	if created.IsZero() {
		created = time.Now().UTC()
	}
	_, _, err := s.events.UpsertEvent(ctx, &store.RuntimeEvent{
		ID:            record.ID,
		SessionID:     record.SessionID,
		TurnID:        record.TurnID,
		Author:        contextSummaryAuthor,
		Kind:          contextpkg.SummaryKind,
		SchemaVersion: contextpkg.SummarySchema,
		Payload:       append([]byte(nil), record.Payload...),
		CreatedAt:     created,
	})
	return err
}

type routerSummarizer struct {
	llm      adkmodel.LLM
	producer contextpkg.Producer
	bound    int
}

func newRouterSummarizer(llm adkmodel.LLM, producer contextpkg.Producer, maxOutputTokens int) *routerSummarizer {
	return &routerSummarizer{llm: llm, producer: producer, bound: contextpkg.OutputByteBound(maxOutputTokens) + 1}
}

func (s *routerSummarizer) Summarize(ctx context.Context, req *contextpkg.SummarizeRequest) (*contextpkg.SummarizeResult, error) {
	if ctx == nil {
		return nil, errors.New("cli: context must not be nil")
	}
	if req == nil {
		return nil, errors.New("cli: summarize request must not be nil")
	}
	if s.llm == nil {
		return nil, errors.New("cli: summarizer has no model")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if req.MaxOutputTokens <= 0 || req.MaxOutputTokens > 1<<20 {
		return nil, errors.New("cli: summarize request carries an invalid output bound")
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
		return nil, contextpkg.Errorf(contextpkg.ErrorCodeSummaryInvalid, "summarizer output exceeds the output bound")
	}
	if strings.TrimSpace(collected.String()) == "" {
		return nil, contextpkg.Errorf(contextpkg.ErrorCodeSummaryUnavailable, "summarizer returned no text")
	}
	return &contextpkg.SummarizeResult{Text: collected.String(), Producer: s.producer}, nil
}
