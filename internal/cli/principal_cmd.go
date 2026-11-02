package cli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/principal"
	"github.com/anggasct/aura/internal/store"
)

type principalSummary struct {
	id         string
	class      principal.Class
	access     []string
	sessions   int
	lastActive time.Time
}

func collectPrincipals(ctx context.Context, cfg *config.Config, db *sql.DB) (map[string]*principalSummary, error) {
	owners, err := ownerResolver(cfg)
	if err != nil {
		return nil, err
	}
	summaries := map[string]*principalSummary{}
	entry := func(id string) *principalSummary {
		summary, ok := summaries[id]
		if !ok {
			summary = &principalSummary{id: id, class: principal.ClassPublic}
			summaries[id] = summary
		}
		return summary
	}
	for i := range cfg.Channels.Discord.Accounts {
		account := &cfg.Channels.Discord.Accounts[i]
		for _, userID := range account.AllowedUserIDs {
			if owners.IsOwnerIdentity(principal.ChannelDiscord, userID) {
				continue
			}
			summary := entry(principal.Namespaced(principal.ChannelDiscord, userID))
			summary.class = principal.ClassTrusted
			summary.access = append(summary.access, "allowlist:"+account.Name)
		}
	}
	grants, err := store.NewPrincipalGrantStore(db).List(ctx)
	if err != nil {
		return nil, err
	}
	for i := range grants {
		summary := entry(grants[i].PrincipalID)
		summary.class = principal.ClassTrusted
		summary.access = append(summary.access, "grant:"+grants[i].Account)
	}
	activities, err := store.ListPrincipalActivity(ctx, db)
	if err != nil {
		return nil, err
	}
	for i := range activities {
		summary := entry(activities[i].PrincipalID)
		summary.sessions = activities[i].Sessions
		summary.lastActive = activities[i].LastActive
	}
	delete(summaries, principal.OwnerID)
	for id := range summaries {
		if !strings.HasPrefix(id, principal.ChannelDiscord+":") {
			delete(summaries, id)
		}
	}
	return summaries, nil
}

func formatLastActive(at time.Time) string {
	if at.IsZero() {
		return "-"
	}
	return at.UTC().Format(time.RFC3339)
}

func formatAccess(access []string) string {
	if len(access) == 0 {
		return "-"
	}
	return strings.Join(access, ",")
}

func newPrincipalCmd(gf *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "principal",
		Short: "Inspect people who talk to your personas",
	}
	cmd.AddCommand(newPrincipalListCmd(gf), newPrincipalShowCmd(gf), newPrincipalForgetCmd(gf))
	return cmd
}

func newPrincipalListCmd(gf *globalFlags) *cobra.Command {
	var class string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List trusted and public principals",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if class != "" && class != string(principal.ClassTrusted) && class != string(principal.ClassPublic) {
				return &usageError{err: errors.New("class must be trusted or public")}
			}
			return withStorage(cmd, gf, true, func(ctx context.Context, _ *slog.Logger, cfg *config.Config, db *sql.DB) error {
				summaries, err := collectPrincipals(ctx, cfg, db)
				if err != nil {
					return err
				}
				lines := []string{"PRINCIPAL\tCLASS\tACCESS\tSESSIONS\tLAST_ACTIVE"}
				for _, id := range slices.Sorted(maps.Keys(summaries)) {
					summary := summaries[id]
					if class != "" && string(summary.class) != class {
						continue
					}
					lines = append(lines, fmt.Sprintf("%s\t%s\t%s\t%d\t%s", summary.id, summary.class, formatAccess(summary.access), summary.sessions, formatLastActive(summary.lastActive)))
				}
				if len(lines) == 1 {
					return writeChildLines(cmd, []string{"no principals"})
				}
				return writeChildLines(cmd, lines)
			})
		},
	}
	cmd.Flags().StringVar(&class, "class", "", "only trusted or public principals")
	return cmd
}

func newPrincipalShowCmd(gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "show <id>",
		Short: "Show one principal's access and activity",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withStorage(cmd, gf, true, func(ctx context.Context, _ *slog.Logger, cfg *config.Config, db *sql.DB) error {
				summaries, err := collectPrincipals(ctx, cfg, db)
				if err != nil {
					return err
				}
				summary, ok := summaries[args[0]]
				if !ok {
					return &exitCodeError{code: 1, err: errors.New("principal_not_found: no trusted or public principal has this id")}
				}
				standing, err := store.NewStandingGrantStore(db).List(ctx, summary.id, time.Now().UTC())
				if err != nil {
					return err
				}
				pending, err := store.NewPairingStore(db).Pending(ctx, time.Now().UTC())
				if err != nil {
					return err
				}
				pairing := 0
				for i := range pending {
					if pending[i].PrincipalID == summary.id {
						pairing++
					}
				}
				return writeChildLines(cmd, []string{
					"principal: " + summary.id,
					"class: " + string(summary.class),
					"access: " + formatAccess(summary.access),
					fmt.Sprintf("standing_grants: %d", len(standing)),
					fmt.Sprintf("pending_pairing: %d", pairing),
					fmt.Sprintf("sessions: %d", summary.sessions),
					"last_active: " + formatLastActive(summary.lastActive),
				})
			})
		},
	}
}
