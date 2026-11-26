package workflow

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/configkit"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/principal"
)

const proposalCardQueue = 16

type Caller struct {
	SessionID      string
	PrincipalID    string
	PrincipalClass principal.Class
	AgentID        string
	Tainted        bool
}

type Info struct {
	ID      string `json:"id"`
	Version int    `json:"version"`
	Goal    string `json:"goal"`
}

type StepState struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

type RunState struct {
	RunID      string      `json:"run_id"`
	WorkflowID string      `json:"workflow_id,omitempty"`
	Status     string      `json:"status"`
	Steps      []StepState `json:"steps,omitempty"`
}

type ProposalSummary struct {
	ProposalID     string `json:"proposal_id"`
	Digest         string `json:"digest"`
	Plan           string `json:"plan"`
	UntrustedInput bool   `json:"untrusted_input"`
	ApproveWith    string `json:"approve_with"`
}

type RoutineSummary struct {
	ID      string `json:"id"`
	Version int    `json:"version"`
}

type ReplyTargeter interface {
	DiscordReplyTarget(sessionID string) (account, channelID string, ok bool)
}

type ProposalCard struct {
	ID      string
	Plan    string
	Tainted bool
}

type ProposalOfferer interface {
	OfferProposal(ctx context.Context, card ProposalCard, decide func(ctx context.Context, approved bool) (string, error)) error
	ServesSession(sessionID string) bool
}

type proposalOffer struct {
	sessionID string
	card      ProposalCard
}

type proposalCards struct {
	offerers []ProposalOfferer
	logger   *slog.Logger
	requests chan proposalOffer
}

type ControllerDeps struct {
	Interpreter *Interpreter
	Runs        *Store
	Registry    *auraagent.Registry
	Quarantined []string
	Targets     []ReplyTargeter
	Offerers    []ProposalOfferer
	ShowCall    ToolCallText
	Logger      *slog.Logger
	Now         func() time.Time
}

type Controller struct {
	interpreter *Interpreter
	runs        *Store
	registry    *auraagent.Registry
	targets     []ReplyTargeter
	cards       *proposalCards
	showCall    ToolCallText
	now         func() time.Time
}

func NewController(deps *ControllerDeps) (*Controller, error) {
	if deps == nil || deps.Interpreter == nil || deps.Runs == nil || deps.Registry == nil {
		return nil, codedError(ErrorCodeInvalidArgument, "workflow controller requires an interpreter, store, and persona registry")
	}
	definitions := deps.Registry.Definitions()
	for i := range definitions {
		for _, id := range definitions[i].Workflows {
			if slices.Contains(deps.Quarantined, id) {
				continue
			}
			if _, ok := deps.Interpreter.Spec(id); !ok {
				return nil, configkit.Field("agents.definitions", fault.New(auraagent.ErrorCodePersonaInvalid, fmt.Sprintf("persona %q workflows: %q is not a loaded workflow definition", definitions[i].ID, id)))
			}
		}
	}
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	controller := &Controller{
		interpreter: deps.Interpreter,
		runs:        deps.Runs,
		registry:    deps.Registry,
		targets:     slices.Clone(deps.Targets),
		showCall:    deps.ShowCall,
		now:         now,
	}
	if len(deps.Offerers) > 0 {
		controller.cards = &proposalCards{
			offerers: slices.Clone(deps.Offerers),
			logger:   logger,
			requests: make(chan proposalOffer, proposalCardQueue),
		}
	}
	return controller, nil
}

func (c *Controller) Interpreter() *Interpreter {
	if c == nil {
		return nil
	}
	return c.interpreter
}

func (c *Controller) Store() *Store {
	if c == nil {
		return nil
	}
	return c.runs
}

func (c *Controller) ShowCall() ToolCallText {
	if c == nil {
		return nil
	}
	return c.showCall
}

func (c *Controller) RunProposalCards(ctx context.Context) {
	if c == nil || c.cards == nil {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case offer := <-c.cards.requests:
			c.deliverProposalCard(ctx, &offer)
		}
	}
}

