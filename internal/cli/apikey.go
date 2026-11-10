package cli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/channels/httpchat"
	"github.com/anggasct/aura/internal/config"
	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/store"
)

const (
	accessActionAPIKeyCreate = "apikey_create"
	accessActionAPIKeyRevoke = "apikey_revoke"
	apiKeyLabelMaxLength     = 80
)

type apiKeyCreateFlags struct {
	persona      string
	class        string
	label        string
	confirmOwner bool
}

func newAPIKeyCmd(gf *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "apikey",
		Short: "Manage API keys for the HTTP chat channel",
	}
	cmd.AddCommand(newAPIKeyCreateCmd(gf), newAPIKeyListCmd(gf), newAPIKeyRevokeCmd(gf))
	return cmd
}

func newAPIKeyCreateCmd(gf *globalFlags) *cobra.Command {
	flags := &apiKeyCreateFlags{}
	cmd := &cobra.Command{
		Use:   "create --persona <id> --class trusted|public|owner --label <text> [--confirm-owner]",
		Short: "Create an API key and print it once",
		Args:  noPositionalArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := flags.validate(); err != nil {
				return err
			}
			return withStorage(cmd, gf, true, func(ctx context.Context, _ *slog.Logger, cfg *config.Config, db *sql.DB) error {
				return createAPIKey(ctx, cmd, cfg, db, flags)
			})
		},
	}
	f := cmd.Flags()
	f.StringVar(&flags.persona, "persona", "", "persona the key talks to")
	f.StringVar(&flags.class, "class", "", "principal class of the key: trusted, public or owner")
	f.StringVar(&flags.label, "label", "", "operator note that identifies the key")
	f.BoolVar(&flags.confirmOwner, "confirm-owner", false, "acknowledge that an owner key bypasses every guest gate")
	return cmd
}

func (f *apiKeyCreateFlags) validate() error {
	if strings.TrimSpace(f.persona) == "" {
		return &usageError{err: errors.New("apikey create requires --persona")}
	}
	if strings.TrimSpace(f.label) == "" {
		return &usageError{err: errors.New("apikey create requires --label")}
	}
	if utf8.RuneCountInString(f.label) > apiKeyLabelMaxLength || strings.ContainsFunc(f.label, unicode.IsControl) {
		return &usageError{err: fmt.Errorf("apikey create --label must be %d characters or fewer without control characters", apiKeyLabelMaxLength)}
	}
	if _, ok := apiKeyClass(f.class); !ok {
		return &usageError{err: errors.New("apikey create requires --class trusted, public or owner")}
	}
	if f.class == store.APIKeyClassOwner && !f.confirmOwner {
		return &usageError{err: errors.New("apikey create --class owner requires --confirm-owner")}
	}
	if f.class != store.APIKeyClassOwner && f.confirmOwner {
		return &usageError{err: errors.New("apikey create --confirm-owner only applies to --class owner")}
	}
	return nil
}

func createAPIKey(ctx context.Context, cmd *cobra.Command, cfg *config.Config, db *sql.DB, flags *apiKeyCreateFlags) error {
	registry, err := buildAgentRegistry(cfg)
	if err != nil {
		return err
	}
	if _, ok := registry.Lookup(flags.persona); !ok {
		return &auraagent.Error{Code: auraagent.ErrorCodeNotFound, Detail: "agent " + flags.persona + " is not registered"}
	}
	issued, err := httpchat.NewKey()
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	record := &store.APIKey{
		ID: issued.ID, KeySHA256: issued.Digest, PersonaID: flags.persona, Class: flags.class,
		Label: strings.TrimSpace(flags.label), CreatedAt: now,
	}
	if err := store.NewAPIKeyStore(db).Create(ctx, record); err != nil {
		return err
	}
	if err := recordAccessAction(ctx, db, apiKeyAction(accessActionAPIKeyCreate, record), issued.ID, now); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "created api key %s; store it now, it is not shown again\n", issued.ID); err != nil {
		return err
	}
	return writeChildLines(cmd, []string{issued.Plaintext})
}

func apiKeyAction(action string, key *store.APIKey) *accessAction {
	return &accessAction{
		Action:    action,
		Principal: principal.Namespaced(principal.ChannelHTTP, key.ID),
		Persona:   key.PersonaID,
		Channel:   principal.ChannelHTTP,
	}
}

func newAPIKeyListCmd(gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List API keys without their secrets",
		Args:  noPositionalArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withStorage(cmd, gf, true, func(ctx context.Context, _ *slog.Logger, _ *config.Config, db *sql.DB) error {
				keys, err := store.NewAPIKeyStore(db).List(ctx)
				if err != nil {
					return err
				}
				if len(keys) == 0 {
					return writeChildLines(cmd, []string{"no api keys"})
				}
				lines := []string{"ID\tPERSONA\tCLASS\tLABEL\tCREATED\tREVOKED"}
				for i := range keys {
					lines = append(lines, fmt.Sprintf("%s\t%s\t%s\t%s\t%s\t%s", keys[i].ID, keys[i].PersonaID, keys[i].Class, keys[i].Label,
						keys[i].CreatedAt.UTC().Format(time.RFC3339), formatLastActive(keys[i].RevokedAt)))
				}
				return writeChildLines(cmd, lines)
			})
		},
	}
}

func newAPIKeyRevokeCmd(gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "revoke <key_id>",
		Short: "Revoke an API key",
		Args:  exactlyOneKeyID,
		RunE: func(cmd *cobra.Command, args []string) error {
			return withStorage(cmd, gf, true, func(ctx context.Context, _ *slog.Logger, _ *config.Config, db *sql.DB) error {
				keys := store.NewAPIKeyStore(db)
				now := time.Now().UTC()
				changed, err := keys.Revoke(ctx, args[0], now)
				if err != nil {
					return err
				}
				if !changed {
					return writeChildLines(cmd, []string{"already revoked " + args[0]})
				}
				key, err := keys.GetByID(ctx, args[0])
				if err != nil {
					return err
				}
				if err := recordAccessAction(ctx, db, apiKeyAction(accessActionAPIKeyRevoke, &key), key.ID, now); err != nil {
					return err
				}
				return writeChildLines(cmd, []string{"revoked " + args[0]})
			})
		},
	}
}

func exactlyOneKeyID(cmd *cobra.Command, args []string) error {
	if len(args) != 1 {
		return &usageError{fmt.Errorf("%s requires exactly one key id", cmd.CommandPath())}
	}
	return nil
}

func apiKeyClass(raw string) (principal.Class, bool) {
	switch raw {
	case store.APIKeyClassOwner:
		return principal.ClassOwner, true
	case store.APIKeyClassTrusted:
		return principal.ClassTrusted, true
	case store.APIKeyClassPublic:
		return principal.ClassPublic, true
	default:
		return "", false
	}
}
