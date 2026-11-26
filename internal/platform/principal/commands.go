package principal

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

	"github.com/anggasct/aura/internal/platform/clikit"
	"github.com/anggasct/aura/internal/platform/fault"
)

type CommandMode int

const (
	CommandModeConfig CommandMode = iota
	CommandModeStorage
	CommandModeForgetApply
)

type PersonaChecker interface {
	Exists(personaID string) bool
}

type PrincipalActivity struct {
	PrincipalID string
	Sessions    int
	LastActive  time.Time
}

type PrincipalActivityReader interface {
	PrincipalActivity(ctx context.Context) ([]PrincipalActivity, error)
}

type AllowlistedPrincipal struct {
	PrincipalID string
	Account     string
}

type LiveRunCanceller interface {
	CancelForPrincipal(ctx context.Context, principalID string) ([]string, error)
}

type BlobCollector func(ctx context.Context, db *sql.DB, artifactRoot string, before time.Time) (int, error)

type CommandEnv struct {
	Logger          *slog.Logger
	DB              *sql.DB
	Owners          *Resolver
	Actions         OwnerActionRecorder
	Personas        PersonaChecker
	PersonaNotFound func(personaID string) error
	Activity        PrincipalActivityReader
	Allowlisted     []AllowlistedPrincipal
	Pairings        PairingStore
	LiveRuns        LiveRunCanceller
	Erasers         []Eraser
	Rekeyers        []Rekeyer
	RekeyMoves      func(osUser string) ([]OwnerMove, error)
	ArtifactRoot    string
	CollectBlobs    BlobCollector
}

type CommandOpener func(cmd *cobra.Command, mode CommandMode, fn func(ctx context.Context, env *CommandEnv) error) error

func Commands(open CommandOpener) []*cobra.Command {
	return []*cobra.Command{
		newGrantsCmd(open),
		newAPIKeyCmd(open),
		newPairingCmd(open),
		newPrincipalCmd(open),
		newOwnerCmd(open),
	}
}

func runWithOpener(cmd *cobra.Command, open CommandOpener, mode CommandMode, fn func(ctx context.Context, env *CommandEnv) error) error {
	if open == nil {
		return fault.New(ErrorCodeInvalidArgument, "principal command opener must not be nil")
	}
	return open(cmd, mode, func(ctx context.Context, env *CommandEnv) error {
		if env == nil {
			return fault.New(ErrorCodeInvalidArgument, "principal command environment must not be nil")
		}
		if mode != CommandModeConfig && env.DB == nil {
			return fault.New(ErrorCodeInvalidArgument, "principal command requires an open database")
		}
		if env.Logger == nil {
			env.Logger = slog.New(slog.DiscardHandler)
		}
		return fn(ctx, env)
	})
}

type PrincipalSummary struct {
	ID         string
	Class      Class
	Access     []string
	Sessions   int
	LastActive time.Time
}

func CollectPrincipals(ctx context.Context, db *sql.DB, allowlisted []AllowlistedPrincipal, activity PrincipalActivityReader) (map[string]*PrincipalSummary, error) {
	if db == nil {
		return nil, errNilArgument("database")
	}
	summaries := map[string]*PrincipalSummary{}
	entry := func(id string) *PrincipalSummary {
		summary, ok := summaries[id]
		if !ok {
			summary = &PrincipalSummary{ID: id, Class: ClassPublic}
			summaries[id] = summary
		}
		return summary
	}
	for _, item := range allowlisted {
		summary := entry(item.PrincipalID)
		summary.Class = ClassTrusted
		summary.Access = append(summary.Access, "allowlist:"+item.Account)
	}
	grants, err := NewPrincipalGrantStore(db).List(ctx)
	if err != nil {
		return nil, err
	}
	for i := range grants {
		summary := entry(grants[i].PrincipalID)
		summary.Class = ClassTrusted
		summary.Access = append(summary.Access, "grant:"+grants[i].Account)
	}
	keys, err := NewAPIKeyStore(db).List(ctx)
	if err != nil {
		return nil, err
	}
	for i := range keys {
		key := &keys[i]
		class, ok := APIKeyClass(key.Class)
		if !ok || class == ClassOwner {
			continue
		}
		summary := entry(Namespaced(ChannelHTTP, key.ID))
		summary.Class = class
		summary.Access = append(summary.Access, keyAccess(key))
	}
	if activity != nil {
		activities, err := activity.PrincipalActivity(ctx)
		if err != nil {
			return nil, err
		}
		for i := range activities {
			summary := entry(activities[i].PrincipalID)
			summary.Sessions = activities[i].Sessions
			summary.LastActive = activities[i].LastActive
		}
	}
	delete(summaries, OwnerID)
	for id := range summaries {
		channel, _, _ := strings.Cut(id, ":")
		if channel != ChannelDiscord && channel != ChannelTelegram && channel != ChannelHTTP && channel != ChannelEmail {
			delete(summaries, id)
		}
	}
	return summaries, nil
}

