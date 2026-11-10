package app

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/config"
	auraagent "github.com/anggasct/aura/internal/platform/agent"
)

const exitNoPersona = 3

type bindingScopeFlags struct {
	channel   string
	account   string
	guildID   string
	channelID string
	threadID  string
	userID    string
	chatID    string
	topicID   string
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
			if err := scope.validate(cmd); err != nil {
				return err
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
			router, err := auraagent.NewRouter(registry, &result.Config.Channels, result.Config.Bindings)
			if err != nil {
				return err
			}
			if !router.HasAccount(scope.channel, scope.account) {
				return &usageError{fmt.Errorf("%s: account %q is not configured under channels.%s.accounts", cmd.CommandPath(), scope.account, scope.channel)}
			}
			match, ok := router.Resolve(scope.routeScope())
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
	flags.StringVar(&scope.chatID, "chat", "", "telegram group chat id")
	flags.StringVar(&scope.topicID, "topic", "", "telegram forum topic id")
	return cmd
}

func (s *bindingScopeFlags) validate(cmd *cobra.Command) error {
	var foreign []string
	switch s.channel {
	case config.BindingChannelDiscord:
		foreign = changedFlags(cmd, "chat", "topic")
	case config.BindingChannelTelegram:
		foreign = changedFlags(cmd, "guild", "channel-id", "thread")
		if s.topicID != "" && s.chatID == "" {
			return &usageError{fmt.Errorf("%s: --topic requires --chat", cmd.CommandPath())}
		}
	default:
		return &usageError{fmt.Errorf("%s: --channel %q is not supported; use %q or %q", cmd.CommandPath(), s.channel, config.BindingChannelDiscord, config.BindingChannelTelegram)}
	}
	if len(foreign) > 0 {
		return &usageError{fmt.Errorf("%s: --%s does not apply to --channel %s", cmd.CommandPath(), strings.Join(foreign, ", --"), s.channel)}
	}
	return nil
}

func changedFlags(cmd *cobra.Command, names ...string) []string {
	var set []string
	for _, name := range names {
		if cmd.Flags().Changed(name) {
			set = append(set, name)
		}
	}
	return set
}

func (s *bindingScopeFlags) routeScope() *auraagent.RouteScope {
	if s.channel == config.BindingChannelTelegram {
		return &auraagent.RouteScope{
			Channel:  s.channel,
			Account:  s.account,
			ChatID:   s.chatID,
			ThreadID: s.topicID,
			UserID:   s.userID,
		}
	}
	return &auraagent.RouteScope{
		Channel:   s.channel,
		Account:   s.account,
		GuildID:   s.guildID,
		ChannelID: s.channelID,
		ThreadID:  s.threadID,
		UserID:    s.userID,
	}
}

func bindingLabel(scope *bindingScopeFlags, bindings []config.Binding, index int) string {
	if index == auraagent.AccountDefaultBinding || index < 0 || index >= len(bindings) {
		return fmt.Sprintf("account default (%s:%s)", scope.channel, scope.account)
	}
	binding := &bindings[index]
	fields := []string{fmt.Sprintf("bindings[%d]", index)}
	for _, field := range []struct{ name, value string }{
		{"guild_id", binding.GuildID},
		{"chat_id", binding.ChatID},
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
