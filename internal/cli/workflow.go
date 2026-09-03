package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/approval"
	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/durable"
	"github.com/anggasct/aura/internal/runtime/adk"
	"github.com/anggasct/aura/internal/tools/builtin"
	"github.com/anggasct/aura/internal/workflow"
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
	)
	return cmd
}

// workflowValidationDeps gathers the registries validation checks against.
// A registry build failure is returned so validation fails closed instead
// of silently skipping the resolution checks.
func workflowValidationDeps(cfg *config.Config) (workflow.ValidationDeps, error) {
	deps := workflow.ValidationDeps{
		KnownTools:     toolsbuiltin.DefinitionNames(),
		EffectfulTools: toolsbuiltin.EffectfulToolNames(),
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
			if _, err := workflow.Compile(spec, deps); err != nil {
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
			ctx := cmd.Context()
			result, err := config.Load(gf.configPath)
			if err != nil {
				return err
			}
			cfg := result.Config
			interpreter, closeStorage, err := buildWorkflowInterpreter(ctx, cfg)
			if err != nil {
				return err
			}
			defer closeStorage()
			runInput := &workflow.RunInput{}
			if inputFlag != "" {
				if err := json.Unmarshal([]byte(inputFlag), runInput); err != nil {
					return fmt.Errorf("invalid --input json: %w", err)
				}
			}
			summary, err := interpreter.Start(ctx, args[0], runInput)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "run: %s\nstatus: %s\n", summary.ID, summary.Status)
			return err
		},
	}
	cmd.Flags().StringVar(&inputFlag, "input", "", "run input as JSON (objective, resources, permissions)")
	return cmd
}

// buildWorkflowInterpreter opens storage, loads and validates the
// configured definitions, and wires the in-process interpreter.
func buildWorkflowInterpreter(ctx context.Context, cfg *config.Config) (*workflow.Interpreter, func(), error) {
	db, err := openStorage(ctx, cfg)
	if err != nil {
		return nil, nil, err
	}
	closeStorage := func() { _ = db.Close() }
	definitionsDir := workflowDefinitionsDir(cfg)
	specs, err := workflow.LoadDefinitionsDir(definitionsDir)
	if err != nil {
		return nil, closeStorage, err
	}
	deps, err := workflowValidationDeps(cfg)
	if err != nil {
		return nil, closeStorage, err
	}
	options := &workflow.Options{
		MaxConcurrentSteps: workflowMaxConcurrentSteps(cfg),
		Logger:             slogDiscard{},
	}
	if cfg.Tools != nil {
		tools, err := newWorkflowToolRunner(cfg, db)
		if err != nil {
			return nil, closeStorage, err
		}
		options.Tools = tools
	}
	interpreter := workflow.NewInterpreter(workflow.NewStore(db), durable.NewFake(), options)
	for _, spec := range specs {
		if err := interpreter.Load(ctx, spec, deps); err != nil {
			return nil, closeStorage, fmt.Errorf("definition %s: %w", spec.ID, err)
		}
	}
	return interpreter, closeStorage, nil
}

func workflowMaxConcurrentSteps(cfg *config.Config) int {
	if cfg.Workflows == nil || cfg.Workflows.MaxConcurrentSteps <= 0 {
		return 4
	}
	return cfg.Workflows.MaxConcurrentSteps
}

// workflowToolRunner adapts the builtin tool executor onto the interpreter
// port; the broker policy path stays unchanged.
type workflowToolRunner struct {
	executor *toolsbuiltin.Executor
}

func (r *workflowToolRunner) Invoke(ctx context.Context, toolID string, args json.RawMessage) (json.RawMessage, error) {
	return r.executor.Execute(ctx, &runtimeadk.BuiltinToolRequest{
		ToolName:    toolID,
		ToolVersion: "v1",
		Arguments:   args,
		Trust:       string(approval.TrustTrustedConfiguration),
	})
}

// newWorkflowToolRunner adapts the builtin tool executor onto the
// interpreter port; the broker policy path stays unchanged. It is only
// called when a tools section is configured; construction failures are
// returned so start exits with the underlying cause.
func newWorkflowToolRunner(cfg *config.Config, db *sql.DB) (workflow.ToolRunner, error) {
	_, artifactRoot, _, err := storagePaths(cfg)
	if err != nil {
		return nil, fmt.Errorf("resolve tool artifact root: %w", err)
	}
	executor, err := toolsbuiltin.New(cfg, db, artifactRoot, nil, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("build builtin tool executor: %w", err)
	}
	return &workflowToolRunner{executor: executor}, nil
}

type slogDiscard struct{}

func (slogDiscard) InfoContext(context.Context, string, ...any) {}

func (slogDiscard) WarnContext(context.Context, string, ...any) {}

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
			interpreter, closeStorage, err := buildWorkflowInterpreter(ctx, cfg)
			if err != nil {
				return err
			}
			defer closeStorage()
			if err := interpreter.Cancel(ctx, args[0]); err != nil && !errors.Is(err, durable.ErrUnknownRun) {
				return err
			}
			if err := runs.SetRunStatus(ctx, args[0], workflow.RunCancelled); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "run: %s\nstatus: %s\n", args[0], workflow.RunCancelled)
			return err
		},
	}
}
