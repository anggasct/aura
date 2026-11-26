package principal

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/platform/clikit"
	"github.com/anggasct/aura/internal/platform/fault"
)

func EraseWithBlobs(ctx context.Context, db *sql.DB, scope ErasureScope, participants []Eraser, artifactRoot string, collect BlobCollector) (ErasureReport, int, error) {
	if db == nil {
		report, err := Erase(ctx, nil, scope, nil)
		return report, 0, err
	}
	report, err := Erase(ctx, db, scope, participants)
	if err != nil {
		return report, 0, err
	}
	var blobs int
	if scope.Apply && artifactRoot != "" && collect != nil &&
		(report.Count(ErasureLineSessions) > 0 ||
			report.Count(ErasureLineRunSessions) > 0 ||
			report.Count(ErasureLineLegacyStepSessions) > 0) {
		deleted, err := collect(ctx, db, artifactRoot, time.Now().UTC().Add(time.Second))
		if err != nil {
			return report, 0, fmt.Errorf("collect erased blobs: %w", err)
		}
		blobs = deleted
	}
	return report, blobs, nil
}

func ForgetPrincipal(ctx context.Context, db *sql.DB, liveRuns LiveRunCanceller, scope ErasureScope, participants []Eraser, artifactRoot string, collect BlobCollector) (cancelled []string, report ErasureReport, err error) {
	if scope.Apply && liveRuns != nil {
		if cancelled, err = liveRuns.CancelForPrincipal(ctx, scope.PrincipalID); err != nil {
			return nil, report, err
		}
	}
	report, _, err = EraseWithBlobs(ctx, db, scope, participants, artifactRoot, collect)
	return cancelled, report, err
}

func SweepRetention(ctx context.Context, access *Config, db *sql.DB, allowlisted []AllowlistedPrincipal, activity PrincipalActivityReader, participants []Eraser, artifactRoot string, collect BlobCollector, now time.Time) (map[Class]int, error) {
	swept := map[Class]int{}
	if access == nil {
		return swept, nil
	}
	ttl := map[Class]time.Duration{
		ClassTrusted: time.Duration(access.Retention.Trusted),
		ClassPublic:  time.Duration(access.Retention.Public),
	}
	summaries, err := CollectPrincipals(ctx, db, allowlisted, activity)
	if err != nil {
		return swept, err
	}
	for _, summary := range summaries {
		limit, ok := ttl[summary.Class]
		if !ok || summary.LastActive.IsZero() || now.Sub(summary.LastActive) <= limit {
			continue
		}
		if _, _, err := EraseWithBlobs(ctx, db, ErasureScope{PrincipalID: summary.ID, Apply: true, KeepGrants: true}, participants, artifactRoot, collect); err != nil {
			if fault.HasCode(err, ErrorCodeErasureBlocked) {
				continue
			}
			return swept, err
		}
		swept[summary.Class]++
	}
	return swept, nil
}

func newPrincipalForgetCmd(open CommandOpener) *cobra.Command {
	var apply bool
	cmd := &cobra.Command{
		Use:   "forget <id>",
		Short: "Erase a trusted or public principal's sessions, memory, grants, pairing rows and workflow runs",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			mode := CommandModeStorage
			if apply {
				mode = CommandModeForgetApply
			}
			return runWithOpener(cmd, open, mode, func(ctx context.Context, env *CommandEnv) error {
				return forgetCommand(ctx, cmd, env, args[0], apply)
			})
		},
	}
	cmd.Flags().BoolVar(&apply, "apply", false, "erase instead of printing what would be erased")
	return cmd
}

func forgetCommand(ctx context.Context, cmd *cobra.Command, env *CommandEnv, principalID string, apply bool) error {
	channel, externalID, err := ParseIdentity(principalID)
	if err != nil {
		return clikit.NewUsageError(errors.New("principal must be a channel identity such as discord:<user id>"))
	}
	if env.Owners.IsOwnerIdentity(channel, externalID) {
		return clikit.NewUsageError(errors.New("the owner is not a principal; use aura memory forget for owner data"))
	}
	cancelled, report, err := ForgetPrincipal(ctx, env.DB, env.LiveRuns, ErasureScope{PrincipalID: principalID, Apply: apply}, env.Erasers, env.ArtifactRoot, env.CollectBlobs)
	if err != nil {
		return err
	}
	lines := report.Lines()
	if len(cancelled) > 0 {
		lines = append([]string{"cancelled_runs: " + strings.Join(cancelled, ", ")}, lines...)
	}
	if !apply {
		return clikit.WriteLines(cmd, append(lines, "dry run: pass --apply to erase "+principalID))
	}
	if env.Actions != nil {
		if err := env.Actions.RecordOwnerAction(ctx, &OwnerAction{Action: AccessActionPrincipalForget, Principal: principalID, Channel: channel}, "forget-"+principalID, time.Now().UTC()); err != nil {
			return err
		}
	}
	return clikit.WriteLines(cmd, append(lines, "erased "+principalID))
}
