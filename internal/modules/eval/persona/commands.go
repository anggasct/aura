package personaeval

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/fault"
)

type CommandRequest struct {
	PersonaID    string
	NeedBehavior bool
}

type CommandEnv struct {
	ConfigPath string
	Definition auraagent.Definition
	Service    *Service
}

type Opener func(cmd *cobra.Command, req CommandRequest, fn func(ctx context.Context, env CommandEnv) error) error

func Commands(open Opener) []*cobra.Command {
	return []*cobra.Command{
		newEvalCmd(open),
		newEvalResultsCmd(open),
	}
}

func runWithOpener(cmd *cobra.Command, open Opener, req CommandRequest, fn func(ctx context.Context, env CommandEnv) error) error {
	if open == nil {
		return fault.New(ErrorCodeInvalidArgument, "persona eval command opener must not be nil")
	}
	return open(cmd, req, func(ctx context.Context, env CommandEnv) error {
		if env.Service == nil {
			return fault.New(ErrorCodeInvalidArgument, "persona eval service must not be nil")
		}
		return fn(ctx, env)
	})
}

func newEvalCmd(open Opener) *cobra.Command {
	var behavior bool
	cmd := &cobra.Command{
		Use:   "eval <id>",
		Short: "Run a persona's policy evaluation, and its behavior evaluation with --behavior, and record the results",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWithOpener(cmd, open, CommandRequest{PersonaID: args[0], NeedBehavior: behavior}, func(ctx context.Context, env CommandEnv) error {
				if behavior && env.Definition.EvalBehavior == nil {
					return invalid("persona %q does not enable behavior evaluation (set evals.behavior)", env.Definition.ID)
				}
				recorded, err := env.Service.RunPolicy(ctx, env.ConfigPath, &env.Definition)
				if err != nil {
					return err
				}
				lines := []string{
					"persona: " + env.Definition.ID,
					"digest: " + shortDigest(recorded.Digest),
					fmt.Sprintf("cases: %d, failed: %d", recorded.CasesTotal, recorded.CasesFailed),
				}
				for _, name := range recorded.FailedCases {
					lines = append(lines, "failed: "+name)
				}
				passed := recorded.Passed
				if behavior {
					scored, report, err := env.Service.RunBehavior(ctx, env.ConfigPath, &env.Definition)
					if err != nil {
						return err
					}
					lines = append(lines, BehaviorLines(&report)...)
					passed = passed && scored.Passed
				}
				if !passed {
					if err := writeLines(cmd, append(lines, "result: fail")); err != nil {
						return err
					}
					return fault.New(ErrorCodeFailed, "the persona evaluation did not pass")
				}
				return writeLines(cmd, append(lines, "result: pass"))
			})
		},
	}
	cmd.Flags().BoolVar(&behavior, "behavior", false, "also run the live behavior evaluation (persona must set evals.behavior)")
	return cmd
}

func newEvalResultsCmd(open Opener) *cobra.Command {
	var limit int
	cmd := &cobra.Command{
		Use:   "eval-results <id>",
		Short: "List recorded evaluation results for a persona",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWithOpener(cmd, open, CommandRequest{PersonaID: args[0]}, func(ctx context.Context, env CommandEnv) error {
				results, err := env.Service.List(ctx, env.Definition.ID, limit)
				if err != nil {
					return err
				}
				if len(results) == 0 {
					return writeLines(cmd, []string{"no evaluation results"})
				}
				current := env.Definition.EvalDigest()
				lines := []string{"CREATED\tLAYER\tRESULT\tCASES\tFAILED\tDIGEST\tCURRENT"}
				for i := range results {
					outcome := "fail"
					if results[i].Passed {
						outcome = "pass"
					}
					lines = append(lines, fmt.Sprintf("%s\t%s\t%s\t%d\t%d\t%s\t%t",
						results[i].CreatedAt.UTC().Format(time.RFC3339),
						results[i].Layer,
						outcome,
						results[i].CasesTotal,
						results[i].CasesFailed,
						shortDigest(results[i].Digest),
						results[i].Digest == current,
					))
				}
				return writeLines(cmd, lines)
			})
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 20, "maximum results to list")
	return cmd
}

func BehaviorLines(report *BehaviorReport) []string {
	lines := make([]string, 0, 1+len(report.Cases))
	lines = append(lines, fmt.Sprintf("behavior score: %.2f (min %.2f)", report.Score, report.MinScore))
	for _, scored := range report.Cases {
		lines = append(lines, fmt.Sprintf("case %s: %.2f — %s", scored.Name, scored.Score, scored.Reason))
	}
	return lines
}

func shortDigest(digest string) string {
	if len(digest) > 12 {
		return digest[:12]
	}
	return digest
}

func writeLines(cmd *cobra.Command, lines []string) error {
	out := cmd.OutOrStdout()
	for _, line := range lines {
		if _, err := fmt.Fprintln(out, line); err != nil {
			return err
		}
	}
	return nil
}
