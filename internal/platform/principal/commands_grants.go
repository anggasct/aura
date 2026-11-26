package principal

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/platform/clikit"
)

func newGrantsCmd(open CommandOpener) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "grants",
		Short: "Inspect and revoke owner standing grants",
	}
	cmd.AddCommand(newGrantsListCmd(open), newGrantsRevokeCmd(open))
	return cmd
}

func newGrantsListCmd(open CommandOpener) *cobra.Command {
	var principalID string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List active standing grants",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runWithOpener(cmd, open, CommandModeStorage, func(ctx context.Context, env *CommandEnv) error {
				grants := NewStandingGrantStore(env.DB)
				active, err := grants.List(ctx, principalID, time.Now().UTC())
				if err != nil {
					return err
				}
				if len(active) == 0 {
					return clikit.WriteLines(cmd, []string{"no standing grants"})
				}
				lines := []string{"ID\tPRINCIPAL\tPERSONA\tTOOL\tEXPIRES"}
				for i := range active {
					grant := &active[i]
					lines = append(lines, fmt.Sprintf("%s\t%s\t%s\t%s\t%s", grant.ID, grant.PrincipalID, grant.AgentID, grant.ToolName, grant.ExpiresAt.UTC().Format(time.RFC3339)))
				}
				return clikit.WriteLines(cmd, lines)
			})
		},
	}
	cmd.Flags().StringVar(&principalID, "principal", "", "only grants for this principal id")
	return cmd
}

func newGrantsRevokeCmd(open CommandOpener) *cobra.Command {
	return &cobra.Command{
		Use:   "revoke <grant-id>",
		Short: "End a standing grant immediately",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWithOpener(cmd, open, CommandModeStorage, func(ctx context.Context, env *CommandEnv) error {
				grants := NewStandingGrantStore(env.DB)
				now := time.Now().UTC()
				active, err := grants.List(ctx, "", now)
				if err != nil {
					return err
				}
				index := slices.IndexFunc(active, func(grant StandingGrant) bool { return grant.ID == args[0] })
				if index < 0 {
					return clikit.NewExitCodeError(1, errors.New("standing_grant_not_found: no active standing grant has this id"))
				}
				revoked, err := grants.Revoke(ctx, args[0], now)
				if err != nil {
					return err
				}
				if !revoked {
					return clikit.NewExitCodeError(1, errors.New("standing_grant_not_found: no active standing grant has this id"))
				}
				if err := recordStandingGrantOwnerAction(ctx, env.Actions, AccessActionStandingGrantRevoke, &active[index], now); err != nil {
					return err
				}
				return clikit.WriteLines(cmd, []string{"revoked " + args[0]})
			})
		},
	}
}
