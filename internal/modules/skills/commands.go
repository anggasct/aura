package skills

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/platform/fault"
)

type Opener func(cmd *cobra.Command, fn func(ctx context.Context, engine *Engine, defaultRoot string) error) error

type usageErr struct {
	err error
}

func (e *usageErr) Error() string { return e.err.Error() }

func (e *usageErr) Unwrap() error { return e.err }

func (e *usageErr) UsageError() bool { return true }

func (e *usageErr) IsUsageError() bool { return true }

func Commands(open Opener) []*cobra.Command {
	cmd := &cobra.Command{
		Use:   "skills",
		Short: "Inspect and review agent skills",
	}
	cmd.AddCommand(
		newListCmd(open),
		newShowCmd(open),
		newValidateCmd(open),
		newCreateCmd(open),
		newReviewCmd(open),
		newAcceptCmd(open),
		newRejectCmd(open),
		newDisableCmd(open),
		newDoctorCmd(open),
	)
	return []*cobra.Command{cmd}
}

func requireOpener(open Opener) error {
	if open == nil {
		return fault.New(ErrorCodeInvalidArgument, "skills command opener must not be nil")
	}
	return nil
}

func exactOneArg(cmd *cobra.Command, args []string) error {
	if len(args) != 1 || strings.TrimSpace(args[0]) == "" {
		return &usageErr{err: fmt.Errorf("%s requires exactly one argument", cmd.CommandPath())}
	}
	return nil
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

func originScope(raw string) string {
	var origin struct {
		Scope string `json:"scope"`
	}
	if err := json.Unmarshal([]byte(raw), &origin); err != nil || origin.Scope == "" {
		return "unknown"
	}
	return origin.Scope
}

func withEngine(cmd *cobra.Command, open Opener, fn func(ctx context.Context, engine *Engine, defaultRoot string) error) error {
	if err := requireOpener(open); err != nil {
		return err
	}
	return open(cmd, func(ctx context.Context, engine *Engine, defaultRoot string) error {
		if engine == nil {
			return errNilArgument("engine")
		}
		return fn(ctx, engine, defaultRoot)
	})
}

func newListCmd(open Opener) *cobra.Command {
	var state string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List skill packages",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := requireOpener(open); err != nil {
				return err
			}
			switch state {
			case "", StateQuarantined, StateActive, StateDisabled, StateRejected, StateConflict:
			default:
				return &usageErr{err: fmt.Errorf("%s: invalid --state %q", cmd.CommandPath(), state)}
			}
			return withEngine(cmd, open, func(ctx context.Context, engine *Engine, _ string) error {
				report, err := engine.Doctor(ctx)
				if err != nil {
					return err
				}
				lines := []string{"ID\tNAME\tSTATE\tDIGEST\tORIGIN"}
				for _, row := range report.Rows {
					if state != "" && row.State != state {
						continue
					}
					digest := row.Digest
					if len(digest) > 12 {
						digest = digest[:12]
					}
					lines = append(lines, row.ID+"\t"+row.Name+"\t"+row.State+"\t"+digest+"\t"+originScope(row.Origin))
				}
				if len(lines) == 1 {
					return writeLines(cmd, []string{"no skills"})
				}
				return writeLines(cmd, lines)
			})
		},
	}
	cmd.Flags().StringVar(&state, "state", "", "filter by state")
	return cmd
}

func newShowCmd(open Opener) *cobra.Command {
	return &cobra.Command{
		Use:   "show <id>",
		Short: "Show one skill package",
		Args:  exactOneArg,
		RunE: func(cmd *cobra.Command, args []string) error {
			return withEngine(cmd, open, func(ctx context.Context, engine *Engine, _ string) error {
				bundle, err := engine.Review(ctx, args[0])
				if err != nil {
					return err
				}
				return writeLines(cmd, []string{
					"id: " + bundle.ID,
					"name: " + bundle.Name,
					"state: " + bundle.State,
					"digest: " + bundle.Digest,
					"origin: " + bundle.Origin,
					"description: " + bundle.Description,
					"compatibility: " + bundle.Compatibility,
					"requested: " + strings.Join(bundle.Requested, ","),
					"granted: " + strings.Join(bundle.Granted, ","),
					"reviewed: " + bundle.ReviewedAt,
					"findings: " + strings.Join(bundle.Findings, ","),
				})
			})
		},
	}
}

func newValidateCmd(open Opener) *cobra.Command {
	return &cobra.Command{
		Use:   "validate <path>",
		Short: "Validate one skill package directory",
		Args:  exactOneArg,
		RunE: func(cmd *cobra.Command, args []string) error {
			return withEngine(cmd, open, func(ctx context.Context, engine *Engine, _ string) error {
				digest, findings, err := engine.ValidateDir(ctx, args[0])
				if err != nil {
					return err
				}
				if len(findings) > 0 {
					lines := append([]string{}, findings...)
					lines = append(lines, "skill_invalid")
					if err := writeLines(cmd, lines); err != nil {
						return err
					}
					return errors.New("skill_invalid")
				}
				return writeLines(cmd, []string{"valid: " + digest})
			})
		},
	}
}

