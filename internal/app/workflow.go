package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	kitapproval "github.com/anggasct/aura/internal/channels/kit/approval"
	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/modules/builtin"
	"github.com/anggasct/aura/internal/modules/github"
	"github.com/anggasct/aura/internal/modules/workflow"
	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/store"
)

func newWorkflowCmd(gf *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "workflow",
		Short: "Validate and run declarative workflows",
	}
	cmd.AddCommand(
		newWorkflowValidateCmd(gf),
		newWorkflowDefinitionsCmd(gf),
		newWorkflowStartCmd(gf),
		newWorkflowRunsCmd(gf),
		newWorkflowInspectCmd(gf),
		newWorkflowCancelCmd(gf),
		newWorkflowApproveCmd(gf),
		newWorkflowSignalCmd(gf),
		newWorkflowProposalsCmd(gf),
		newWorkflowProposalCmd(gf),
	)
	return cmd
}

func stepToolCatalog(cfg *config.Config) (*toolsbuiltin.StepCatalog, error) {
	credentials := githubCredentials(cfg)
	if len(credentials) == 0 {
		return toolsbuiltin.NewStepCatalog(nil), nil
	}
	definitions, err := github.CheckedDefinitions(credentials)
	if err != nil {
		return nil, fmt.Errorf("prepare github step tools: %w", err)
	}
	return toolsbuiltin.NewStepCatalog(definitions), nil
}

func workflowValidationDeps(cfg *config.Config) (*workflow.ValidationDeps, error) {
	catalog, err := stepToolCatalog(cfg)
	if err != nil {
		return nil, err
	}
	deps := &workflow.ValidationDeps{
		KnownTools:     toolsbuiltin.StepToolNames(personaToolNames(cfg)),
		EffectfulTools: append(toolsbuiltin.EffectfulToolNames(), github.ToolNames()...),
		CheckToolCall:  catalog.CheckArguments,
	}
	if cfg.Workflows != nil {
		deps.DefaultStepTimeout = time.Duration(cfg.Workflows.DefaultStepTimeout)
	}
	registry, err := buildAgentRegistry(cfg)
	if err != nil {
		return deps, err
	}
	deps.Agents = registry
	deps.StepAllowed = stepCeiling(registry, catalog)
	return deps, nil
}

func stepCeiling(registry *auraagent.Registry, steps *toolsbuiltin.StepCatalog) func(agentID string, class principal.Class, toolID string) (allowed, personaKnown bool) {
	return func(agentID string, class principal.Class, toolID string) (bool, bool) {
		persona, known := registry.Lookup(agentID)
		if !known {
			return false, false
		}
		tool, found := steps.Lookup(toolID)
		return found && persona.StepToolAllowed(class, tool), true
	}
}

func workflowSet(specs []*workflow.Spec) func(id string) (*workflow.Spec, bool) {
	byID := make(map[string]*workflow.Spec, len(specs))
	for _, spec := range specs {
		byID[spec.ID] = spec
	}
	return func(id string) (*workflow.Spec, bool) {
		spec, ok := byID[id]
		return spec, ok
	}
}

func newWorkflowValidateCmd(gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "validate <file>",
		Short: "Validate a workflow definition file",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			result, err := config.Load(gf.configPath)
			if err != nil {
				return err
			}
			spec, err := workflow.LoadSpecFile(args[0])
			if err != nil {
				return err
			}
			deps, err := workflowValidationDeps(result.Config)
			if err != nil {
				return err
			}
			siblings, err := workflow.LoadDefinitionsDir(workflowDefinitionsDir(result.Config))
			if err != nil {
				return err
			}
			if _, err := workflow.Compile(spec, deps); err != nil {
				return fmt.Errorf("invalid workflow definition: %w", err)
			}
			if err := workflow.ValidateComposition(spec, workflowSet(append(siblings, spec))); err != nil {
				return fmt.Errorf("invalid workflow definition: %w", err)
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), "valid: "+spec.ID)
			return err
		},
	}
}