func keyAccess(key *APIKey) string {
	if key.Active() {
		return "key:" + key.PersonaID
	}
	return "revoked-key:" + key.PersonaID
}

func FormatLastActive(at time.Time) string {
	if at.IsZero() {
		return "-"
	}
	return at.UTC().Format(time.RFC3339)
}

func FormatAccess(access []string) string {
	if len(access) == 0 {
		return "-"
	}
	return strings.Join(access, ",")
}

func APIKeyClass(raw string) (Class, bool) {
	switch raw {
	case string(ClassOwner):
		return ClassOwner, true
	case string(ClassTrusted):
		return ClassTrusted, true
	case string(ClassPublic):
		return ClassPublic, true
	default:
		return "", false
	}
}

func newPrincipalCmd(open CommandOpener) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "principal",
		Short: "Inspect people who talk to your personas",
	}
	cmd.AddCommand(newPrincipalListCmd(open), newPrincipalShowCmd(open), newPrincipalForgetCmd(open))
	return cmd
}

func newPrincipalListCmd(open CommandOpener) *cobra.Command {
	var class string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List trusted and public principals",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if class != "" && class != string(ClassTrusted) && class != string(ClassPublic) {
				return clikit.NewUsageError(errors.New("class must be trusted or public"))
			}
			return runWithOpener(cmd, open, CommandModeStorage, func(ctx context.Context, env *CommandEnv) error {
				summaries, err := CollectPrincipals(ctx, env.DB, env.Allowlisted, env.Activity)
				if err != nil {
					return err
				}
				lines := []string{"PRINCIPAL\tCLASS\tACCESS\tSESSIONS\tLAST_ACTIVE"}
				for _, id := range slices.Sorted(maps.Keys(summaries)) {
					summary := summaries[id]
					if class != "" && string(summary.Class) != class {
						continue
					}
					lines = append(lines, fmt.Sprintf("%s\t%s\t%s\t%d\t%s", summary.ID, summary.Class, FormatAccess(summary.Access), summary.Sessions, FormatLastActive(summary.LastActive)))
				}
				if len(lines) == 1 {
					return clikit.WriteLines(cmd, []string{"no principals"})
				}
				return clikit.WriteLines(cmd, lines)
			})
		},
	}
	cmd.Flags().StringVar(&class, "class", "", "only trusted or public principals")
	return cmd
}

func newPrincipalShowCmd(open CommandOpener) *cobra.Command {
	return &cobra.Command{
		Use:   "show <id>",
		Short: "Show one principal's access and activity",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWithOpener(cmd, open, CommandModeStorage, func(ctx context.Context, env *CommandEnv) error {
				if env.Pairings == nil {
					return errNilArgument("pairing store")
				}
				summaries, err := CollectPrincipals(ctx, env.DB, env.Allowlisted, env.Activity)
				if err != nil {
					return err
				}
				summary, ok := summaries[args[0]]
				if !ok {
					return clikit.NewExitCodeError(1, errors.New("principal_not_found: no trusted or public principal has this id"))
				}
				standing, err := NewStandingGrantStore(env.DB).List(ctx, summary.ID, time.Now().UTC())
				if err != nil {
					return err
				}
				pending, err := env.Pairings.Pending(ctx, time.Now().UTC())
				if err != nil {
					return err
				}
				pairing := 0
				for i := range pending {
					if pending[i].PrincipalID == summary.ID {
						pairing++
					}
				}
				return clikit.WriteLines(cmd, []string{
					"principal: " + summary.ID,
					"class: " + string(summary.Class),
					"access: " + FormatAccess(summary.Access),
					fmt.Sprintf("standing_grants: %d", len(standing)),
					fmt.Sprintf("pending_pairing: %d", pairing),
					fmt.Sprintf("sessions: %d", summary.Sessions),
					"last_active: " + FormatLastActive(summary.LastActive),
				})
			})
		},
	}
}
