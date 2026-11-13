package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/config"
	toolsbuiltin "github.com/anggasct/aura/internal/modules/builtin"
	"github.com/anggasct/aura/internal/modules/workflow"
	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/principal"
)

func newWorkflowProposalsCmd(gf *globalFlags) *cobra.Command {
	var status string
	cmd := &cobra.Command{
		Use:   "proposals",
		Short: "List agent-proposed workflows",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			switch status {
			case "", workflow.ProposalProposed, workflow.ProposalApproved, workflow.ProposalRejected, workflow.ProposalExpired:
			default:
				return &usageError{err: errors.New("--status must be proposed, approved, rejected or expired")}
			}
			return withStorage(cmd, gf, true, func(ctx context.Context, _ *slog.Logger, _ *config.Config, db *sql.DB) error {
				proposals, err := workflow.NewStore(db).Proposals(ctx, status, time.Now())
				if err != nil {
					return err
				}
				if len(proposals) == 0 {
					return writeChildLines(cmd, []string{"no proposals"})
				}
				lines := []string{"ID\tSTATUS\tPERSONA\tCREATED\tGOAL"}
				for _, proposal := range proposals {
					lines = append(lines, fmt.Sprintf("%s\t%s\t%s\t%s\t%s", proposal.ID, proposal.Status, workflow.EscapeText(proposal.AgentID), proposal.CreatedAt.UTC().Format(time.RFC3339), workflow.EscapeText(proposal.Spec.Goal)))
				}
				return writeChildLines(cmd, lines)
			})
		},
	}
	cmd.Flags().StringVar(&status, "status", "", "filter by proposed, approved, rejected or expired")
	return cmd
}

func newWorkflowProposalCmd(gf *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "proposal",
		Short: "Show, approve or reject one agent-proposed workflow",
	}
	cmd.AddCommand(
		&cobra.Command{
			Use:   "show <proposal-id>",
			Short: "Show a proposal and its plan",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				return withStorage(cmd, gf, true, func(ctx context.Context, _ *slog.Logger, _ *config.Config, db *sql.DB) error {
					proposal, err := workflow.NewStore(db).Proposal(ctx, args[0], time.Now())
					if err != nil {
						return err
					}
					lines := []string{
						"proposal: " + workflow.EscapeText(proposal.ID),
						"status: " + workflow.EscapeText(proposal.Status),
						"persona: " + workflow.EscapeText(proposal.AgentID),
						"session: " + workflow.EscapeText(proposal.SessionID),
						"digest: " + workflow.EscapeText(proposal.Digest),
					}
					lines = append(lines, "tainted: "+yesNo(proposal.Tainted))
					if proposal.Tainted {
						lines = append(lines, approval.UntrustedInputNotice)
					}
					if proposal.RunID != "" {
						lines = append(lines, "run: "+workflow.EscapeText(proposal.RunID))
					}
					if proposal.PlanText == "" {
						return writeChildLines(cmd, append(lines, planNotStoredLine))
					}
					return writeChildLines(cmd, append(lines, proposal.PlanText))
				})
			},
		},
		&cobra.Command{
			Use:   "approve <proposal-id>",
			Short: "Approve a proposal and start exactly the approved spec",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				result, err := config.Load(gf.configPath)
				if err != nil {
					return err
				}
				return withApp(cmd, &commandInput{result: &result, parts: []Part{PartWorkflows}}, func(ctx context.Context, app *App) error {
					secrets := slices.Concat(toolsbuiltin.ConfiguredToolSecrets(result.Config), githubCredentialSecrets(result.Config))
					runID, err := approveStoredProposal(ctx, result.Config, app.interpreter, workflow.NewStore(app.db), args[0], secrets, time.Now())
					if err != nil {
						return err
					}
					return writeChildLines(cmd, []string{"approved: " + args[0], "run: " + runID})
				})
			},
		},
		&cobra.Command{
			Use:   "reject <proposal-id>",
			Short: "Reject a proposal; nothing runs",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				result, err := config.Load(gf.configPath)
				if err != nil {
					return err
				}
				return withApp(cmd, &commandInput{result: &result, parts: []Part{PartWorkflows}}, func(ctx context.Context, app *App) error {
					runs := workflow.NewStore(app.db)
					if _, err := runs.Proposal(ctx, args[0], time.Now()); err != nil {
						return err
					}
					if err := runs.DecideProposal(ctx, args[0], workflow.ProposalRejected, "", time.Now()); err != nil {
						return err
					}
					return writeChildLines(cmd, []string{"rejected: " + args[0]})
				})
			},
		},
	)
	return cmd
}

func yesNo(value bool) string {
	if value {
		return "yes"
	}
	return "no"
}

