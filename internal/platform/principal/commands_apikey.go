package principal

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/platform/clikit"
	"github.com/anggasct/aura/internal/platform/fault"
)

const APIKeyLabelMaxLength = 80

type apiKeyCreateFlags struct {
	persona      string
	class        string
	label        string
	confirmOwner bool
}

func newAPIKeyCmd(open CommandOpener) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "apikey",
		Short: "Manage API keys for the HTTP chat channel",
	}
	cmd.AddCommand(newAPIKeyCreateCmd(open), newAPIKeyListCmd(open), newAPIKeyRevokeCmd(open))
	return cmd
}

func newAPIKeyCreateCmd(open CommandOpener) *cobra.Command {
	flags := &apiKeyCreateFlags{}
	cmd := &cobra.Command{
		Use:   "create --persona <id> --class trusted|public|owner --label <text> [--confirm-owner]",
		Short: "Create an API key and print it once",
		Args:  clikit.NoPositionalArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := flags.validate(); err != nil {
				return err
			}
			return runWithOpener(cmd, open, CommandModeStorage, func(ctx context.Context, env *CommandEnv) error {
				return createAPIKey(ctx, cmd, env, flags)
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
		return clikit.NewUsageError(errors.New("apikey create requires --persona"))
	}
	if strings.TrimSpace(f.label) == "" {
		return clikit.NewUsageError(errors.New("apikey create requires --label"))
	}
	if utf8.RuneCountInString(f.label) > APIKeyLabelMaxLength || strings.ContainsFunc(f.label, unicode.IsControl) {
		return clikit.NewUsageError(fmt.Errorf("apikey create --label must be %d characters or fewer without control characters", APIKeyLabelMaxLength))
	}
	if _, ok := APIKeyClass(f.class); !ok {
		return clikit.NewUsageError(errors.New("apikey create requires --class trusted, public or owner"))
	}
	if f.class == string(ClassOwner) && !f.confirmOwner {
		return clikit.NewUsageError(errors.New("apikey create --class owner requires --confirm-owner"))
	}
	if f.class != string(ClassOwner) && f.confirmOwner {
		return clikit.NewUsageError(errors.New("apikey create --confirm-owner only applies to --class owner"))
	}
	return nil
}

func createAPIKey(ctx context.Context, cmd *cobra.Command, env *CommandEnv, flags *apiKeyCreateFlags) error {
	if env.Personas == nil {
		return errNilArgument("persona checker")
	}
	if !env.Personas.Exists(flags.persona) {
		if env.PersonaNotFound != nil {
			return env.PersonaNotFound(flags.persona)
		}
		return fault.New(ErrorCodeAPIKeyInvalid, "agent "+flags.persona+" is not registered")
	}
	issued, err := NewKey()
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	record := &APIKey{
		ID: issued.ID, KeySHA256: issued.Digest, PersonaID: flags.persona, Class: flags.class,
		Label: strings.TrimSpace(flags.label), CreatedAt: now,
	}
	if err := NewAPIKeyStore(env.DB).Create(ctx, record); err != nil {
		return err
	}
	if env.Actions != nil {
		if err := env.Actions.RecordOwnerAction(ctx, apiKeyAction(AccessActionAPIKeyCreate, record), issued.ID, now); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "created api key %s; store it now, it is not shown again\n", issued.ID); err != nil {
		return err
	}
	return clikit.WriteLines(cmd, []string{issued.Plaintext})
}

func apiKeyAction(action string, key *APIKey) *OwnerAction {
	return &OwnerAction{
		Action:    action,
		Principal: Namespaced(ChannelHTTP, key.ID),
		Persona:   key.PersonaID,
		Channel:   ChannelHTTP,
	}
}

func newAPIKeyListCmd(open CommandOpener) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List API keys without their secrets",
		Args:  clikit.NoPositionalArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runWithOpener(cmd, open, CommandModeStorage, func(ctx context.Context, env *CommandEnv) error {
				keys, err := NewAPIKeyStore(env.DB).List(ctx)
				if err != nil {
					return err
				}
				if len(keys) == 0 {
					return clikit.WriteLines(cmd, []string{"no api keys"})
				}
				lines := []string{"ID\tPERSONA\tCLASS\tLABEL\tCREATED\tREVOKED"}
				for i := range keys {
					lines = append(lines, fmt.Sprintf("%s\t%s\t%s\t%s\t%s\t%s", keys[i].ID, keys[i].PersonaID, keys[i].Class, keys[i].Label,
						keys[i].CreatedAt.UTC().Format(time.RFC3339), FormatLastActive(keys[i].RevokedAt)))
				}
				return clikit.WriteLines(cmd, lines)
			})
		},
	}
}

func newAPIKeyRevokeCmd(open CommandOpener) *cobra.Command {
	return &cobra.Command{
		Use:   "revoke <key_id>",
		Short: "Revoke an API key",
		Args:  exactlyOneKeyID,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWithOpener(cmd, open, CommandModeStorage, func(ctx context.Context, env *CommandEnv) error {
				keys := NewAPIKeyStore(env.DB)
				now := time.Now().UTC()
				changed, err := keys.Revoke(ctx, args[0], now)
				if err != nil {
					return err
				}
				if !changed {
					return clikit.WriteLines(cmd, []string{"already revoked " + args[0]})
				}
				key, err := keys.GetByID(ctx, args[0])
				if err != nil {
					return err
				}
				if env.Actions != nil {
					if err := env.Actions.RecordOwnerAction(ctx, apiKeyAction(AccessActionAPIKeyRevoke, &key), key.ID, now); err != nil {
						return err
					}
				}
				return clikit.WriteLines(cmd, []string{"revoked " + args[0]})
			})
		},
	}
}

func exactlyOneKeyID(cmd *cobra.Command, args []string) error {
	if len(args) != 1 {
		return clikit.NewUsageError(fmt.Errorf("%s requires exactly one key id", cmd.CommandPath()))
	}
	return nil
}
