package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	auraagent "github.com/anggasct/aura/internal/agent"
	"github.com/anggasct/aura/internal/config"
)

const exitNoPersona = 3

type bindingScopeFlags struct {
	channel   string
	account   string
	guildID   string
	channelID string
	threadID  string
	userID    string
}

func newBindingsCmd(gf *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "bindings",
		Short: "Inspect persona bindings",
	}
	cmd.AddCommand(newBindingsExplainCmd(gf))
	return cmd
}

func newBindingsExplainCmd(gf *globalFlags) *cobra.Command {
	scope := &bindingScopeFlags{}
	cmd := &cobra.Command{
		Use:   "explain",
		Short: "Show which persona a channel scope routes to",
		Args:  noPositionalArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if scope.channel != config.BindingChannelDiscord {
				return &usageError{fmt.Errorf("%s: --channel %q is not supported; only %q", cmd.CommandPath(), scope.channel, config.BindingChannelDiscord)}
			}
			if scope.account == "" {
				return &usageError{fmt.Errorf("%s requires --account", cmd.CommandPath())}
			}
			result, err := config.Load(gf.configPath)
			if err != nil {
				return err
			}
			registry, err := buildAgentRegistry(result.Config)
			if err != nil {
				return err
			}
			router, err := auraagent.NewRouter(registry, &result.Config.Channels.Discord, result.Config.Bindings)
			if err != nil {
				return err
			}
			if !router.HasAccount(scope.channel, scope.account) {
				return &usageError{fmt.Errorf("%s: account %q is not configured under channels.%s.accounts", cmd.CommandPath(), scope.account, scope.channel)}
			}
			match, ok := router.Resolve(&auraagent.RouteScope{
				Channel:   scope.channel,
				Account:   scope.account,
				GuildID:   scope.guildID,
				ChannelID: scope.channelID,
				ThreadID:  scope.threadID,
				UserID:    scope.userID,
			})
			out := cmd.OutOrStdout()
			if !ok {
				if _, err := fmt.Fprintln(out, "no persona"); err != nil {
					return err
				}
				return &exitCodeError{code: exitNoPersona, err: errors.New("no binding or account default matches the scope")}
			}
			if _, err := fmt.Fprintf(out, "persona: %s\nscore: %d\nbinding: %s\n", match.Persona, match.Score, bindingLabel(scope, result.Config.Bindings, match.Binding)); err != nil {
				return err
			}
			return cmd.Context().Err()
		},
	}
	flags := cmd.Flags()
	flags.StringVar(&scope.channel, "channel", config.BindingChannelDiscord, "channel the message arrives on")
	flags.StringVar(&scope.account, "account", "", "channel account name")
	flags.StringVar(&scope.guildID, "guild", "", "guild id")
	flags.StringVar(&scope.channelID, "channel-id", "", "channel id (the parent channel for thread messages)")
	flags.StringVar(&scope.threadID, "thread", "", "thread id")
	flags.StringVar(&scope.userID, "user", "", "author user id")
	return cmd
}

func bindingLabel(scope *bindingScopeFlags, bindings []config.Binding, index int) string {
	if index == auraagent.AccountDefaultBinding || index < 0 || index >= len(bindings) {
		return fmt.Sprintf("account default (%s:%s)", scope.channel, scope.account)
	}
	binding := &bindings[index]
	fields := []string{fmt.Sprintf("bindings[%d]", index)}
	for _, field := range []struct{ name, value string }{
		{"guild_id", binding.GuildID},
		{"channel_id", binding.ChannelID},
		{"thread_id", binding.ThreadID},
		{"user_id", binding.UserID},
	} {
		if field.value != "" {
			fields = append(fields, field.name+"="+field.value)
		}
	}
	return strings.Join(fields, " ")
}
