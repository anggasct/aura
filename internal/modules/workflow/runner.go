package workflow

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"time"

	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/tools"
	"github.com/anggasct/aura/internal/platform/turn"
)

type StepBroker interface {
	Definition(name, version string) (tools.Definition, bool)
	StandingApprovalAllowed(ctx context.Context, request *toolbroker.ToolRequest) (bool, error)
	ExactCallText(name string, args json.RawMessage) (string, error)
	GrantUntil(ctx context.Context, request *toolbroker.ToolRequest, until time.Time) (approval.ApprovalGrant, error)
	Execute(ctx context.Context, request *toolbroker.ToolRequest) (toolbroker.ToolResult, error)
}

type RunSessionReader interface {
	Get(ctx context.Context, id string) (turn.Session, error)
}

type StepRequester struct {
	Principal string
	Class     principal.Class
	Persona   string
}

type OwnerRouteHolder interface {
	Hold(sessionID string, requester *StepRequester) func()
}

type ToolStepRunnerDeps struct {
	Broker   StepBroker
	Sessions RunSessionReader
	Registry *auraagent.Registry
	Runs     *Store
	Route    OwnerRouteHolder
	Logger   *slog.Logger
	Now      func() time.Time
}

type ToolStepRunner struct {
	broker   StepBroker
	sessions RunSessionReader
	registry *auraagent.Registry
	runs     *Store
	route    OwnerRouteHolder
	logger   *slog.Logger
	now      func() time.Time
}

func NewToolStepRunner(deps *ToolStepRunnerDeps) (*ToolStepRunner, error) {
	if deps == nil || deps.Broker == nil || deps.Sessions == nil || deps.Registry == nil || deps.Runs == nil {
		return nil, codedError(ErrorCodeInvalidArgument, "workflow tool runner needs a broker, sessions, a persona registry and a workflow store")
	}
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	return &ToolStepRunner{
		broker:   deps.Broker,
		sessions: deps.Sessions,
		registry: deps.Registry,
		runs:     deps.Runs,
		route:    deps.Route,
		logger:   logger,
		now:      now,
	}, nil
}

func (r *ToolStepRunner) Broker() StepBroker {
	if r == nil {
		return nil
	}
	return r.broker
}

type workflowToolIDs struct {
	sessionID      string
	turnID         string
	requestID      string
	idempotencyKey string
}

func workflowToolIDsFor(call *StepCall) workflowToolIDs {
	sum := sha256.Sum256([]byte(call.RunID + "/" + call.StepID))
	digest := hex.EncodeToString(sum[:])[:24]
	return workflowToolIDs{
		sessionID:      RunSessionID(call.RunID),
		turnID:         "turn-" + digest,
		requestID:      "req-" + digest,
		idempotencyKey: "wf/" + call.RunID + "/" + call.StepID,
	}
}

func workflowTrustLabel(kind ApprovalKind) (approval.TrustLabel, error) {
	switch kind {
	case ApprovalInstalled:
		return approval.TrustTrustedConfiguration, nil
	case ApprovalProposal, ApprovalRoutine:
		return approval.TrustDerivedUntrusted, nil
	default:
		return "", codedError(ErrorCodeApprovalMissing, "workflow approval has no known kind")
	}
}

func (r *ToolStepRunner) checkRunSession(ctx context.Context, sessionID string, origin *RunOrigin) error {
	sess, err := r.sessions.Get(ctx, sessionID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return codedError(ErrorCodeSessionInvalid, "run session "+sessionID+" does not exist")
	case err != nil:
		return codedError(ErrorCodeStepFailed, "read run session: "+err.Error())
	case sess.OwnerID != origin.PrincipalID || sess.AgentID != origin.AgentID:
		return codedError(ErrorCodeSessionInvalid, "run session "+sessionID+" does not belong to the run's origin")
	}
	return nil
}

