package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/anggasct/aura/internal/durable"
	gatewaywebhook "github.com/anggasct/aura/internal/gateway/webhook"
	"github.com/anggasct/aura/internal/workflow"
)

const Source = "github"

const maxNormalizedPayloadBytes = 4096

type CorrelationStore interface {
	BindCorrelation(ctx context.Context, correlation *workflow.Correlation) error
	ResolveCorrelation(ctx context.Context, source, eventType, externalID, dedupeKey string) (workflow.Correlation, error)
	DeleteCorrelation(ctx context.Context, source, eventType, externalID, dedupeKey string) error
	Run(ctx context.Context, runID string) (*workflow.RunSummary, error)
}

type NormalizedEvent struct {
	EventType  string
	ExternalID string
	Payload    json.RawMessage
}

type Adapter struct {
	store   CorrelationStore
	runtime durable.Runtime
	logger  *slog.Logger
}

func NewAdapter(store CorrelationStore, runtime durable.Runtime, logger *slog.Logger) (*Adapter, error) {
	if store == nil {
		return nil, errors.New("github adapter requires a correlation store")
	}
	if runtime == nil {
		return nil, errors.New("github adapter requires a durable runtime")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Adapter{store: store, runtime: runtime, logger: logger}, nil
}

func decodeDocument(body []byte) map[string]json.RawMessage {
	var document map[string]json.RawMessage
	if json.Unmarshal(body, &document) != nil {
		return nil
	}
	return document
}

func Normalize(body []byte) (NormalizedEvent, bool, error) {
	document := decodeDocument(body)
	if document == nil {
		return NormalizedEvent{}, false, nil
	}
	repository, ok := document["repository"]
	if !ok {
		return NormalizedEvent{}, false, nil
	}
	suite, hasSuite := document["check_suite"]
	run, hasRun := document["check_run"]
	if !hasSuite && !hasRun {
		return NormalizedEvent{}, false, nil
	}
	action, err := stringField(document, "action", 64)
	if err != nil || action == "" {
		return NormalizedEvent{}, true, gatewaywebhook.Errorf(gatewaywebhook.ErrorCodeInvalidRequest, "github event carries no action")
	}
	repo, err := stringFieldAt(repository, "full_name", 128)
	if err != nil || repo == "" || !strings.Contains(repo, "/") {
		return NormalizedEvent{}, true, gatewaywebhook.Errorf(gatewaywebhook.ErrorCodeInvalidRequest, "github event carries no repository")
	}
	entity := "check_suite"
	entityBody := suite
	if hasRun {
		entity = "check_run"
		entityBody = run
	}
	externalID, err := externalIDFor(repo, entityBody)
	if err != nil {
		return NormalizedEvent{}, true, err
	}
	payload, err := signalPayloadFor(entityBody)
	if err != nil {
		return NormalizedEvent{}, true, err
	}
	return NormalizedEvent{EventType: entity + "." + action, ExternalID: externalID, Payload: payload}, true, nil
}

func externalIDFor(repo string, entityBody json.RawMessage) (string, error) {
	var entity struct {
		PullRequests []struct {
			Number int `json:"number"`
		} `json:"pull_requests"`
		HeadSHA string `json:"head_sha"`
	}
	if err := json.Unmarshal(entityBody, &entity); err != nil {
		return "", gatewaywebhook.Errorf(gatewaywebhook.ErrorCodeInvalidRequest, "github event body is not decodable")
	}
	if len(entity.PullRequests) > 0 && entity.PullRequests[0].Number > 0 {
		return fmt.Sprintf("%s#%d", repo, entity.PullRequests[0].Number), nil
	}
	if entity.HeadSHA == "" || len(entity.HeadSHA) > 64 {
		return "", gatewaywebhook.Errorf(gatewaywebhook.ErrorCodeInvalidRequest, "github event names neither a pull request nor a commit")
	}
	return repo + "@" + entity.HeadSHA, nil
}

func signalPayloadFor(entityBody json.RawMessage) (json.RawMessage, error) {
	var entity struct {
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
		HeadSHA    string `json:"head_sha"`
		HTMLURL    string `json:"html_url"`
	}
	if err := json.Unmarshal(entityBody, &entity); err != nil {
		return nil, gatewaywebhook.Errorf(gatewaywebhook.ErrorCodeInvalidRequest, "github event body is not decodable")
	}
	payload, err := json.Marshal(map[string]string{
		"status":     truncField(entity.Status, 32),
		"conclusion": truncField(entity.Conclusion, 32),
		"head_sha":   truncField(entity.HeadSHA, 64),
		"html_url":   truncField(entity.HTMLURL, 512),
	})
	if err != nil {
		return nil, gatewaywebhook.Errorf(gatewaywebhook.ErrorCodeInvalidRequest, "github signal payload is not encodable")
	}
	if len(payload) > maxNormalizedPayloadBytes {
		return nil, gatewaywebhook.Errorf(gatewaywebhook.ErrorCodeInvalidRequest, "github signal payload exceeds the bound")
	}
	return payload, nil
}

func stringField(document map[string]json.RawMessage, name string, limit int) (string, error) {
	raw, ok := document[name]
	if !ok {
		return "", nil
	}
	return stringFieldAt(raw, "", limit)
}

func stringFieldAt(raw json.RawMessage, name string, limit int) (string, error) {
	if name != "" {
		var document map[string]json.RawMessage
		if err := json.Unmarshal(raw, &document); err != nil {
			return "", err
		}
		raw = document[name]
	}
	if len(raw) == 0 {
		return "", nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", err
	}
	return truncField(value, limit), nil
}

func truncField(value string, limit int) string {
	if len(value) > limit {
		return value[:limit]
	}
	return value
}

func eventDocument(event *gatewaywebhook.AcceptedEvent) []byte {
	if len(event.Envelope.Payload) > 0 {
		return event.Envelope.Payload
	}
	return event.Body
}

func eventDedupeKey(event *gatewaywebhook.AcceptedEvent) string {
	if event.Envelope.EventID != "" {
		return event.Envelope.EventID
	}
	return event.BodyDigest
}

func (a *Adapter) Handle(ctx context.Context, event *gatewaywebhook.AcceptedEvent) (bool, gatewaywebhook.ExecutionRef, error) {
	if event == nil {
		return false, gatewaywebhook.ExecutionRef{}, nil
	}
	document := eventDocument(event)
	if len(document) == 0 {
		return false, gatewaywebhook.ExecutionRef{}, nil
	}
	normalized, isGitHub, err := Normalize(document)
	if err != nil {
		return true, gatewaywebhook.ExecutionRef{}, err
	}
	if !isGitHub {
		return false, gatewaywebhook.ExecutionRef{}, nil
	}
	binding, err := a.store.ResolveCorrelation(ctx, Source, normalized.EventType, normalized.ExternalID, "")
	if err != nil {
		if code, ok := workflow.CodeOf(err); ok && code == workflow.ErrorCodeCorrelationUnmatched {
			a.logger.WarnContext(ctx, "github event matches no workflow",
				"component", "github",
				"event_type", normalized.EventType,
				"external_id", normalized.ExternalID,
				"body_digest", event.BodyDigest,
			)
			return true, gatewaywebhook.ExecutionRef{ExecutionID: "unmatched"}, nil
		}
		return true, gatewaywebhook.ExecutionRef{}, err
	}
	delivery := &workflow.Correlation{
		Source: binding.Source, EventType: binding.EventType, ExternalID: binding.ExternalID,
		RunID: binding.RunID, SignalName: binding.SignalName, DedupeKey: eventDedupeKey(event),
	}
	if err := a.store.BindCorrelation(ctx, delivery); err != nil {
		if code, ok := workflow.CodeOf(err); ok && code == workflow.ErrorCodeCorrelationConflict {
			return true, gatewaywebhook.ExecutionRef{ExecutionID: binding.RunID}, nil
		}
		return true, gatewaywebhook.ExecutionRef{}, err
	}
	summary, err := a.store.Run(ctx, binding.RunID)
	if err != nil {
		_ = a.store.DeleteCorrelation(ctx, delivery.Source, delivery.EventType, delivery.ExternalID, delivery.DedupeKey)
		return true, gatewaywebhook.ExecutionRef{}, err
	}
	if err := a.runtime.Signal(ctx, durable.RunRef{Key: summary.DurableKey}, binding.SignalName, normalized.Payload); err != nil {
		_ = a.store.DeleteCorrelation(ctx, delivery.Source, delivery.EventType, delivery.ExternalID, delivery.DedupeKey)
		return true, gatewaywebhook.ExecutionRef{}, fmt.Errorf("signal run %s: %w", binding.RunID, err)
	}
	return true, gatewaywebhook.ExecutionRef{ExecutionID: binding.RunID}, nil
}
