package toolbroker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/toolcall"
	"github.com/anggasct/aura/internal/platform/tools"
)

type authorization struct {
	canonical  *ToolRequest
	definition *tools.Definition
	decision   approval.PolicyDecision
	expiry     time.Time
	standing   bool

	previewLines []string

	standingAllowed bool
}

func (a *authorization) forOperation() bool {
	return a.definition != nil && a.definition.Operation != ""
}

type authorizationRecord struct {
	State         string           `json:"state"`
	RequestDigest string           `json:"request_digest"`
	Failure       *recordedFailure `json:"failure,omitempty"`
}

func (b *Broker) authorize(ctx context.Context, slot *toolcall.Slot, call *authorization) (string, error) {
	scope, ok := durable.TurnScopeFrom(ctx)
	if !ok {
		return b.decideAuthorization(ctx, call)
	}
	raw, err := scope.Invocation().RunAction(ctx, slot.Action("authorization"), func(ctx context.Context) ([]byte, error) {
		return b.recordAuthorization(ctx, call)
	})
	if err != nil {
		return ApprovalRejected, b.unrecordedAuthorization(call.canonical, err)
	}
	return b.readAuthorization(raw, call.canonical)
}

func (b *Broker) decideAuthorization(ctx context.Context, call *authorization) (string, error) {
	return b.decideAuthorizationWith(ctx, ctx, call)
}

func (b *Broker) decideAuthorizationWith(turnCtx, callCtx context.Context, call *authorization) (string, error) {
	if err := contextError(turnCtx, call.canonical.Deadline); err != nil {
		return ApprovalRejected, err
	}
	key := toolKey(call.canonical)
	if call.decision.Outcome == approval.OutcomeRequireApproval && !call.standing {
		if call.forOperation() && !b.sessionReachable(call.canonical.SessionID) {
			return ApprovalMissing, operationNoApprover(call.canonical, call.definition)
		}
		if call.canonical.Tainted && requestClass(call.canonical) == principal.ClassPublic {
			return ApprovalMissing, errorf(ResultUntrustedInputNoApprover, "tool %q needs approval because this turn carries untrusted input, and a public principal's turn never asks the owner", key)
		}
		if call.canonical.Tainted && !b.sessionReachable(call.canonical.SessionID) {
			return ApprovalMissing, errorf(ResultUntrustedInputNoApprover, "tool %q needs approval because this turn carries untrusted input, and nobody can be asked", key)
		}
		if b.decider == nil {
			return ApprovalMissing, errorf(ResultApprovalRequired, "tool %q requires approval", key)
		}
	}
	state, err := b.approvalAnswer(callCtx, call)
	if err != nil {
		return state, err
	}
	if err := contextError(turnCtx, call.expiry); err != nil {
		return ApprovalRejected, err
	}
	grant, err := b.engine.GrantUntil(callCtx, toApprovalRequest(call.canonical, b.PolicyVersion()), call.expiry)
	if err != nil {
		return state, mapApprovalError(err)
	}
	if err := b.engine.ValidateAndConsume(callCtx, toApprovalRequest(call.canonical, b.PolicyVersion()), &grant); err != nil {
		return state, mapApprovalError(err)
	}
	return state, nil
}

func unattendedRefusal(request *ToolRequest, definition *tools.Definition) error {
	key := toolKey(request)
	if definition != nil && definition.Operation != "" {
		return operationNoApprover(request, definition)
	}
	if request.Tainted {
		return errorf(ResultUntrustedInputNoApprover, "tool %q needs approval because this turn carries untrusted input, and an unattended call never asks", key)
	}
	return errorf(ResultApprovalRequired, "tool %q requires approval, and an unattended call never asks", key)
}

func (b *Broker) approvalAnswer(ctx context.Context, call *authorization) (string, error) {
	if call.standing {
		return ApprovalStanding, nil
	}
	if call.decision.Outcome != approval.OutcomeRequireApproval {
		return ApprovalAuto, nil
	}
	key := toolKey(call.canonical)
	prompt, err := b.buildApprovalPrompt(call)
	if err != nil {
		return ApprovalRejected, err
	}
	if prompt.ReasonCode == approval.ReasonUntrustedTrustGrant {
		b.recordTaintedTrustGrant(ctx, "card")
	}
	accepted, err := b.decider(ctx, prompt)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return ApprovalRejected, errorf(ResultDeadlineExceeded, "tool request ended: %v", err)
		}
		return ApprovalRejected, errorf(ResultPolicyDenied, "tool %q approval failed closed: %s", key, redact([]byte(err.Error()), b.secretValues()))
	}
	if !accepted {
		return ApprovalRejected, errorf(ResultApprovalRequired, "tool %q approval was rejected", key)
	}
	return ApprovalApproved, nil
}

func (b *Broker) recordAuthorization(ctx context.Context, call *authorization) ([]byte, error) {
	callCtx, cancel := withCallDeadline(ctx, call.canonical.Deadline)
	defer cancel()
	state, err := b.decideAuthorizationWith(ctx, callCtx, call)
	record := authorizationRecord{State: state, RequestDigest: call.canonical.RequestDigest}
	if err != nil {
		var refusal *fault.Error[ResultClass]
		if ctx.Err() != nil || !errors.As(err, &refusal) || !recordableClass(refusal.Code) {
			return nil, b.redactedAuthorizationError(err)
		}
		record.Failure = &recordedFailure{Class: refusal.Code, Detail: string(redact([]byte(refusal.Detail), b.secretValues()))}
	}
	return json.Marshal(record)
}

func recordableClass(class ResultClass) bool {
	return class.stable() && class != ResultOK
}

func (b *Broker) redactedAuthorizationError(err error) error {
	secrets := b.secretValues()
	var coded *fault.Error[ResultClass]
	if errors.As(err, &coded) && recordableClass(coded.Code) {
		return fault.New(coded.Code, string(redact([]byte(coded.Detail), secrets)))
	}
	return fault.New(ResultPolicyDenied, string(redact([]byte(err.Error()), secrets)))
}

func (b *Broker) unrecordedAuthorization(canonical *ToolRequest, err error) error {
	class, ok := fault.CodeOf[ResultClass](err)
	if !ok || !recordableClass(class) {
		class = ResultPolicyDenied
	}
	return errorf(class, "tool %q authorization failed closed: %s", toolKey(canonical), redact([]byte(err.Error()), b.secretValues()))
}

func (b *Broker) readAuthorization(raw []byte, canonical *ToolRequest) (string, error) {
	record, ok := decodeAuthorization(raw)
	if !ok || record.RequestDigest != canonical.RequestDigest {
		return ApprovalRejected, errorf(ResultPolicyDenied, "tool %q has a recorded authorization that does not belong to this request", toolKey(canonical))
	}
	if record.Failure == nil {
		return record.State, nil
	}
	return record.State, fault.New(record.Failure.Class, string(redact([]byte(record.Failure.Detail), b.secretValues())))
}

func decodeAuthorization(raw []byte) (authorizationRecord, bool) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var record authorizationRecord
	if err := decoder.Decode(&record); err != nil {
		return authorizationRecord{}, false
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return authorizationRecord{}, false
	}
	if !record.consistent() {
		return authorizationRecord{}, false
	}
	return record, true
}

func (r *authorizationRecord) consistent() bool {
	refused := r.Failure != nil
	if refused && !recordableClass(r.Failure.Class) {
		return false
	}
	switch r.State {
	case ApprovalAuto, ApprovalStanding, ApprovalApproved:
		return true
	case ApprovalRejected, ApprovalMissing:
		return refused
	}
	return false
}
