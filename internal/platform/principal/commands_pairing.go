package principal

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/platform/clikit"
)

func parsePrincipalArg(channel, raw string) (string, error) {
	if !PairingChannel(channel) {
		return "", clikit.NewUsageError(errors.New("channel must be discord or telegram"))
	}
	externalID := strings.TrimPrefix(raw, channel+":")
	if _, _, err := ParseIdentity(channel + ":" + externalID); err != nil {
		return "", clikit.NewUsageError(fmt.Errorf("principal must be a %s user id", channel))
	}
	return Namespaced(channel, externalID), nil
}

func newPairingCmd(open CommandOpener) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pairing",
		Short: "Approve and manage DM pairing requests",
	}
	cmd.AddCommand(newPairingListCmd(open), newPairingApproveCmd(open), newPairingRevokeCmd(open), newPairingClearCmd(open))
	return cmd
}

func newPairingListCmd(open CommandOpener) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List pending pairing requests",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runWithOpener(cmd, open, CommandModeStorage, func(ctx context.Context, env *CommandEnv) error {
				if env.Pairings == nil {
					return errNilArgument("pairing store")
				}
				pending, err := env.Pairings.Pending(ctx, time.Now().UTC())
				if err != nil {
					return err
				}
				if len(pending) == 0 {
					return clikit.WriteLines(cmd, []string{"no pending pairing requests"})
				}
				lines := []string{"ACCOUNT\tPRINCIPAL\tEXPIRES"}
				for i := range pending {
					lines = append(lines, fmt.Sprintf("%s\t%s\t%s", pending[i].Account, pending[i].PrincipalID, pending[i].ExpiresAt.UTC().Format(time.RFC3339)))
				}
				return clikit.WriteLines(cmd, lines)
			})
		},
	}
}

func newPairingApproveCmd(open CommandOpener) *cobra.Command {
	return &cobra.Command{
		Use:   "approve <channel> <code>",
		Short: "Grant trusted access to the sender of a pairing code",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			channel := args[0]
			if !PairingChannel(channel) {
				return clikit.NewUsageError(errors.New("channel must be discord or telegram"))
			}
			return runWithOpener(cmd, open, CommandModeStorage, func(ctx context.Context, env *CommandEnv) error {
				if env.Pairings == nil {
					return errNilArgument("pairing store")
				}
				now := time.Now().UTC()
				if until, locked, err := PairingLock(ctx, env.Pairings, channel, now); err != nil {
					return err
				} else if locked {
					return clikit.NewExitCodeError(1, fmt.Errorf("pairing_locked: too many wrong codes; approvals for %s reopen at %s", channel, until.Format(time.RFC3339)))
				}
				request, found, err := env.Pairings.Approve(ctx, channel, PairingCodeDigest(args[1]), OwnerID, now)
				if err != nil {
					return err
				}
				if !found {
					if env.Actions != nil {
						if err := env.Actions.RecordOwnerAction(ctx, &OwnerAction{Action: AccessActionPairingInvalid, Channel: channel}, channel, now); err != nil {
							return err
						}
					}
					return clikit.NewExitCodeError(1, errors.New("pairing_invalid: no pending pairing request has this code"))
				}
				if env.Actions != nil {
					if err := env.Actions.RecordOwnerAction(ctx, &OwnerAction{Action: AccessActionPrincipalGrant, Principal: request.PrincipalID, Account: request.Account, Channel: channel}, request.PrincipalID, now); err != nil {
						return err
					}
				}
				return clikit.WriteLines(cmd, []string{"granted " + request.PrincipalID + " on " + request.Account})
			})
		},
	}
}

func newPairingRevokeCmd(open CommandOpener) *cobra.Command {
	return &cobra.Command{
		Use:   "revoke <channel> <principal>",
		Short: "Revoke a principal's paired trusted access",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			principalID, err := parsePrincipalArg(args[0], args[1])
			if err != nil {
				return err
			}
			return runWithOpener(cmd, open, CommandModeStorage, func(ctx context.Context, env *CommandEnv) error {
				grants := NewPrincipalGrantStore(env.DB)
				now := time.Now().UTC()
				accounts, err := grants.Revoke(ctx, principalID, now)
				if err != nil {
					return err
				}
				if len(accounts) == 0 {
					return clikit.NewExitCodeError(1, errors.New("grant_not_found: principal has no paired access; allowlisted users are removed from the config"))
				}
				lines := make([]string, 0, len(accounts))
				for _, account := range accounts {
					if env.Actions != nil {
						if err := env.Actions.RecordOwnerAction(ctx, &OwnerAction{Action: AccessActionPrincipalRevoke, Principal: principalID, Account: account, Channel: args[0]}, principalID+"-"+account, now); err != nil {
							return err
						}
					}
					lines = append(lines, "revoked "+principalID+" on "+account)
				}
				return clikit.WriteLines(cmd, lines)
			})
		},
	}
}

func newPairingClearCmd(open CommandOpener) *cobra.Command {
	return &cobra.Command{
		Use:   "clear-pending",
		Short: "Drop every pending pairing request",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runWithOpener(cmd, open, CommandModeStorage, func(ctx context.Context, env *CommandEnv) error {
				if env.Pairings == nil {
					return errNilArgument("pairing store")
				}
				cleared, err := env.Pairings.ClearPending(ctx)
				if err != nil {
					return err
				}
				return clikit.WriteLines(cmd, []string{fmt.Sprintf("cleared %d pending pairing requests", cleared)})
			})
		},
	}
}