func newWorkflowDefinitionsCmd(gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "definitions",
		Short: "List workflow definitions from the configured directory",
		RunE: func(cmd *cobra.Command, args []string) error {
			result, err := config.Load(gf.configPath)
			if err != nil {
				return err
			}
			specs, err := workflow.LoadDefinitionsDir(workflowDefinitionsDir(result.Config))
			if err != nil {
				return err
			}
			deps, err := workflowValidationDeps(result.Config)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			for _, spec := range specs {
				if _, err := workflow.Compile(spec, deps); err != nil {
					return fmt.Errorf("definition %s: %w", spec.ID, err)
				}
				stepIDs := make([]string, 0, len(spec.Steps))
				for index := range spec.Steps {
					stepIDs = append(stepIDs, spec.Steps[index].ID)
				}
				if _, err := fmt.Fprintf(out, "%s\tv%d\t%s\n", spec.ID, spec.Version, strings.Join(stepIDs, ",")); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

func workflowDefinitionsDir(cfg *config.Config) string {
	if cfg.Workflows == nil || cfg.Workflows.DefinitionsDir == "" {
		return ""
	}
	return cfg.Workflows.DefinitionsDir
}

func newWorkflowStartCmd(gf *globalFlags) *cobra.Command {
	var inputFlag string
	cmd := &cobra.Command{
		Use:   "start <id> [--input json]",
		Short: "Start one workflow run",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			result, err := config.Load(gf.configPath)
			if err != nil {
				return err
			}
			return withApp(cmd, &commandInput{result: &result, parts: []Part{PartWorkflows}}, func(ctx context.Context, app *App) error {
				runInput := &workflow.RunInput{}
				if inputFlag != "" {
					if err := json.Unmarshal([]byte(inputFlag), runInput); err != nil {
						return fmt.Errorf("invalid --input json: %w", err)
					}
				}
				summary, err := app.interpreter.StartConfigured(ctx, args[0], runInput, principal.OwnerID, principal.ClassOwner, "")
				if err != nil {
					return err
				}
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "run: %s\nstatus: %s\nnote: the run is executed by a running aura server\n", summary.ID, summary.Status)
				return err
			})
		},
	}
	cmd.Flags().StringVar(&inputFlag, "input", "", "run input as JSON (objective, resources, permissions)")
	return cmd
}

func workflowMaxConcurrentSteps(cfg *config.Config) int {
	if cfg.Workflows == nil || cfg.Workflows.MaxConcurrentSteps <= 0 {
		return 4
	}
	return cfg.Workflows.MaxConcurrentSteps
}

type workflowToolRunnerInput struct {
	Executor *toolsbuiltin.Executor
	Sessions store.SessionService
	Registry *auraagent.Registry
	Runs     *workflow.Store
	Route    *kitapproval.OwnerRoute
	Logger   *slog.Logger
}

type workflowToolRunner struct {
	in     *workflowToolRunnerInput
	broker *toolbroker.Broker
	logger *slog.Logger
	now    func() time.Time
}

type workflowToolIDs struct {
	sessionID      string
	turnID         string
	requestID      string
	idempotencyKey string
}

func workflowToolIDsFor(call *workflow.StepCall) workflowToolIDs {
	sum := sha256.Sum256([]byte(call.RunID + "/" + call.StepID))
	digest := hex.EncodeToString(sum[:])[:24]
	return workflowToolIDs{
		sessionID:      workflow.RunSessionID(call.RunID),
		turnID:         "turn-" + digest,
		requestID:      "req-" + digest,
		idempotencyKey: "wf/" + call.RunID + "/" + call.StepID,
	}
}

func workflowTrustLabel(kind workflow.ApprovalKind) (approval.TrustLabel, error) {
	switch kind {
	case workflow.ApprovalInstalled:
		return approval.TrustTrustedConfiguration, nil
	case workflow.ApprovalProposal, workflow.ApprovalRoutine:
		return approval.TrustDerivedUntrusted, nil
	default:
		return "", &workflow.Error{Code: workflow.ErrorCodeApprovalMissing, Detail: "workflow approval has no known kind"}
	}
}

func (r *workflowToolRunner) checkRunSession(ctx context.Context, sessionID string, origin *workflow.RunOrigin) error {
	sess, err := r.in.Sessions.Get(ctx, sessionID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return &workflow.Error{Code: workflow.ErrorCodeSessionInvalid, Detail: "run session " + sessionID + " does not exist"}
	case err != nil:
		return &workflow.Error{Code: workflow.ErrorCodeStepFailed, Detail: "read run session: " + err.Error()}
	case sess.OwnerID != origin.PrincipalID || sess.AgentID != origin.AgentID:
		return &workflow.Error{Code: workflow.ErrorCodeSessionInvalid, Detail: "run session " + sessionID + " does not belong to the run's origin"}
	}
	return nil
}

func checkStepApproval(call *workflow.StepCall) (approval.TrustLabel, error) {
	trust, err := workflowTrustLabel(call.Approval.Kind)
	if err != nil {
		return "", err
	}
	record := &call.Approval
	switch {
	case record.Digest == "" || record.DefinitionID == "" || record.DefinitionVersion < 1:
		return "", &workflow.Error{Code: workflow.ErrorCodeApprovalMissing, Detail: "step call carries an incomplete approval record"}
	case record.Digest != call.DefinitionDigest:
		return "", &workflow.Error{Code: workflow.ErrorCodeApprovalMissing, Detail: "step call approval digest does not match the run's definition digest"}
	case record.DefinitionID != call.DefinitionID || record.DefinitionVersion != call.DefinitionVersion:
		return "", &workflow.Error{Code: workflow.ErrorCodeApprovalMissing, Detail: "step call approval belongs to another definition"}
	}
	return trust, nil
}

func workflowToolError(toolID string, err error) error {
	code, ok := toolbroker.CodeOf(err)
	if !ok {
		return err
	}
	if code == toolbroker.ResultCapabilityUnavailable {
		return &workflow.Error{Code: workflow.ErrorCodeCapabilityUnavailable, Detail: "tool " + toolID + " needs a capability this host does not provide"}
	}
	if code == toolbroker.ResultApprovalRequired || code == toolbroker.ResultUntrustedInputNoApprover {
		return &workflow.Error{Code: workflow.ErrorCodeApprovalRejected, Detail: err.Error()}
	}
	return err
}

func (r *workflowToolRunner) refused(ctx context.Context, call *workflow.StepCall, toolID string, err error) error {
	var code string
	if coded, ok := workflow.CodeOf(err); ok {
		code = string(coded)
	} else if coded, ok := toolbroker.CodeOf(err); ok {
		code = string(coded)
	}
	r.logger.WarnContext(ctx, "workflow tool step refused before the broker", "component", "workflow",
		"run_id", call.RunID, "step_id", call.StepID, "tool", toolID, "code", code, "error", err.Error())
	return err
}

func (r *workflowToolRunner) ownsProposalRun(ctx context.Context, call *workflow.StepCall) error {
	proposal, err := r.in.Runs.Proposal(ctx, call.Approval.ProposalID, r.now())
	if err != nil {
		return &workflow.Error{Code: workflow.ErrorCodeApprovalSpent, Detail: "the proposal's claimed run could not be read: " + err.Error()}
	}
	if call.RunID == "" || proposal.RunID != call.RunID {
		return &workflow.Error{Code: workflow.ErrorCodeApprovalSpent, Detail: "the tainted proposal was approved for another run"}
	}
	return nil
}

func (r *workflowToolRunner) attaches(ctx context.Context, call *workflow.StepCall, request *toolbroker.ToolRequest) (bool, error) {
	class := principal.ParseClass(call.Origin.PrincipalClass)
	if class != principal.ClassOwner || call.Origin.PrincipalID != principal.OwnerID {
		return r.broker.StandingApprovalAllowed(ctx, request)
	}
	if !request.Tainted {
		return true, nil
	}
	if call.Approval.Kind == workflow.ApprovalProposal && call.Approval.Tainted {
		return true, r.ownsProposalRun(ctx, call)
	}
	return false, nil
}

func (r *workflowToolRunner) grant(ctx context.Context, call *workflow.StepCall, request *toolbroker.ToolRequest) error {
	attach, err := r.attaches(ctx, call, request)
	if err != nil || !attach {
		return err
	}
	if call.Approval.Kind == workflow.ApprovalProposal || call.Approval.Kind == workflow.ApprovalRoutine {
		if _, err := r.broker.ExactCallText(request.ToolName, request.Arguments); err != nil {
			return &workflow.Error{Code: workflow.ErrorCodePlanUnshowable, Detail: err.Error()}
		}
	}
	grant, err := r.broker.GrantUntil(ctx, request, request.Deadline)
	if err != nil {
		return workflowToolError(request.ToolName, err)
	}
	request.Approval = &grant
	return nil
}

func (r *workflowToolRunner) prepare(ctx context.Context, call *workflow.StepCall, toolID string, args json.RawMessage) (*toolbroker.ToolRequest, error) {
	if _, scoped := durable.TurnScopeFrom(ctx); scoped {
		return nil, &workflow.Error{Code: workflow.ErrorCodeExecutorInvalid, Detail: "a workflow tool step never runs inside a durable turn"}
	}
	definition, found := r.broker.Definition(toolID, "v1")
	if !found {
		return nil, &workflow.Error{Code: workflow.ErrorCodeCapabilityExceeded, Detail: "tool " + toolID + " is not registered"}
	}
	if definition.Orchestration {
		return nil, &workflow.Error{Code: workflow.ErrorCodeExecutorInvalid, Detail: "tool " + toolID + " is an orchestration tool and cannot run as a workflow step"}
	}
	if toolID == toolsbuiltin.EmailSendToolName {
		return nil, &workflow.Error{Code: workflow.ErrorCodeCapabilityUnavailable, Detail: "a workflow run has no email account to send from"}
	}
	trust, err := checkStepApproval(call)
	if err != nil {
		return nil, err
	}
	class := principal.ParseClass(call.Origin.PrincipalClass)
	persona, ok := r.in.Registry.Lookup(call.Origin.AgentID)
	if !ok {
		return nil, &workflow.Error{Code: workflow.ErrorCodePersonaMissing, Detail: "persona " + call.Origin.AgentID + " is not configured"}
	}
	if !persona.StepToolAllowed(class, &definition) {
		return nil, &workflow.Error{Code: workflow.ErrorCodeCapabilityExceeded, Detail: "tool " + toolID + " is outside the ceiling of persona " + call.Origin.AgentID + " for " + string(class) + " runs"}
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		return nil, &workflow.Error{Code: workflow.ErrorCodeExecutorInvalid, Detail: "tool step has no deadline"}
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
		ToolVersion:           "v1",
		Arguments:             args,
		Capabilities:          slices.Clone(definition.RequiredCapabilities),
		Trust:                 trust,
	}
	if err := r.grant(ctx, call, request); err != nil {
		return nil, err
	}
	return request, nil
}

func (r *workflowToolRunner) Invoke(ctx context.Context, toolID string, args json.RawMessage) (json.RawMessage, error) {
	call, ok := workflow.StepCallFrom(ctx)
	if len(args) == 0 || string(args) == "{}" || string(args) == "null" {
		return nil, r.refused(ctx, &call, toolID, &workflow.Error{
			Code:   workflow.ErrorCodeStepFailed,
			Detail: "tool step has no arguments: the workflow-core step model does not carry tool arguments, so no canned or placeholder payload is executed; real tool-step arguments ship with the durable-workflows feature",
		})
	}
	if !ok {
		return nil, r.refused(ctx, &call, toolID, &workflow.Error{Code: workflow.ErrorCodeExecutorInvalid, Detail: "tool step has no step call"})
	}
	request, err := r.prepare(ctx, &call, toolID, args)
	if err != nil {
		return nil, r.refused(ctx, &call, toolID, err)
	}
	result, err := r.execute(ctx, request)
	if err != nil {
		return nil, workflowToolError(toolID, err)
	}
	return result.Output, nil
}

func (r *workflowToolRunner) execute(ctx context.Context, request *toolbroker.ToolRequest) (toolbroker.ToolResult, error) {
	release := r.in.Route.Hold(request.SessionID, &kitapproval.Requester{Principal: request.PrincipalID, Class: request.PrincipalClass, Persona: request.AgentID})
	defer release()
	return r.broker.Execute(ctx, request)
}

func newWorkflowToolRunner(in *workflowToolRunnerInput) (workflow.ToolRunner, error) {
	if in == nil || in.Executor == nil || in.Executor.Broker() == nil || in.Sessions == nil || in.Registry == nil || in.Runs == nil {
		return nil, invalidOptions("workflow tool runner needs an executor, sessions, a persona registry and a workflow store")
	}
	runner := &workflowToolRunner{in: in, broker: in.Executor.Broker(), logger: in.Logger, now: time.Now}
	if runner.logger == nil {
		runner.logger = slog.Default()
	}
	return runner, nil
}

func newWorkflowRunsCmd(gf *globalFlags) *cobra.Command {
	var statusFilter string
	cmd := &cobra.Command{
		Use:   "runs [--status filter]",
		Short: "List workflow runs",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			result, err := config.Load(gf.configPath)
			if err != nil {
				return err
			}
			db, err := openStorage(ctx, result.Config)
			if err != nil {
				return err
			}
			defer func() { _ = db.Close() }()
			runs, err := workflow.NewStore(db).Runs(ctx, statusFilter)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			for _, run := range runs {
				if _, err := fmt.Fprintf(out, "%s\t%s\t%s@v%d\t%s\n", run.ID, run.Status, run.DefinitionID, run.DefinitionVersion, workflow.EscapeText(run.Goal)); err != nil {
					return err
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&statusFilter, "status", "", "filter by status")
	return cmd
}

func newWorkflowInspectCmd(gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "inspect <run>",
		Short: "Show one run and its steps",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			result, err := config.Load(gf.configPath)
			if err != nil {
				return err
			}
			db, err := openStorage(ctx, result.Config)
			if err != nil {
				return err
			}
			defer func() { _ = db.Close() }()
			runs := workflow.NewStore(db)
			run, err := runs.Run(ctx, args[0])
			if err != nil {
				return err
			}
			steps, err := runs.Steps(ctx, run.ID)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if _, err := fmt.Fprintf(out, "run: %s\nstatus: %s\ndefinition: %s@v%d\ngoal: %s\n", run.ID, run.Status, run.DefinitionID, run.DefinitionVersion, workflow.EscapeText(run.Goal)); err != nil {
				return err
			}
			for _, step := range steps {
				line := fmt.Sprintf("step: %s\t%s\tattempt %d", step.StepID, step.Status, step.Attempt)
				if step.ErrorCode != "" {
					line += "\t" + step.ErrorCode
				}
				if _, err := fmt.Fprintln(out, line); err != nil {
					return err
				}
				if step.OutputArtifactDigest != "" {
					if _, err := fmt.Fprintf(out, "  artifact: %s\n", step.OutputArtifactDigest); err != nil {
						return err
					}
					continue
				}
				if len(step.Output) > 0 {
					if _, err := fmt.Fprintf(out, "  output: %s\n", bytes.TrimSpace(step.Output)); err != nil {
						return err
					}
				}
			}
			correlations, err := runs.ListCorrelationsByRun(ctx, run.ID)
			if err != nil {
				return err
			}
			for _, correlation := range correlations {
				matched := "awaiting"
				if correlation.DedupeKey != "" {
					matched = "matched"
				}
				if _, err := fmt.Fprintf(out, "correlation: %s\t%s\t%s\t%s\t%s\n",
					correlation.Source, correlation.EventType, correlation.ExternalID, correlation.SignalName, matched); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

func newWorkflowCancelCmd(gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "cancel <run>",
		Short: "Cancel a workflow run cooperatively",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			result, err := config.Load(gf.configPath)
			if err != nil {
				return err
			}
			cfg := result.Config
			db, err := openStorage(ctx, cfg)
			if err != nil {
				return err
			}
			defer func() { _ = db.Close() }()
			runs := workflow.NewStore(db)
			run, err := runs.Run(ctx, args[0])
			if err != nil {
				return err
			}
			if run.Status == workflow.RunSucceeded || run.Status == workflow.RunFailed || run.Status == workflow.RunCancelled {
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "run: %s\nstatus: %s\n", run.ID, run.Status)
				return err
			}
			return withApp(cmd, &commandInput{result: &result, parts: []Part{PartWorkflows}}, func(ctx context.Context, app *App) error {
				if err := app.interpreter.Cancel(ctx, args[0]); err != nil && !errors.Is(err, durable.ErrUnknownRun) {
					return err
				}
				if err := runs.SetRunStatus(ctx, args[0], workflow.RunCancelled); err != nil {
					return err
				}
				_, err := fmt.Fprintf(cmd.OutOrStdout(), "run: %s\nstatus: %s\n", args[0], workflow.RunCancelled)
				return err
			})
		},
	}
}

func newWorkflowApproveCmd(gf *globalFlags) *cobra.Command {
	var stepID string
	var reject bool
	cmd := &cobra.Command{
		Use:   "approve <run> --step <id> [--reject]",
		Short: "Resolve a suspended approval step",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if stepID == "" {
				return errors.New("step id is required: pass --step <id>")
			}
			result, err := config.Load(gf.configPath)
			if err != nil {
				return err
			}
			return withApp(cmd, &commandInput{result: &result, parts: []Part{PartWorkflows}}, func(ctx context.Context, app *App) error {
				decision := "approve"
				if reject {
					decision = "reject"
				}
				payload, err := json.Marshal(map[string]string{"decision": decision})
				if err != nil {
					return fmt.Errorf("encode approval decision: %w", err)
				}
				if err := app.interpreter.Signal(ctx, args[0], "approval."+stepID, payload); err != nil {
					return err
				}
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "run: %s\nsignal: %s\ndecision: %s\n", args[0], "approval."+stepID, decision)
				return err
			})
		},
	}
	cmd.Flags().StringVar(&stepID, "step", "", "approval step id to resolve")
	cmd.Flags().BoolVar(&reject, "reject", false, "reject instead of approving")
	return cmd
}

func newWorkflowSignalCmd(gf *globalFlags) *cobra.Command {
	var stepID string
	var payloadFlag string
	cmd := &cobra.Command{
		Use:   "signal <run> --step <id> --payload json",
		Short: "Deliver a payload to a suspended wait step",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if stepID == "" {
				return errors.New("step id is required: pass --step <id>")
			}
			if !json.Valid([]byte(payloadFlag)) {
				return errors.New("payload must be valid JSON")
			}
			result, err := config.Load(gf.configPath)
			if err != nil {
				return err
			}
			return withApp(cmd, &commandInput{result: &result, parts: []Part{PartWorkflows}}, func(ctx context.Context, app *App) error {
				if err := app.interpreter.Signal(ctx, args[0], "wait."+stepID, []byte(payloadFlag)); err != nil {
					return err
				}
				_, err := fmt.Fprintf(cmd.OutOrStdout(), "run: %s\nsignal: %s\n", args[0], "wait."+stepID)
				return err
			})
		},
	}
	cmd.Flags().StringVar(&stepID, "step", "", "wait step id to signal")
	cmd.Flags().StringVar(&payloadFlag, "payload", "", "signal payload as JSON")
	return cmd
}
