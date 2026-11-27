package toolbroker

import (
	"context"
	"encoding/json"
	"slices"

	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/toolcall"
	"github.com/anggasct/aura/internal/platform/tools"
	"github.com/anggasct/aura/internal/platform/turn"
)

const operationProvider = "operation"

type Preview struct {
	Lines  []string `json:"lines"`
	Digest string   `json:"digest"`
	Empty  bool     `json:"empty"`
}

type Previewer func(ctx context.Context, request *ToolRequest) (Preview, error)

type operationCall struct {
	canonical  *ToolRequest
	definition *tools.Definition
	previewer  Previewer
}

type operationAdmission struct {
	lines    []string
	result   ToolResult
	finished bool
}

func providerFor(definition *tools.Definition) string {
	if definition == nil || definition.Operation != "" {
		return operationProvider
	}
	return effectProvider
}

func nilOperationArguments() error {
	return errorf(ResultInvalidArgument, "operation request, definition and call must not be nil")
}

func checkOperationRegistration(definition *tools.Definition, rule *approval.Rule, previewer Previewer) error {
	if definition == nil || rule == nil {
		return nilOperationArguments()
	}
	if err := definition.ValidateOperation(); err != nil {
		return err
	}
	if definition.Operation == "" {
		if previewer != nil {
			return errorf(ResultInvalidArgument, "tool %q has a previewer but no operation", definition.Name)
		}
		return nil
	}
	if rule.RequiresApproval != definition.RequiresApproval {
		return errorf(ResultInvalidArgument, "rule for tool %q does not require approval as its risk requires", definition.Name)
	}
	switch {
	case definition.Risk == tools.RiskRead && previewer != nil:
		return errorf(ResultInvalidArgument, "read tool %q must not have a previewer", definition.Name)
	case needsPreview(definition) && previewer == nil:
		return errorf(ResultInvalidArgument, "tool %q needs a previewer for its risk", definition.Name)
	}
	return nil
}

func needsPreview(definition *tools.Definition) bool {
	return definition == nil || definition.Risk == tools.RiskTrustGrant || definition.Risk == tools.RiskDestructive
}

func operationOwnerOnly(request *ToolRequest, class principal.Class, definition *tools.Definition) error {
	if request == nil || definition == nil {
		return nilOperationArguments()
	}
	if _, known := tools.FlagsFor(definition.Risk, definition.FreshApproval); !known {
		return errorf(ResultPolicyDenied, "tool %q has no known risk", toolKey(request))
	}
	if class != principal.ClassOwner || request.Origin == "" || request.Origin == turn.OriginChild || request.Origin == turn.OriginWorkflow {
		return errorf(ResultToolTierDenied, "tool %q is available only on a turn the owner started", toolKey(request))
	}
	return nil
}

func operationRefusal(request *ToolRequest, definition *tools.Definition) error {
	if request == nil || definition == nil {
		return nilOperationArguments()
	}
	key := toolKey(request)
	if definition.Risk == tools.RiskTrustGrant && request.Tainted {
		return errorf(ResultUntrustedInputTrustGrant, "tool %q grants trust and this turn carries untrusted input", key)
	}
	if definition.Privileged() && unattendedRequest(request, definition) {
		return errorf(ResultOwnerTurnRequired, "tool %q needs an answer from the owner, and an unattended call never asks", key)
	}
	if request.Approval != nil {
		if definition.FreshApproval {
			return errorf(ResultFreshApprovalRequired, "tool %q needs a new answer from the owner for each call", key)
		}
		return errorf(ResultToolTierDenied, "tool %q does not accept an attached approval", key)
	}
	if changeNeedsCard(request, definition) && unattendedRequest(request, definition) {
		return operationNoApprover(request, definition)
	}
	return nil
}

func changeNeedsCard(request *ToolRequest, definition *tools.Definition) bool {
	return definition.Risk == tools.RiskChange && !definition.OwnerNotice && (!request.ContentTaintTracked || request.Tainted)
}

func unattendedRequest(request *ToolRequest, definition *tools.Definition) bool {
	if request == nil || definition == nil || request.Unattended {
		return true
	}
	return definition.Operation != "" && (request.Origin == turn.OriginSchedule || request.Origin == turn.OriginHTTP)
}

func operationPolicyOutcome(decision *approval.PolicyDecision, request *ToolRequest, definition *tools.Definition) error {
	if decision == nil || request == nil || definition == nil {
		return nilOperationArguments()
	}
	switch decision.Outcome {
	case approval.OutcomeAllow, approval.OutcomeRequireApproval:
		return nil
	case approval.OutcomeDeny:
		return errorf(ResultPolicyDenied, "tool %q is denied by policy", toolKey(request))
	default:
		return errorf(ResultPolicyDenied, "tool %q has a policy outcome the broker does not know", toolKey(request))
	}
}