func (c *Controller) offerProposal(ctx context.Context, sessionID string, card ProposalCard) {
	if c.cards == nil {
		return
	}
	select {
	case c.cards.requests <- proposalOffer{sessionID: sessionID, card: card}:
	default:
		c.cards.logger.WarnContext(ctx, "proposal card unavailable", "component", "workflow", "proposal_id", card.ID, "error", "card queue is full")
	}
}

func (c *Controller) deliverProposalCard(ctx context.Context, offer *proposalOffer) {
	sessionID, proposalID := offer.sessionID, offer.card.ID
	offerer := c.cards.offerers[0]
	for _, candidate := range c.cards.offerers {
		if candidate.ServesSession(sessionID) {
			offerer = candidate
			break
		}
	}
	decide := func(ctx context.Context, approved bool) (string, error) {
		if !approved {
			if err := c.runs.DecideProposal(ctx, proposalID, ProposalRejected, "", c.now()); err != nil {
				return "", err
			}
			return "rejected " + proposalID, nil
		}
		runID, err := ApproveRenderedProposal(ctx, c.interpreter, c.runs, proposalID, c.showCall, c.now())
		if err != nil {
			return "", err
		}
		return "approved " + proposalID + ": run " + runID, nil
	}
	if err := offerer.OfferProposal(ctx, offer.card, decide); err != nil {
		c.cards.logger.WarnContext(ctx, "proposal card unavailable", "component", "workflow", "proposal_id", proposalID, "error", err)
	}
}

func (c *Controller) permits(caller *Caller, id string) bool {
	if caller.PrincipalClass == principal.ClassOwner {
		return true
	}
	definition, ok := c.registry.Lookup(caller.AgentID)
	return ok && slices.Contains(definition.Workflows, id)
}

func (c *Controller) List(_ context.Context, caller *Caller) ([]Info, error) {
	if caller == nil {
		return nil, codedError(ErrorCodeInvalidArgument, "caller must not be nil")
	}
	var workflows []Info
	for _, spec := range c.interpreter.Definitions() {
		if c.permits(caller, spec.ID) {
			workflows = append(workflows, Info{ID: spec.ID, Version: spec.Version, Goal: spec.Goal})
		}
	}
	slices.SortFunc(workflows, func(a, b Info) int { return cmp.Compare(a.ID, b.ID) })
	return workflows, nil
}

func (c *Controller) Start(ctx context.Context, caller *Caller, id, objective string) (RunState, error) {
	if caller == nil {
		return RunState{}, codedError(ErrorCodeInvalidArgument, "caller must not be nil")
	}
	if !c.permits(caller, id) {
		return RunState{}, codedError(ErrorCodeNotPermitted, "this workflow is not available here")
	}
	if _, ok := c.interpreter.Spec(id); !ok {
		return RunState{}, fmt.Errorf("workflow %q is not loaded", id)
	}
	origin := &RunOrigin{
		PrincipalID: caller.PrincipalID, PrincipalClass: string(caller.PrincipalClass), AgentID: caller.AgentID, SessionID: caller.SessionID, Tainted: caller.Tainted,
	}
	for _, targeter := range c.targets {
		if account, channelID, ok := targeter.DiscordReplyTarget(caller.SessionID); ok {
			origin.ReplyAccount, origin.ReplyChannelID = account, channelID
			break
		}
	}
	summary, err := c.interpreter.Start(ctx, id, &RunInput{Objective: strings.TrimSpace(objective)}, origin)
	if err != nil {
		return RunState{}, err
	}
	return RunState{RunID: summary.ID, WorkflowID: summary.DefinitionID, Status: summary.Status}, nil
}

func (c *Controller) visibleRun(ctx context.Context, caller *Caller, runID string) (*RunSummary, error) {
	run, err := c.runs.Run(ctx, runID)
	if fault.HasCode(err, ErrorCodeRunNotFound) {
		return nil, codedError(ErrorCodeRunNotFound, "no such workflow run")
	}
	if err != nil {
		return nil, err
	}
	if caller.PrincipalClass != principal.ClassOwner && (run.Origin.PrincipalID == "" || run.Origin.PrincipalID != caller.PrincipalID) {
		return nil, codedError(ErrorCodeRunNotFound, "no such workflow run")
	}
	return run, nil
}

