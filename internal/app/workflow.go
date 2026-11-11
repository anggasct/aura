package app

import (
	"bytes"
	"context"
	"crypto/rand"
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
	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/turn/adk"
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

func workflowValidationDeps(cfg *config.Config) (workflow.ValidationDeps, error) {
	deps := workflow.ValidationDeps{
		KnownTools:     append(toolsbuiltin.DefinitionNames(), github.ToolNames()...),
		EffectfulTools: append(toolsbuiltin.EffectfulToolNames(), github.ToolNames()...),
	}
	if cfg.Workflows != nil {
		deps.DefaultStepTimeout = time.Duration(cfg.Workflows.DefaultStepTimeout)
	}
	registry, err := buildAgentRegistry(cfg)
	if err != nil {
		return deps, err
	}
	deps.Agents = registry
	return deps, nil
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
				summary, err := app.interpreter.Start(ctx, args[0], runInput)
				if err != nil {
					return err
				}
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "run: %s\nstatus: %s\n", summary.ID, summary.Status)
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
	executor *toolsbuiltin.Executor
	db       *sql.DB
	caps     map[string][]string
}

func (r *workflowToolRunner) Invoke(ctx context.Context, toolID string, args json.RawMessage) (json.RawMessage, error) {
	if len(args) == 0 || string(args) == "{}" || string(args) == "null" {
		return nil, &workflow.Error{
			Code:   workflow.ErrorCodeStepFailed,
			Detail: "tool step has no arguments: the workflow-core step model does not carry tool arguments, so no canned or placeholder payload is executed; real tool-step arguments ship with the durable-workflows feature",
		}
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, fmt.Errorf("generate tool request id: %w", err)
	}
	sessionID := "wf-" + hex.EncodeToString(b[:8])
	turnID := "turn-" + hex.EncodeToString(b[8:12])
	reqID := "req-" + hex.EncodeToString(b[12:])
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if r.db != nil {
		_, _ = r.db.ExecContext(ctx,
			`INSERT INTO session (id, owner_id, metadata_json, created_at, updated_at) VALUES (?, ?, '{}', ?, ?) ON CONFLICT(id) DO NOTHING`,
			sessionID, "workflow", now, now,
		)
	}
	output, err := r.executor.Execute(ctx, &runtimeadk.BuiltinToolRequest{
		RequestID:       reqID,
		SessionID:       sessionID,
		TurnID:          turnID,
		IdempotencyKey:  "wf/" + reqID,
		EventSequence:   1,
		EventInvocation: reqID,
		EventBranch:     "main",
		EventAuthor:     "workflow",
		ToolName:        toolID,
		ToolVersion:     "v1",
		Arguments:       args,
		Capabilities:    slices.Clone(r.caps[toolID]),
		Trust:           string(approval.TrustTrustedConfiguration),
	})
	if code, ok := toolbroker.CodeOf(err); ok && code == toolbroker.ResultCapabilityUnavailable {
		return nil, &workflow.Error{Code: workflow.ErrorCodeCapabilityUnavailable, Detail: "tool " + toolID + " needs a capability this host does not provide"}
	}
	return output, err
}

func newWorkflowToolRunner(result *config.LoadResult, db *sql.DB, logger *slog.Logger) (workflow.ToolRunner, error) {
	cfg := result.Config
	_, artifactRoot, _, err := storagePaths(cfg)
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
	if err := github.Register(executor, github.ToolOptions{}); err != nil {
		return nil, fmt.Errorf("register github tools: %w", err)
	}
	caps := make(map[string][]string)
	definitions := executor.Definitions()
	for i := range definitions {
		caps[definitions[i].Name] = slices.Clone(definitions[i].RequiredCapabilities)
	}
	return &workflowToolRunner{executor: executor, db: db, caps: caps}, nil
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
				if _, err := fmt.Fprintf(out, "%s\t%s\t%s@v%d\t%s\n", run.ID, run.Status, run.DefinitionID, run.DefinitionVersion, run.Goal); err != nil {
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
			if _, err := fmt.Fprintf(out, "run: %s\nstatus: %s\ndefinition: %s@v%d\ngoal: %s\n", run.ID, run.Status, run.DefinitionID, run.DefinitionVersion, run.Goal); err != nil {
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
