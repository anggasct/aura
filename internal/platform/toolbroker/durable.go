package toolbroker

import (
	"context"
	"encoding/json"

	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/durable"
)

func (b *Broker) evaluatePolicy(ctx context.Context, request *approval.ToolRequest) (approval.PolicyDecision, error) {
	scope, ok := durable.TurnScopeFrom(ctx)
	if !ok {
		return b.engine.Evaluate(ctx, request)
	}
	raw, err := scope.Invocation().RunAction(ctx, scope.NextOp(), b.recordApproval(func(ctx context.Context) ([]byte, error) {
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

func (b *Broker) executeDurable(ctx context.Context, scope *durable.TurnScope, canonical *ToolRequest, constraints approval.Constraints, adapter Adapter, effectful bool) (result ToolResult, err error) {
	defer func() {
		if recovered := durable.RecoveredPanic(); recovered != nil && err == nil {
			err = b.classifyExecutionError(ctx, canonical, recovered)
		}
	}()
	call := &journaledTool{canonical: canonical, constraints: constraints, adapter: adapter, effectful: effectful}
	raw, err := scope.Invocation().RunAction(ctx, scope.NextOp(), func(ctx context.Context) ([]byte, error) {
		return b.runJournaledTool(ctx, call)
	})
	if err != nil {
		return ToolResult{}, b.classifyExecutionError(ctx, canonical, err)
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

func (b *Broker) validateDurable(ctx context.Context, scope *durable.TurnScope, canonical *ToolRequest, grant *approval.ApprovalGrant) error {
	if scope == nil || grant == nil {
		return b.engine.ValidateAndConsume(ctx, toApprovalRequest(canonical, b.PolicyVersion()), grant)
	}
	raw, err := scope.Invocation().RunAction(ctx, scope.NextOp(), b.recordApproval(func(ctx context.Context) ([]byte, error) {
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