func (c *Controller) Status(ctx context.Context, caller *Caller, runID string) (RunState, error) {
	if caller == nil {
		return RunState{}, codedError(ErrorCodeInvalidArgument, "caller must not be nil")
	}
	run, err := c.visibleRun(ctx, caller, runID)
	if err != nil {
		return RunState{}, err
	}
	steps, err := c.runs.Steps(ctx, run.ID)
	if err != nil {
		return RunState{}, err
	}
	state := RunState{RunID: run.ID, WorkflowID: run.DefinitionID, Status: run.Status, Steps: make([]StepState, 0, len(steps))}
	for _, step := range steps {
		state.Steps = append(state.Steps, StepState{ID: step.StepID, Status: step.Status})
	}
	return state, nil
}

func (c *Controller) Cancel(ctx context.Context, caller *Caller, runID string) (RunState, error) {
	if caller == nil {
		return RunState{}, codedError(ErrorCodeInvalidArgument, "caller must not be nil")
	}
	run, err := c.visibleRun(ctx, caller, runID)
	if err != nil {
		return RunState{}, err
	}
	if err := c.interpreter.Cancel(ctx, run.ID); err != nil {
		return RunState{}, err
	}
	status, err := c.interpreter.RunStatus(ctx, run.ID)
	if err != nil {
		return RunState{}, err
	}
	return RunState{RunID: run.ID, WorkflowID: run.DefinitionID, Status: status}, nil
}

func (c *Controller) Propose(ctx context.Context, caller *Caller, raw json.RawMessage) (ProposalSummary, error) {
	if caller == nil {
		return ProposalSummary{}, codedError(ErrorCodeInvalidArgument, "caller must not be nil")
	}
	if caller.PrincipalClass != principal.ClassOwner {
		return ProposalSummary{}, codedError(ErrorCodeNotPermitted, "this workflow is not available here")
	}
	var spec Spec
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&spec); err != nil {
		return ProposalSummary{}, codedError(ErrorCodeSpecInvalid, "spec is not a workflow spec: "+err.Error())
	}
	id := NewProposalID()
	normalized := NormalizeProposal(&spec, id)
	graph, err := c.interpreter.CompileProposal(normalized, caller.AgentID)
	if err != nil {
		return ProposalSummary{}, err
	}
	plan, err := RenderPlan(normalized, graph, c.showCall)
	if err != nil {
		return ProposalSummary{}, err
	}
	digest, err := Digest(normalized)
	if err != nil {
		return ProposalSummary{}, err
	}
	if err := c.runs.CreateProposal(ctx, &Proposal{
		ID: id, SessionID: caller.SessionID, AgentID: caller.AgentID, PrincipalID: caller.PrincipalID, Tainted: caller.Tainted,
		Spec: normalized, PlanText: plan, Digest: digest, CreatedAt: c.now().UTC(),
	}); err != nil {
		return ProposalSummary{}, err
	}
	c.offerProposal(ctx, caller.SessionID, ProposalCard{ID: id, Plan: plan, Tainted: caller.Tainted})
	return ProposalSummary{ProposalID: id, Digest: digest, Plan: plan, UntrustedInput: caller.Tainted, ApproveWith: "aura workflow proposal approve " + id}, nil
}

func (c *Controller) SaveRoutine(ctx context.Context, caller *Caller, runID, name string) (RoutineSummary, error) {
	if caller == nil {
		return RoutineSummary{}, codedError(ErrorCodeInvalidArgument, "caller must not be nil")
	}
	if caller.PrincipalClass != principal.ClassOwner {
		return RoutineSummary{}, codedError(ErrorCodeNotPermitted, "this workflow is not available here")
	}
	routine, err := c.interpreter.SaveRoutine(ctx, runID, name)
	if fault.HasCode(err, ErrorCodeRunNotFound) {
		return RoutineSummary{}, codedError(ErrorCodeRunNotFound, "no such workflow run")
	}
	if err != nil {
		return RoutineSummary{}, err
	}
	return RoutineSummary{ID: routine.ID, Version: routine.Version}, nil
}
