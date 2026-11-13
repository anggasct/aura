package toolbroker

import (
	"context"
	"encoding/json"

	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/toolcall"
)

func (b *Broker) evaluatePolicy(ctx context.Context, slot *toolcall.Slot, request *approval.ToolRequest) (approval.PolicyDecision, error) {
	scope, ok := durable.TurnScopeFrom(ctx)
	if !ok {
		return b.engine.Evaluate(ctx, request)
	}
	raw, err := scope.Invocation().RunAction(ctx, slot.Action("policy"), b.recordApproval(func(ctx context.Context) ([]byte, error) {
		decision, err := b.engine.Evaluate(ctx, request)
		if err != nil {
			return nil, err
		}
		return json.Marshal(decision)
	}))
	if err != nil {
		return approval.PolicyDecision{}, err
	}
	recorded, err := b.readOutcome(raw)
	if err != nil {
		return approval.PolicyDecision{}, err
	}
	var decision approval.PolicyDecision
	if err := json.Unmarshal(recorded, &decision); err != nil {
		return approval.PolicyDecision{}, err
	}
	return decision, nil
}

func (b *Broker) executeDurable(ctx context.Context, slot *toolcall.Slot, call *journaledTool) (result ToolResult, err error) {
	defer func() {
		if recovered := durable.RecoveredPanic(); recovered != nil && err == nil {
			err = b.classifyExecutionError(ctx, call.canonical, recovered)
		}
	}()
	scope, _ := durable.TurnScopeFrom(ctx)
	raw, err := scope.Invocation().RunAction(ctx, slot.Action("execution"), func(ctx context.Context) ([]byte, error) {
		return b.runJournaledTool(ctx, call)
	})
	if err != nil {
		return ToolResult{}, b.classifyExecutionError(ctx, call.canonical, err)
	}
	recorded, err := b.readOutcome(raw)
	if err != nil {
		return ToolResult{}, err
	}
	var decoded ToolResult
	if err := json.Unmarshal(recorded, &decoded); err != nil {
		return ToolResult{}, unreadableOutcome()
	}
	return decoded, nil
}

func (b *Broker) validateDurable(ctx context.Context, slot *toolcall.Slot, canonical *ToolRequest) error {
	grant := canonical.Approval
	scope, ok := durable.TurnScopeFrom(ctx)
	if !ok || grant == nil {
		return b.engine.ValidateAndConsume(ctx, toApprovalRequest(canonical, b.PolicyVersion()), grant)
	}
	raw, err := scope.Invocation().RunAction(ctx, slot.Action("grant-validation"), b.recordApproval(func(ctx context.Context) ([]byte, error) {
		if err := b.engine.ValidateAndConsume(ctx, toApprovalRequest(canonical, b.PolicyVersion()), grant); err != nil {
			return nil, err
		}
		return []byte(`{}`), nil
	}))
	if err != nil {
		return err
	}
	_, err = b.readOutcome(raw)
	return err
}