func operationApproval(decision *approval.PolicyDecision, request *ToolRequest, definition *tools.Definition) error {
	if err := operationPolicyOutcome(decision, request, definition); err != nil {
		return err
	}
	if decision.Outcome == approval.OutcomeAllow && (definition.Privileged() || changeNeedsCard(request, definition)) {
		decision.Outcome = approval.OutcomeRequireApproval
		decision.ReasonCode = string(approval.ErrorCodeApprovalRequired)
		if !definition.Privileged() {
			decision.ReasonCode = approval.ReasonContentUnverified
		}
	}
	return nil
}

func operationNoApprover(request *ToolRequest, definition *tools.Definition) error {
	if request == nil || definition == nil {
		return nilOperationArguments()
	}
	key := toolKey(request)
	if request.Tainted && !definition.Privileged() {
		return errorf(ResultUntrustedInputNoApprover, "tool %q needs approval because this turn carries untrusted input, and nobody can be asked", key)
	}
	return errorf(ResultOwnerTurnRequired, "tool %q needs an answer from the owner, and this turn has nobody to ask", key)
}

func (b *Broker) admitOperation(ctx context.Context, slot *toolcall.Slot, call *operationCall) (operationAdmission, error) {
	if call == nil || call.canonical == nil || call.definition == nil {
		return operationAdmission{}, nilOperationArguments()
	}
	b.mu.RLock()
	previewer := b.previewers[call.definition.Key()]
	b.mu.RUnlock()
	if previewer == nil {
		if needsPreview(call.definition) {
			return operationAdmission{}, errorf(ResultPolicyDenied, "tool %q has no previewer", toolKey(call.canonical))
		}
		return operationAdmission{}, nil
	}
	call.previewer = previewer
	preview, err := b.runPreview(ctx, slot, call)
	if err != nil {
		return operationAdmission{}, err
	}
	if preview.Empty {
		output, err := json.Marshal(struct {
			Preview Preview `json:"preview"`
		}{preview})
		if err != nil {
			return operationAdmission{}, errorf(ResultExecutionFailed, "encode preview result: %v", err)
		}
		return operationAdmission{result: ToolResult{Class: ResultOK, Output: output}, finished: true}, nil
	}
	call.canonical.PreviewDigest = preview.Digest
	if err := b.digestRequest(call.canonical); err != nil {
		return operationAdmission{}, err
	}
	return operationAdmission{lines: preview.Lines}, nil
}

func (b *Broker) runPreview(ctx context.Context, slot *toolcall.Slot, call *operationCall) (Preview, error) {
	if call == nil || call.canonical == nil || call.definition == nil || call.previewer == nil {
		return Preview{}, nilOperationArguments()
	}
	scope, scoped := durable.TurnScopeFrom(ctx)
	if !scoped {
		return b.previewAction(ctx, call)
	}
	raw, err := scope.Invocation().RunAction(ctx, slot.Action("preview"), b.recordApproval(func(ctx context.Context) ([]byte, error) {
		preview, err := b.previewAction(ctx, call)
		if err != nil {
			return nil, err
		}
		return json.Marshal(preview)
	}))
	if err != nil {
		return Preview{}, b.classifyExecutionError(ctx, call.canonical, err)
	}
	recorded, err := b.readOutcome(raw)
	if err != nil {
		return Preview{}, err
	}
	var preview Preview
	if err := json.Unmarshal(recorded, &preview); err != nil || !preview.wellFormed() {
		return Preview{}, unreadableOutcome()
	}
	return preview, nil
}

func (b *Broker) previewAction(ctx context.Context, call *operationCall) (Preview, error) {
	if call == nil || call.canonical == nil || call.definition == nil || call.previewer == nil {
		return Preview{}, nilOperationArguments()
	}
	if err := contextError(ctx, call.canonical.Deadline); err != nil {
		return Preview{}, err
	}
	callCtx, cancel := withCallDeadline(ctx, call.canonical.Deadline)
	defer cancel()
	preview, err := call.previewer(callCtx, previewRequest(call.canonical))
	if err != nil {
		return Preview{}, b.classifyExecutionError(ctx, call.canonical, err)
	}
	if !preview.wellFormed() {
		return Preview{}, errorf(ResultExecutionFailed, "tool %q returned a preview without lines or without a digest", toolKey(call.canonical))
	}
	if _, err := b.cardText(call.definition, call.canonical.Arguments, preview.Lines); err != nil {
		return Preview{}, err
	}
	return preview, nil
}

func previewRequest(canonical *ToolRequest) *ToolRequest {
	copied := *canonical
	copied.Arguments = slices.Clone(canonical.Arguments)
	copied.Capabilities = slices.Clone(canonical.Capabilities)
	copied.Knowledge = slices.Clone(canonical.Knowledge)
	copied.Approval = nil
	return &copied
}

func (p *Preview) wellFormed() bool {
	return p.Empty || (len(p.Lines) > 0 && p.Digest != "")
}