const planNotStoredLine = "plan: not stored; this proposal predates plan storage; propose it again"

type planCheck func(proposal *workflow.Proposal, graph *workflow.Graph) error

func approveRenderedProposal(ctx context.Context, _ *config.Config, interpreter *workflow.Interpreter, runs *workflow.Store, id string, render workflow.ToolCallText, now time.Time) (string, error) {
	return approveOnStoredPlan(ctx, interpreter, runs, id, now, func(proposal *workflow.Proposal, graph *workflow.Graph) error {
		if render == nil {
			return planUnshowable("no tool display is available to show the plan again")
		}
		plan, err := workflow.RenderPlan(proposal.Spec, graph, render)
		if err != nil {
			return err
		}
		if plan != proposal.PlanText {
			return planUnshowable("the plan changed since it was proposed; propose it again")
		}
		return nil
	})
}

func approveStoredProposal(ctx context.Context, _ *config.Config, interpreter *workflow.Interpreter, runs *workflow.Store, id string, secrets []string, now time.Time) (string, error) {
	return approveOnStoredPlan(ctx, interpreter, runs, id, now, func(proposal *workflow.Proposal, _ *workflow.Graph) error {
		if holdsSecret(proposal.PlanText, secrets) || stepArgumentsHoldSecret(proposal.Spec, secrets) {
			return planUnshowable("the plan holds a configured secret value; propose it again without it")
		}
		return nil
	})
}

func planUnshowable(detail string) error {
	return &workflow.Error{Code: workflow.ErrorCodePlanUnshowable, Detail: detail}
}

func holdsSecret(text string, secrets []string) bool {
	for _, secret := range secrets {
		if secret != "" && strings.Contains(text, secret) {
			return true
		}
	}
	return false
}

func valueHoldsSecret(value any, secrets []string) bool {
	switch typed := value.(type) {
	case string:
		return holdsSecret(typed, secrets)
	case []any:
		return slices.ContainsFunc(typed, func(item any) bool { return valueHoldsSecret(item, secrets) })
	case map[string]any:
		for key, item := range typed {
			if holdsSecret(key, secrets) || valueHoldsSecret(item, secrets) {
				return true
			}
		}
	}
	return false
}

func stepArgumentsHoldSecret(spec *workflow.Spec, secrets []string) bool {
	for index := range spec.Steps {
		executor := &spec.Steps[index].Executor
		if executor.Kind != workflow.KindTool {
			continue
		}
		if holdsSecret(string(executor.ToolArgs), secrets) {
			return true
		}
		var decoded any
		if json.Unmarshal(executor.ToolArgs, &decoded) == nil && valueHoldsSecret(decoded, secrets) {
			return true
		}
	}
	return false
}

func approveOnStoredPlan(ctx context.Context, interpreter *workflow.Interpreter, runs *workflow.Store, id string, now time.Time, check planCheck) (string, error) {
	proposal, err := runs.Proposal(ctx, id, now)
	if err != nil {
		return "", err
	}
	if proposal.Status != workflow.ProposalProposed {
		return "", &workflow.Error{Code: workflow.ErrorCodeProposalDecided, Detail: "proposal " + id + " is " + proposal.Status}
	}
	digest, err := workflow.Digest(proposal.Spec)
	if err != nil {
		return "", err
	}
	if digest != proposal.Digest {
		return "", &workflow.Error{Code: workflow.ErrorCodeDigestMismatch, Detail: "proposal " + id + " no longer matches the digest it was proposed with"}
	}
	if proposal.PlanText == "" {
		return "", planUnshowable("proposal " + id + " has no stored plan; propose it again")
	}
	graph, err := interpreter.CompileProposal(proposal.Spec, proposal.AgentID)
	if err != nil {
		return "", err
	}
	if err := check(proposal, graph); err != nil {
		return "", err
	}
	if err := interpreter.LoadAll(ctx, []*workflow.Spec{proposal.Spec}, interpreter.ValidationDeps()); err != nil {
		return "", err
	}
	if err := runs.ApproveProposal(ctx, id, &workflow.Approval{
		DefinitionID: proposal.Spec.ID, DefinitionVersion: proposal.Spec.Version, Digest: digest,
		AgentID: proposal.AgentID, ApproverID: principal.OwnerID, Tainted: proposal.Tainted, ApprovedAt: now,
	}); err != nil {
		return "", err
	}
	summary, err := interpreter.StartApproved(ctx, id, &workflow.RunInput{}, &workflow.RunOrigin{
		PrincipalID: proposal.PrincipalID, PrincipalClass: string(principal.ClassOwner), AgentID: proposal.AgentID, SessionID: proposal.SessionID, Tainted: proposal.Tainted,
	})
	if err != nil {
		return "", err
	}
	return summary.ID, nil
}
