package principal

import (
	"context"
	"fmt"
	"os/user"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/platform/clikit"
)

func newOwnerCmd(open CommandOpener) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "owner",
		Short: "Inspect the owner principal and migrate owner keys",
	}
	cmd.AddCommand(newOwnerWhoamiCmd(open), newOwnerRekeyCmd(open))
	return cmd
}

func newOwnerWhoamiCmd(open CommandOpener) *cobra.Command {
	return &cobra.Command{
		Use:   "whoami",
		Short: "Show the canonical owner id and how many channel identities map to it",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runWithOpener(cmd, open, CommandModeConfig, func(_ context.Context, env *CommandEnv) error {
				var counts []string
				for _, channel := range []string{ChannelDiscord, ChannelTelegram, ChannelEmail} {
					if n := env.Owners.Count(channel); n > 0 {
						counts = append(counts, fmt.Sprintf("%s=%d", channel, n))
					}
				}
				identities := "none"
				if len(counts) > 0 {
					identities = strings.Join(counts, " ")
				}
				return clikit.WriteLines(cmd, []string{
					"owner: " + OwnerID,
					"terminal: owner",
					"identities: " + identities,
				})
			})
		},
	}
}

func newOwnerRekeyCmd(open CommandOpener) *cobra.Command {
	var apply bool
	cmd := &cobra.Command{
		Use:   "rekey",
		Short: "Preview or apply moving historical owner rows to canonical principal ids",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runWithOpener(cmd, open, CommandModeStorage, func(ctx context.Context, env *CommandEnv) error {
				if env.RekeyMoves == nil {
					return errNilArgument("owner rekey moves")
				}
				osUser := ""
				if current, err := user.Current(); err == nil {
					osUser = current.Username
				} else {
					env.Logger.WarnContext(ctx, "os user unavailable; terminal history is not rekeyed", "component", "owner", "error", err)
				}
				moves, err := env.RekeyMoves(osUser)
				if err != nil {
					return err
				}
				report, err := Rekey(ctx, env.DB, moves, apply, env.Rekeyers)
				if err != nil {
					return err
				}
				lines := OwnerRekeyLines(report, apply)
				if apply {
					if env.Actions != nil {
						if err := env.Actions.RecordOwnerRekey(ctx, report, time.Now().UTC()); err != nil {
							return err
						}
					}
					env.Logger.InfoContext(ctx, "owner rekey applied",
						"component", "owner",
						"sessions", report.Count(RekeyLineSessions),
						"memory_documents", report.Count(RekeyLineMemoryDocuments),
						"profile_facts", report.Count(RekeyLineProfileFacts),
						"profile_fact_conflicts", report.Count(RekeyLineProfileFactConflicts),
					)
				}
				return clikit.WriteLines(cmd, lines)
			})
		},
	}
	cmd.Flags().BoolVar(&apply, "apply", false, "apply the move (default dry-run)")
	return cmd
}

func OwnerRekeyLines(report RekeyReport, apply bool) []string {
	header := "dry-run: nothing written; rerun with --apply to move"
	if apply {
		header = "applied"
	}
	lines := []string{
		header,
		fmt.Sprintf("session: %d", report.Count(RekeyLineSessions)),
		fmt.Sprintf("memory_document: %d", report.Count(RekeyLineMemoryDocuments)),
		fmt.Sprintf("profile_fact: %d", report.Count(RekeyLineProfileFacts)),
	}
	if conflicts := report.Count(RekeyLineProfileFactConflicts); conflicts > 0 {
		lines = append(lines, fmt.Sprintf("profile_fact conflicts left in place: %d (review with aura profile list)", conflicts))
	}
	return lines
}