func checkStepApproval(call *StepCall) (approval.TrustLabel, error) {
	trust, err := workflowTrustLabel(call.Approval.Kind)
	if err != nil {
		return "", err
	}
	record := &call.Approval
	switch {
	case record.Digest == "" || record.DefinitionID == "" || record.DefinitionVersion < 1:
		return "", codedError(ErrorCodeApprovalMissing, "step call carries an incomplete approval record")
	case record.Digest != call.DefinitionDigest:
		return "", codedError(ErrorCodeApprovalMissing, "step call approval digest does not match the run's definition digest")
	case record.DefinitionID != call.DefinitionID || record.DefinitionVersion != call.DefinitionVersion:
		return "", codedError(ErrorCodeApprovalMissing, "step call approval belongs to another definition")
	}
	return trust, nil
}

func workflowToolError(toolID string, err error) error {
	code, ok := fault.CodeOf[toolbroker.ResultClass](err)
	if !ok {
		return err
	}
	if code == toolbroker.ResultCapabilityUnavailable {
		return codedError(ErrorCodeCapabilityUnavailable, "tool "+toolID+" needs a capability this host does not provide")
	}
	if code == toolbroker.ResultApprovalRequired || code == toolbroker.ResultUntrustedInputNoApprover {
		return codedError(ErrorCodeApprovalRejected, err.Error())
	}
	return err
}

func (r *ToolStepRunner) refused(ctx context.Context, call *StepCall, toolID string, err error) error {
	var code string
	if coded, ok := fault.CodeOf[ErrorCode](err); ok {
		code = string(coded)
	} else if coded, ok := fault.CodeOf[toolbroker.ResultClass](err); ok {
		code = string(coded)
	}
	r.logger.WarnContext(ctx, "workflow tool step refused before the broker", "component", "workflow",
		"run_id", call.RunID, "step_id", call.StepID, "tool", toolID, "code", code, "error", err.Error())
	return err
}

func (r *ToolStepRunner) ownsProposalRun(ctx context.Context, call *StepCall) error {
	proposal, err := r.runs.Proposal(ctx, call.Approval.ProposalID, r.now())
	if err != nil {
		return codedError(ErrorCodeApprovalSpent, "the proposal's claimed run could not be read: "+err.Error())
	}
	if call.RunID == "" || proposal.RunID != call.RunID {
		return codedError(ErrorCodeApprovalSpent, "the tainted proposal was approved for another run")
	}
	return nil
}

func (r *ToolStepRunner) attaches(ctx context.Context, call *StepCall, request *toolbroker.ToolRequest) (bool, error) {
	class := principal.ParseClass(call.Origin.PrincipalClass)
	if class != principal.ClassOwner || call.Origin.PrincipalID != principal.OwnerID {
		return r.broker.StandingApprovalAllowed(ctx, request)
	}
	if !request.Tainted {
		return true, nil
	}
	if call.Approval.Kind == ApprovalProposal && call.Approval.Tainted {
		return true, r.ownsProposalRun(ctx, call)
	}
	return false, nil
}

func (r *ToolStepRunner) grant(ctx context.Context, call *StepCall, request *toolbroker.ToolRequest) error {
	attach, err := r.attaches(ctx, call, request)
	if err != nil || !attach {
		return err
	}
	if call.Approval.Kind == ApprovalProposal || call.Approval.Kind == ApprovalRoutine {
		if _, err := r.broker.ExactCallText(request.ToolName, request.Arguments); err != nil {
			return codedError(ErrorCodePlanUnshowable, err.Error())
		}
	}
	grant, err := r.broker.GrantUntil(ctx, request, request.Deadline)
	if err != nil {
		return workflowToolError(request.ToolName, err)
	}
	request.Approval = &grant
	return nil
}