func newCreateCmd(open Opener) *cobra.Command {
	var name, description, root string
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Stage a new skill draft into quarantine",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := requireOpener(open); err != nil {
				return err
			}
			if strings.TrimSpace(name) == "" || strings.TrimSpace(description) == "" {
				return &usageErr{err: fmt.Errorf("%s requires --name and --description", cmd.CommandPath())}
			}
			return withEngine(cmd, open, func(ctx context.Context, engine *Engine, defaultRoot string) error {
				targetRoot := root
				if targetRoot == "" {
					if defaultRoot == "" {
						return errors.New("skills roots are not configured")
					}
					targetRoot = defaultRoot
				}
				summary, err := engine.StageDraft(ctx, name, description, targetRoot)
				if err != nil {
					return err
				}
				return writeLines(cmd, []string{"id: " + summary.ID, "digest: " + summary.Digest})
			})
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "skill name")
	cmd.Flags().StringVar(&description, "description", "", "skill description")
	cmd.Flags().StringVar(&root, "root", "", "skill root (default: first configured root)")
	return cmd
}

func newReviewCmd(open Opener) *cobra.Command {
	return &cobra.Command{
		Use:   "review <id>",
		Short: "Show the review bundle for a quarantined skill",
		Args:  exactOneArg,
		RunE: func(cmd *cobra.Command, args []string) error {
			return withEngine(cmd, open, func(ctx context.Context, engine *Engine, _ string) error {
				bundle, err := engine.Review(ctx, args[0])
				if err != nil {
					return err
				}
				if bundle.State != StateQuarantined {
					if err := writeLines(cmd, []string{"state: " + bundle.State}); err != nil {
						return err
					}
					return fmt.Errorf("skill %q is not reviewable", args[0])
				}
				return writeLines(cmd, []string{
					"id: " + bundle.ID,
					"name: " + bundle.Name,
					"origin: " + bundle.Origin,
					"digest: " + bundle.Digest,
					"description: " + bundle.Description,
					"compatibility: " + bundle.Compatibility,
					"requested: " + strings.Join(bundle.Requested, ","),
					"allowed-tools: " + strings.Join(bundle.AllowedTools, ","),
					"granted: " + strings.Join(bundle.Granted, ","),
					"scripts: " + strings.Join(bundle.Scripts, ","),
					"files: " + strings.Join(bundle.Files, ","),
					"links: " + strings.Join(bundle.Links, ","),
					"findings: " + strings.Join(bundle.Findings, ","),
				})
			})
		},
	}
}

func newAcceptCmd(open Opener) *cobra.Command {
	var digest string
	var grants []string
	cmd := &cobra.Command{
		Use:   "accept <id>",
		Short: "Accept a quarantined skill with exact grants",
		Args:  exactOneArg,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := requireOpener(open); err != nil {
				return err
			}
			if strings.TrimSpace(digest) == "" {
				return &usageErr{err: fmt.Errorf("%s requires --digest", cmd.CommandPath())}
			}
			return withEngine(cmd, open, func(ctx context.Context, engine *Engine, _ string) error {
				if _, err := engine.Accept(ctx, args[0], digest, grants); err != nil {
					return err
				}
				return writeLines(cmd, []string{"accepted: " + args[0]})
			})
		},
	}
	cmd.Flags().StringVar(&digest, "digest", "", "exact content digest")
	cmd.Flags().StringSliceVar(&grants, "grant", nil, "granted capability (repeatable)")
	return cmd
}

func newRejectCmd(open Opener) *cobra.Command {
	var reason string
	cmd := &cobra.Command{
		Use:   "reject <id>",
		Short: "Reject a quarantined skill",
		Args:  exactOneArg,
		RunE: func(cmd *cobra.Command, args []string) error {
			return withEngine(cmd, open, func(ctx context.Context, engine *Engine, _ string) error {
				if _, err := engine.Reject(ctx, args[0], reason); err != nil {
					return err
				}
				return writeLines(cmd, []string{"rejected: " + args[0]})
			})
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "", "rejection reason")
	return cmd
}

func newDisableCmd(open Opener) *cobra.Command {
	return &cobra.Command{
		Use:   "disable <id>",
		Short: "Disable an active skill",
		Args:  exactOneArg,
		RunE: func(cmd *cobra.Command, args []string) error {
			return withEngine(cmd, open, func(ctx context.Context, engine *Engine, _ string) error {
				if err := engine.Disable(ctx, args[0]); err != nil {
					return err
				}
				return writeLines(cmd, []string{"disabled: " + args[0]})
			})
		},
	}
}

func newDoctorCmd(open Opener) *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Report skill registry health",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withEngine(cmd, open, func(ctx context.Context, engine *Engine, _ string) error {
				report, err := engine.Doctor(ctx)
				if err != nil {
					return err
				}
				lines := make([]string, 0, len(report.Rows)+len(report.Conflicts))
				for _, row := range report.Rows {
					lines = append(lines, row.ID+" "+row.State+" "+row.Digest+" "+row.Origin+" "+strings.Join(row.Findings, ","))
				}
				for _, group := range report.Conflicts {
					lines = append(lines, "conflict "+group.Name+": "+strings.Join(group.IDs, ","))
				}
				if len(lines) == 0 {
					lines = append(lines, "no skills")
				}
				return writeLines(cmd, lines)
			})
		},
	}
}
