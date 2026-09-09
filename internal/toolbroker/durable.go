package toolbroker

import (
	"context"
	"encoding/json"
	"time"

	"github.com/anggasct/aura/internal/approval"
	"github.com/anggasct/aura/internal/durable"
	"github.com/anggasct/aura/internal/store"
)

type ApprovalDecision struct {
	Approved bool   `json:"approved"`
	Note     string `json:"note,omitempty"`
}

type ApprovalEvent struct {
	ID          string          `json:"id"`
	TurnID      string          `json:"turn_id"`
	SessionID   string          `json:"session_id"`
	PrincipalID string          `json:"principal_id"`
	ToolName    string          `json:"tool_name"`
	ToolVersion string          `json:"tool_version"`
	Arguments   json.RawMessage `json:"arguments"`
	ApprovalID  string          `json:"approval_id"`
	ExpiresAt   time.Time       `json:"expires_at"`
}

type ApprovalEventSink interface {
	EmitApprovalRequired(ctx context.Context, event *ApprovalEvent) error
}

type approvalSinkKey struct{}

func WithApprovalSink(ctx context.Context, sink ApprovalEventSink) context.Context {
	if sink == nil {
		return ctx
	}
	return context.WithValue(ctx, approvalSinkKey{}, sink)
}

func ApprovalSinkFrom(ctx context.Context) (ApprovalEventSink, bool) {
	sink, ok := ctx.Value(approvalSinkKey{}).(ApprovalEventSink)
	if !ok || sink == nil {
		return nil, false
	}
	return sink, true
}

func (b *Broker) evaluatePolicy(ctx context.Context, request *approval.ToolRequest) (approval.PolicyDecision, string, error) {
	scope, ok := durable.TurnScopeFrom(ctx)
	if !ok {
		decision, err := b.engine.Evaluate(ctx, request)
		return decision, "", err
	}
	key := scope.NextOp()
	raw, err := scope.Invocation().RunAction(ctx, key, func(ctx context.Context) ([]byte, error) {
		decision, err := b.engine.Evaluate(ctx, request)
		if err != nil {
			return nil, err
		}
		return json.Marshal(decision)
	})
	if err != nil {
		return approval.PolicyDecision{}, "", err
	}
	var decision approval.PolicyDecision
	if err := json.Unmarshal(raw, &decision); err != nil {
		return approval.PolicyDecision{}, "", err
	}
	return decision, key, nil
}

func (b *Broker) executeDurable(ctx context.Context, scope *durable.TurnScope, canonical *ToolRequest, constraints approval.Constraints, adapter Adapter, effectful bool) (result ToolResult, err error) {
	defer func() {
		if recovered := durable.RecoveredPanic(); recovered != nil && err == nil {
			err = recovered
		}
	}()
	raw, err := scope.Invocation().RunAction(ctx, scope.NextOp(), func(ctx context.Context) ([]byte, error) {
		var result ToolResult
		var err error
		if effectful {
			result, err = b.executeEffect(ctx, canonical, constraints, adapter)
		} else {
			result, err = adapter(ctx, canonical, constraints)
		}
		if err != nil {
			return nil, err
		}
		return json.Marshal(result)
	})
	if err != nil {
		return ToolResult{}, err
	}
	var decoded ToolResult
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return ToolResult{}, err
	}
	return decoded, nil
}

func (b *Broker) parkForApproval(ctx context.Context, scope *durable.TurnScope, evalKey string, canonical *ToolRequest, approvalExpiry time.Time) (grant *approval.ApprovalGrant, err error) {
	defer func() {
		if recovered := durable.RecoveredPanic(); recovered != nil && err == nil {
			err = recovered
		}
	}()
	signal := "approval-" + evalKey
	addr := durable.FormatApprovalAddr(canonical.TurnID, signal)
	sink, ok := ApprovalSinkFrom(ctx)
	if !ok {
		return nil, errorf(ResultCapabilityUnavailable, "durable approval requires an event sink")
	}
	prompt := b.buildApprovalPrompt(canonical, approval.PolicyDecision{Outcome: approval.OutcomeRequireApproval}, approvalExpiry)
	event := ApprovalEvent{
		ID:          "approval-" + canonical.TurnID + "-" + signal,
		TurnID:      canonical.TurnID,
		SessionID:   canonical.SessionID,
		PrincipalID: canonical.PrincipalID,
		ToolName:    canonical.ToolName,
		ToolVersion: canonical.ToolVersion,
		Arguments:   json.RawMessage(prompt.Arguments),
		ApprovalID:  addr,
		ExpiresAt:   approvalExpiry,
	}
	if err := sink.EmitApprovalRequired(ctx, &event); err != nil {
		return nil, err
	}
	wait := time.Until(approvalExpiry)
	if wait <= 0 {
		return nil, errorf(ResultPolicyDenied, "tool %q approval window elapsed", canonical.ToolName)
	}
	payload, timedOut, ok := scope.Invocation().Wait(ctx, signal, wait)
	if !ok {
		if ctx.Err() != nil {
			return nil, errorf(ResultDeadlineExceeded, "tool request ended: %v", ctx.Err())
		}
		return nil, errorf(ResultPolicyDenied, "tool %q approval wait failed", canonical.ToolName)
	}
	if timedOut {
		return nil, errorf(ResultPolicyDenied, "tool %q approval window elapsed", canonical.ToolName)
	}
	var decision ApprovalDecision
	if err := json.Unmarshal(payload, &decision); err != nil || !decision.Approved {
		return nil, errorf(ResultPolicyDenied, "tool %q approval was rejected", canonical.ToolName)
	}
	issued, err := b.Grant(ctx, canonical, time.Until(approvalExpiry))
	if err != nil {
		return nil, err
	}
	return &issued, nil
}

type ApprovalEventStore interface {
	UpsertEvent(ctx context.Context, e *store.RuntimeEvent) (uint64, bool, error)
}

type approvalEventSink struct {
	events  ApprovalEventStore
	publish func(*store.RuntimeEvent)
}

func NewApprovalEventSink(events ApprovalEventStore, publish func(*store.RuntimeEvent)) ApprovalEventSink {
	return &approvalEventSink{events: events, publish: publish}
}

func (s *approvalEventSink) EmitApprovalRequired(ctx context.Context, event *ApprovalEvent) error {
	payload, err := json.Marshal(struct {
		ApprovalID string          `json:"approval_id"`
		ToolName   string          `json:"tool_name"`
		Version    string          `json:"tool_version"`
		Arguments  json.RawMessage `json:"arguments"`
		ExpiresAt  string          `json:"expires_at"`
	}{
		ApprovalID: event.ApprovalID,
		ToolName:   event.ToolName,
		Version:    event.ToolVersion,
		Arguments:  event.Arguments,
		ExpiresAt:  event.ExpiresAt.UTC().Format(time.RFC3339Nano),
	})
	if err != nil {
		return err
	}
	full := &store.RuntimeEvent{
		ID:            event.ID,
		SessionID:     event.SessionID,
		TurnID:        event.TurnID,
		Author:        event.PrincipalID,
		Kind:          "approval.required",
		SchemaVersion: 1,
		Payload:       payload,
		CreatedAt:     time.Now().UTC(),
	}
	sequence, _, err := s.events.UpsertEvent(ctx, full)
	if err != nil {
		return err
	}
	full.Sequence = sequence
	s.publish(full)
	return nil
}
