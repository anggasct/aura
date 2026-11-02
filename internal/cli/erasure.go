package cli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/principal"
	"github.com/anggasct/aura/internal/store"
)

const (
	accessActionPrincipalForget = "principal_forget"
	retentionSweepInterval      = 24 * time.Hour
)

func erasureLines(counts *store.ErasureCounts) []string {
	return []string{
		fmt.Sprintf("sessions: %d", counts.Sessions),
		fmt.Sprintf("memory_documents: %d", counts.MemoryDocuments),
		fmt.Sprintf("grants: %d", counts.Grants),
		fmt.Sprintf("standing_grants: %d", counts.StandingGrants),
		fmt.Sprintf("pairing_requests: %d", counts.PairingRequests),
		fmt.Sprintf("usage_links: %d", counts.UsageLinks),
	}
}

type principalEraser struct {
	db           *sql.DB
	artifactRoot string
	now          func() time.Time
}

func (e *principalEraser) Forget(ctx context.Context, principalID string) error {
	if _, err := store.ErasePrincipal(ctx, e.db, principalID, true, store.ErasureOptions{ArtifactRoot: e.artifactRoot}); err != nil {
		return err
	}
	channel, _, _ := strings.Cut(principalID, ":")
	return recordAccessAction(ctx, e.db, &accessAction{Action: accessActionPrincipalForget, Principal: principalID, Channel: channel}, "forgetme-"+principalID, e.now().UTC())
}

func newPrincipalForgetCmd(gf *globalFlags) *cobra.Command {
	var apply bool
	cmd := &cobra.Command{
		Use:   "forget <id>",
		Short: "Erase a trusted or public principal's sessions, memory, grants and pairing rows",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withStorage(cmd, gf, true, func(ctx context.Context, _ *slog.Logger, cfg *config.Config, db *sql.DB) error {
				principalID := args[0]
				channel, externalID, err := principal.ParseIdentity(principalID)
				if err != nil {
					return &usageError{err: errors.New("principal must be a channel identity such as discord:<user id>")}
				}
				owners, err := ownerResolver(cfg)
				if err != nil {
					return err
				}
				if owners.IsOwnerIdentity(channel, externalID) {
					return &usageError{err: errors.New("the owner is not a principal; use aura memory forget for owner data")}
				}
				_, artifactRoot, _, err := storagePaths(cfg)
				if err != nil {
					return err
				}
				counts, err := store.ErasePrincipal(ctx, db, principalID, apply, store.ErasureOptions{ArtifactRoot: artifactRoot})
				if err != nil {
					return err
				}
				lines := erasureLines(&counts)
				if !apply {
					return writeChildLines(cmd, append(lines, "dry run: pass --apply to erase "+principalID))
				}
				if err := recordAccessAction(ctx, db, &accessAction{Action: accessActionPrincipalForget, Principal: principalID, Channel: channel}, "forget-"+principalID, time.Now().UTC()); err != nil {
					return err
				}
				return writeChildLines(cmd, append(lines, "erased "+principalID))
			})
		},
	}
	cmd.Flags().BoolVar(&apply, "apply", false, "erase instead of printing what would be erased")
	return cmd
}

func sweepRetention(ctx context.Context, cfg *config.Config, db *sql.DB, artifactRoot string, now time.Time) (map[principal.Class]int, error) {
	swept := map[principal.Class]int{}
	if cfg.Access == nil {
		return swept, nil
	}
	ttl := map[principal.Class]time.Duration{
		principal.ClassTrusted: time.Duration(cfg.Access.Retention.Trusted),
		principal.ClassPublic:  time.Duration(cfg.Access.Retention.Public),
	}
	summaries, err := collectPrincipals(ctx, cfg, db)
	if err != nil {
		return swept, err
	}
	for _, summary := range summaries {
		limit, ok := ttl[summary.class]
		if !ok || summary.lastActive.IsZero() || now.Sub(summary.lastActive) <= limit {
			continue
		}
		if _, err := store.ErasePrincipal(ctx, db, summary.id, true, store.ErasureOptions{KeepGrants: true, ArtifactRoot: artifactRoot}); err != nil {
			return swept, err
		}
		swept[summary.class]++
	}
	return swept, nil
}

func runRetentionSweeps(ctx context.Context, cfg *config.Config, db *sql.DB, artifactRoot string, logger *slog.Logger) {
	sweep := func() {
		swept, err := sweepRetention(ctx, cfg, db, artifactRoot, time.Now().UTC())
		if err != nil {
			logger.WarnContext(ctx, "retention sweep failed", "component", "access", "error", err)
			return
		}
		logger.InfoContext(ctx, "retention sweep", "component", "access", "trusted", swept[principal.ClassTrusted], "public", swept[principal.ClassPublic])
	}
	sweep()
	ticker := time.NewTicker(retentionSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sweep()
		}
	}
}
