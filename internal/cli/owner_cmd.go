package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"os/user"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/principal"
	"github.com/anggasct/aura/internal/store"
)

const (
	ownerRekeyEventKind = "principal.rekey.v1"
	legacyCronPrincipal = "cron"
)

func newOwnerCmd(gf *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "owner",
		Short: "Inspect the owner principal and migrate owner keys",
	}
	cmd.AddCommand(newOwnerWhoamiCmd(gf), newOwnerRekeyCmd(gf))
	return cmd
}

func newOwnerWhoamiCmd(gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "whoami",
		Short: "Show the canonical owner id and how many channel identities map to it",
		RunE: func(cmd *cobra.Command, args []string) error {
			return withStorage(cmd, gf, false, func(_ context.Context, _ *slog.Logger, cfg *config.Config, _ *sql.DB) error {
				owners, err := ownerResolver(cfg)
				if err != nil {
					return err
				}
				identities := "none"
				if n := owners.Count(principal.ChannelDiscord); n > 0 {
					identities = fmt.Sprintf("%s=%d", principal.ChannelDiscord, n)
				}
				return writeProfileLines(cmd, []string{
					"owner: " + principal.OwnerID,
					"terminal: owner",
					"identities: " + identities,
				})
			})
		},
	}
}

func newOwnerRekeyCmd(gf *globalFlags) *cobra.Command {
	var apply bool
	cmd := &cobra.Command{
		Use:   "rekey",
		Short: "Preview or apply moving historical owner rows to canonical principal ids",
		RunE: func(cmd *cobra.Command, args []string) error {
			return withStorage(cmd, gf, true, func(ctx context.Context, logger *slog.Logger, cfg *config.Config, db *sql.DB) error {
				osUser := ""
				if current, err := user.Current(); err == nil {
					osUser = current.Username
				} else {
					logger.WarnContext(ctx, "os user unavailable; terminal history is not rekeyed", "component", "owner", "error", err)
				}
				moves, err := ownerRekeyMoves(cfg, osUser)
				if err != nil {
					return err
				}
				report, err := store.RekeyOwners(ctx, db, moves, apply)
				if err != nil {
					return err
				}
				lines := ownerRekeyLines(report, apply)
				if apply {
					if err := recordOwnerRekey(ctx, db, report, time.Now().UTC()); err != nil {
						return err
					}
					logger.InfoContext(ctx, "owner rekey applied",
						"component", "owner",
						"sessions", report.Sessions,
						"memory_documents", report.MemoryDocuments,
						"profile_facts", report.ProfileFacts,
						"profile_fact_conflicts", report.ProfileFactConflicts,
					)
				}
				return writeProfileLines(cmd, lines)
			})
		},
	}
	cmd.Flags().BoolVar(&apply, "apply", false, "apply the move (default dry-run)")
	return cmd
}

func ownerRekeyMoves(cfg *config.Config, osUser string) ([]store.OwnerMove, error) {
	owners, err := ownerResolver(cfg)
	if err != nil {
		return nil, err
	}
	var moves []store.OwnerMove
	seen := map[string]bool{principal.OwnerID: true}
	add := func(from, to string) {
		if from == "" || seen[from] {
			return
		}
		seen[from] = true
		moves = append(moves, store.OwnerMove{From: from, To: to})
	}
	add(strings.TrimSpace(osUser), principal.OwnerID)
	add(legacyCronPrincipal, principal.OwnerID)
	if cfg != nil && cfg.Owner != nil {
		for _, identity := range cfg.Owner.Identities {
			if _, externalID, err := principal.ParseIdentity(identity); err == nil {
				add(externalID, principal.OwnerID)
			}
		}
	}
	if cfg != nil {
		for _, id := range slices.Sorted(slices.Values(cfg.Channels.Discord.AllowedUserIDs)) {
			if !owners.IsOwnerIdentity(principal.ChannelDiscord, id) {
				add(id, principal.Namespaced(principal.ChannelDiscord, id))
			}
		}
	}
	return moves, nil
}

func ownerRekeyLines(report store.OwnerRekeyReport, apply bool) []string {
	header := "dry-run: nothing written; rerun with --apply to move"
	if apply {
		header = "applied"
	}
	lines := []string{
		header,
		fmt.Sprintf("session: %d", report.Sessions),
		fmt.Sprintf("memory_document: %d", report.MemoryDocuments),
		fmt.Sprintf("profile_fact: %d", report.ProfileFacts),
	}
	if report.ProfileFactConflicts > 0 {
		lines = append(lines, fmt.Sprintf("profile_fact conflicts left in place: %d (review with aura profile list)", report.ProfileFactConflicts))
	}
	return lines
}

func recordOwnerRekey(ctx context.Context, db *sql.DB, report store.OwnerRekeyReport, at time.Time) error {
	sessionID, err := ensureOwnerActionSession(ctx, db, principal.OwnerID)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(struct {
		Sessions             int64  `json:"sessions"`
		MemoryDocuments      int64  `json:"memory_documents"`
		ProfileFacts         int64  `json:"profile_facts"`
		ProfileFactConflicts int64  `json:"profile_fact_conflicts"`
		At                   string `json:"at"`
	}{
		Sessions:             report.Sessions,
		MemoryDocuments:      report.MemoryDocuments,
		ProfileFacts:         report.ProfileFacts,
		ProfileFactConflicts: report.ProfileFactConflicts,
		At:                   at.Format(time.RFC3339Nano),
	})
	if err != nil {
		return fmt.Errorf("cli: marshal owner rekey: %w", err)
	}
	_, _, err = store.NewEventStore(db).UpsertEvent(ctx, &store.RuntimeEvent{
		ID:            "principal-rekey-" + at.Format(time.RFC3339Nano),
		SessionID:     sessionID,
		TurnID:        "owner-action",
		InvocationID:  "owner-action",
		Author:        principal.OwnerID,
		Kind:          ownerRekeyEventKind,
		SchemaVersion: 1,
		Payload:       payload,
		CreatedAt:     at,
	})
	return err
}
