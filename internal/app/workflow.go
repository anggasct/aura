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

func stepCeiling(registry *auraagent.Registry, catalog *toolsbuiltin.StepCatalog) func(agentID string, class principal.Class, toolID string) (allowed, personaKnown bool) {
	return func(agentID string, class principal.Class, toolID string) (bool, bool) {
		persona, known := registry.Lookup(agentID)
		if !known {
			return false, false
		}
		tool, found := catalog.Lookup(toolID)
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

func workflowApprovalDecider(ctx context.Context, prompt *toolbroker.ApprovalPrompt) (bool, error) {
	return true, nil
}

type workflowToolRunner struct {
	executor      *toolsbuiltin.Executor
	catalog       *toolsbuiltin.StepCatalog
	sessions      store.SessionService
	registry      *auraagent.Registry
	caps          map[string][]string
	orchestration map[string]bool
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
	sess, err := r.sessions.Get(ctx, sessionID)
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

func (r *workflowToolRunner) Invoke(ctx context.Context, toolID string, args json.RawMessage) (json.RawMessage, error) {
	if len(args) == 0 || string(args) == "{}" || string(args) == "null" {
		return nil, &workflow.Error{
			Code:   workflow.ErrorCodeStepFailed,
			Detail: "tool step has no arguments: the workflow-core step model does not carry tool arguments, so no canned or placeholder payload is executed; real tool-step arguments ship with the durable-workflows feature",
		}
	}
	call, ok := workflow.StepCallFrom(ctx)
	if !ok {
		return nil, &workflow.Error{Code: workflow.ErrorCodeExecutorInvalid, Detail: "tool step has no step call"}
	}
	if r.orchestration[toolID] {
		return nil, &workflow.Error{Code: workflow.ErrorCodeExecutorInvalid, Detail: "tool " + toolID + " is an orchestration tool and cannot run as a workflow step"}
	}
	trust, err := workflowTrustLabel(call.Approval.Kind)
	if err != nil {
		return nil, err
	}
	class := principal.ParseClass(call.Origin.PrincipalClass)
	persona, ok := r.registry.Lookup(call.Origin.AgentID)
	if !ok {
		return nil, &workflow.Error{Code: workflow.ErrorCodePersonaMissing, Detail: "persona " + call.Origin.AgentID + " is not configured"}
	}
	if tool, found := r.catalog.Lookup(toolID); !found || !persona.StepToolAllowed(class, tool) {
		return nil, &workflow.Error{Code: workflow.ErrorCodeCapabilityExceeded, Detail: "tool " + toolID + " is outside the ceiling of persona " + call.Origin.AgentID + " for " + string(class) + " runs"}
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		return nil, &workflow.Error{Code: workflow.ErrorCodeExecutorInvalid, Detail: "tool step has no deadline"}
	}
	ids := workflowToolIDsFor(&call)
	if err := r.checkRunSession(ctx, ids.sessionID, &call.Origin); err != nil {
		return nil, err
	}
	result, err := r.executor.Broker().Execute(ctx, &toolbroker.ToolRequest{
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
		Capabilities:          slices.Clone(r.caps[toolID]),
		Trust:                 trust,
	})
	if code, ok := toolbroker.CodeOf(err); ok && code == toolbroker.ResultCapabilityUnavailable {
		return nil, &workflow.Error{Code: workflow.ErrorCodeCapabilityUnavailable, Detail: "tool " + toolID + " needs a capability this host does not provide"}
	}
	if err != nil {
		return nil, err
	}
	return result.Output, nil
}

func newWorkflowToolExecutor(result *config.LoadResult, db *sql.DB, logger *slog.Logger) (*toolsbuiltin.Executor, error) {
	_, artifactRoot, _, err := storagePaths(result.Config)
	if err != nil {
		return nil, fmt.Errorf("resolve tool artifact root: %w", err)
	}
	if logger == nil {
		logger = slog.Default()
	}
	executor, err := newToolExecutor(&toolExecutorInput{
		result: result, db: db, artifactRoot: artifactRoot, logger: logger,
		wiring: &toolWiring{decider: workflowApprovalDecider},
	})
	if err != nil {
		return nil, fmt.Errorf("build builtin tool executor: %w", err)
	}
	return executor, nil
}

func newWorkflowToolRunner(result *config.LoadResult, db *sql.DB, registry *auraagent.Registry, logger *slog.Logger) (workflow.ToolRunner, error) {
	executor, err := newWorkflowToolExecutor(result, db, logger)
	if err != nil {
		return nil, err
	}
	catalog, err := stepToolCatalog(result.Config)
	if err != nil {
		return nil, err
	}
	caps := make(map[string][]string)
	definitions := executor.Definitions()
	for i := range definitions {
		caps[definitions[i].Name] = slices.Clone(definitions[i].RequiredCapabilities)
	}
	orchestration := make(map[string]bool)
	registered := executor.Broker().Definitions()
	for i := range registered {
		if registered[i].Orchestration {
			orchestration[registered[i].Name] = true
		}
	}
	return &workflowToolRunner{executor: executor, catalog: catalog, sessions: store.NewSessionService(db), registry: registry, caps: caps, orchestration: orchestration}, nil
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