func (r *ToolStepRunner) prepare(ctx context.Context, call *StepCall, toolID string, args json.RawMessage) (*toolbroker.ToolRequest, error) {
	if _, scoped := durable.TurnScopeFrom(ctx); scoped {
		return nil, codedError(ErrorCodeExecutorInvalid, "a workflow tool step never runs inside a durable turn")
	}
	definition, found := r.broker.Definition(toolID, tools.Version)
	if !found {
		return nil, codedError(ErrorCodeCapabilityExceeded, "tool "+toolID+" is not registered")
	}
	if definition.Orchestration {
		return nil, codedError(ErrorCodeExecutorInvalid, "tool "+toolID+" is an orchestration tool and cannot run as a workflow step")
	}
	if definition.Channel == principal.ChannelEmail {
		return nil, codedError(ErrorCodeCapabilityUnavailable, "a workflow run has no email account to send from")
	}
	trust, err := checkStepApproval(call)
	if err != nil {
		return nil, err
	}
	class := principal.ParseClass(call.Origin.PrincipalClass)
	persona, ok := r.registry.Lookup(call.Origin.AgentID)
	if !ok {
		return nil, codedError(ErrorCodePersonaMissing, "persona "+call.Origin.AgentID+" is not configured")
	}
	if !persona.StepToolAllowed(class, &definition) {
		return nil, codedError(ErrorCodeCapabilityExceeded, "tool "+toolID+" is outside the ceiling of persona "+call.Origin.AgentID+" for "+string(class)+" runs")
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		return nil, codedError(ErrorCodeExecutorInvalid, "tool step has no deadline")
	}
	ids := workflowToolIDsFor(call)
	if err := r.checkRunSession(ctx, ids.sessionID, &call.Origin); err != nil {
		return nil, err
	}
	request := &toolbroker.ToolRequest{
		RequestID:             ids.requestID,
		SessionID:             ids.sessionID,
		TurnID:                ids.turnID,
		PrincipalID:           call.Origin.PrincipalID,
		PrincipalClass:        class,
		AgentID:               call.Origin.AgentID,
		Knowledge:             persona.KnowledgeFor(class),
		Tainted:               call.Origin.Tainted || call.Approval.Tainted,
		Deadline:              deadline,
		IdempotencyKey:        ids.idempotencyKey,
		AllocateEventSequence: true,
		EventInvocation:       ids.requestID,
		EventBranch:           "main",
		EventAuthor:           "workflow",
		ToolName:              toolID,
		ToolVersion:           tools.Version,
		Arguments:             args,
		Capabilities:          slices.Clone(definition.RequiredCapabilities),
		Trust:                 trust,
	}
	if err := r.grant(ctx, call, request); err != nil {
		return nil, err
	}
	return request, nil
}

func (r *ToolStepRunner) Invoke(ctx context.Context, toolID string, args json.RawMessage) (ToolStepResult, error) {
	call, ok := StepCallFrom(ctx)
	if len(args) == 0 || string(args) == "{}" || string(args) == "null" {
		return ToolStepResult{}, r.refused(ctx, &call, toolID, codedError(
			ErrorCodeStepFailed,
			"tool step has no arguments: the workflow-core step model does not carry tool arguments, so no canned or placeholder payload is executed; real tool-step arguments ship with the durable-workflows feature",
		))
	}
	if !ok {
		return ToolStepResult{}, r.refused(ctx, &call, toolID, codedError(ErrorCodeExecutorInvalid, "tool step has no step call"))
	}
	request, err := r.prepare(ctx, &call, toolID, args)
	if err != nil {
		return ToolStepResult{}, r.refused(ctx, &call, toolID, err)
	}
	result, err := r.execute(ctx, request)
	if err != nil {
		return ToolStepResult{}, workflowToolError(toolID, err)
	}
	return ToolStepResult{Output: result.Output}, nil
}

func (r *ToolStepRunner) execute(ctx context.Context, request *toolbroker.ToolRequest) (toolbroker.ToolResult, error) {
	if r.route != nil {
		release := r.route.Hold(request.SessionID, &StepRequester{
			Principal: request.PrincipalID,
			Class:     request.PrincipalClass,
			Persona:   request.AgentID,
		})
		if release != nil {
			defer release()
		}
	}
	return r.broker.Execute(ctx, request)
}
