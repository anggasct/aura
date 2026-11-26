package profile

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/turn"
)

const (
	userAuthor    = "user"
	userEventKind = "adk_event"
)

func (m *Module) AfterTurn(ctx context.Context, req *turn.TurnRequest, events []turn.Event) error {
	if m == nil || m.extractor == nil || req == nil {
		return nil
	}
	if req.PrincipalClass != principal.ClassOwner || !directOwnerOrigin(req.Origin) {
		return nil
	}
	job, ok := profileJob(req.SessionID, events)
	if !ok {
		return nil
	}
	if err := m.extractor.Enqueue(ctx, job); err != nil {
		if fault.HasCode(err, ErrorCodeProfileUnavailable) {
			return nil
		}
		return err
	}
	return nil
}

func directOwnerOrigin(origin turn.Origin) bool {
	channel, _, _ := strings.Cut(string(origin), ":")
	return origin == turn.OriginTerminal || channel == principal.ChannelDiscord || channel == principal.ChannelTelegram || channel == principal.ChannelEmail
}

func profileJob(sessionID string, events []turn.Event) (Job, bool) {
	job := Job{OwnerID: principal.OwnerID, SessionID: sessionID}
	for i := range events {
		event := &events[i]
		if event.Kind != userEventKind || event.Author != userAuthor || event.Sequence == 0 {
			continue
		}
		if text := ownerInputText(event.Payload); text != "" {
			job.Events = append(job.Events, JobEvent{ID: event.ID, Sequence: event.Sequence, Text: text})
		}
	}
	return job, strings.TrimSpace(sessionID) != "" && len(job.Events) > 0
}

func ownerInputText(payload json.RawMessage) string {
	var envelope struct {
		Content *struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"content"`
		Partial bool `json:"partial"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil || envelope.Partial || envelope.Content == nil {
		return ""
	}
	var texts []string
	for _, part := range envelope.Content.Parts {
		text := strings.TrimSpace(part.Text)
		if text == "" || strings.HasPrefix(text, turn.RecallEvidenceStart) || strings.HasPrefix(text, turn.ProfileEvidenceStart) {
			continue
		}
		texts = append(texts, text)
	}
	return strings.Join(texts, "\n")
}
