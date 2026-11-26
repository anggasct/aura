package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/platform/clikit"
	"github.com/anggasct/aura/internal/platform/fault"
)

const (
	ExitNoPersona           = 3
	soulDigestDisplayLength = 12
)

type CommandEnv struct {
	Registry *Registry
	Accounts []ChannelAccount
	Bindings []Binding
}

type CommandOpener func(cmd *cobra.Command, fn func(ctx context.Context, env *CommandEnv) error) error

func Commands(open CommandOpener, personaSubcommands ...*cobra.Command) []*cobra.Command {
	return []*cobra.Command{
		newAgentsCmd(open),
		newPersonaCmd(open, personaSubcommands...),
		newBindingsCmd(open),
	}
}

func runWithOpener(cmd *cobra.Command, open CommandOpener, fn func(ctx context.Context, env *CommandEnv) error) error {
	if open == nil {
		return fault.New(ErrorCodeInvalidArgument, "agent command opener must not be nil")
	}
	return open(cmd, func(ctx context.Context, env *CommandEnv) error {
		if env == nil || env.Registry == nil {
			return fault.New(ErrorCodeInvalidArgument, "agent command environment requires a registry")
		}
		return fn(ctx, env)
	})
}

func newAgentsCmd(open CommandOpener) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "agents",
		Short: "Inspect registered agent definitions",
	}
	cmd.AddCommand(newAgentsListCmd(open), newAgentsShowCmd(open))
	return cmd
}

func newAgentsListCmd(open CommandOpener) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List registered agent definitions",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runWithOpener(cmd, open, func(ctx context.Context, env *CommandEnv) error {
				out := cmd.OutOrStdout()
				write := func(format string, args ...any) error {
					_, werr := fmt.Fprintf(out, format, args...)
					return werr
				}
				if err := write("ID\tCAPABILITIES\tTOOLS\tMODEL ROUTE\n"); err != nil {
					return err
				}
				definitions := env.Registry.Definitions()
				for i := range definitions {
					definition := &definitions[i]
					if err := write("%s\t%s\t%s\t%s\n",
						definition.ID,
						strings.Join(definition.Capabilities, ","),
						strings.Join(definition.Tools, ","),
						routeLabel(definition.ModelRoute),
					); err != nil {
						return err
					}
				}
				return ctx.Err()
			})
		},
	}
}

func newAgentsShowCmd(open CommandOpener) *cobra.Command {
	return &cobra.Command{
		Use:   "show <id>",
		Short: "Show one agent definition",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWithOpener(cmd, open, func(_ context.Context, env *CommandEnv) error {
				definition, ok := env.Registry.Lookup(args[0])
				if !ok {
					return fault.New(ErrorCodeNotFound, "agent "+args[0]+" is not registered")
				}
				out := cmd.OutOrStdout()
				write := func(format string, args ...any) error {
					_, werr := fmt.Fprintf(out, format, args...)
					return werr
				}
				if err := write("ID: %s\n", definition.ID); err != nil {
					return err
				}
				if err := write("Description: %s\n", definition.Description); err != nil {
					return err
				}
				if err := write("Instructions: %s\n", definition.Instructions); err != nil {
					return err
				}
				if err := write("Model route: %s\n", routeLabel(definition.ModelRoute)); err != nil {
					return err
				}
				if err := write("Tools: %s\n", strings.Join(definition.Tools, ", ")); err != nil {
					return err
				}
				if err := write("Capabilities: %s\n", orNone(strings.Join(definition.Capabilities, ", "))); err != nil {
					return err
				}
				if len(definition.Plugins) > 0 {
					if err := write("Plugins: %s\n", strings.Join(definition.Plugins, ", ")); err != nil {
						return err
					}
				}
				if definition.Limits.TurnTimeout > 0 {
					return write("Turn timeout: %s\n", definition.Limits.TurnTimeout)
				}
				return write("Turn timeout: runtime default\n")
			})
		},
	}
}

func newPersonaCmd(open CommandOpener, extra ...*cobra.Command) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "persona",
		Short: "Inspect configured personas",
	}
	cmd.AddCommand(newPersonaListCmd(open))
	cmd.AddCommand(extra...)
	return cmd
}

func newPersonaListCmd(open CommandOpener) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List personas with their soul source and digest",
		Args:  clikit.NoPositionalArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runWithOpener(cmd, open, func(ctx context.Context, env *CommandEnv) error {
				tbl := clikit.NewTable(cmd.OutOrStdout())
				tbl.Printf("ID\tNAME\tSOUL\tSOUL SHA256\tMODEL ROUTE\n")
				definitions := env.Registry.Definitions()
				for i := range definitions {
					definition := &definitions[i]
					tbl.Printf("%s\t%s\t%s\t%s\t%s\n",
						definition.ID,
						definition.Name,
						soulSource(definition),
						definition.Fingerprint().SoulSHA256[:soulDigestDisplayLength],
						routeLabel(definition.ModelRoute),
					)
				}
				if err := tbl.Flush(); err != nil {
					return err
				}
				return ctx.Err()
			})
		},
	}
}

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

func newBindingsCmd(open CommandOpener) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "bindings",
		Short: "Inspect persona bindings",
	}
	cmd.AddCommand(newBindingsExplainCmd(open))
	return cmd
}

func newBindingsExplainCmd(open CommandOpener) *cobra.Command {
	scope := &bindingScopeFlags{}
	cmd := &cobra.Command{
		Use:   "explain",
		Short: "Show which persona a channel scope routes to",
		Args:  clikit.NoPositionalArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := scope.validate(cmd); err != nil {
				return err
			}
			if scope.account == "" {
				return clikit.NewUsageError(fmt.Errorf("%s requires --account", cmd.CommandPath()))
			}
			return runWithOpener(cmd, open, func(ctx context.Context, env *CommandEnv) error {
				router, err := NewRouter(env.Registry, env.Accounts, env.Bindings)
				if err != nil {
					return err
				}
				if !router.HasAccount(scope.channel, scope.account) {
					return clikit.NewUsageError(fmt.Errorf("%s: account %q is not configured under channels.%s.accounts", cmd.CommandPath(), scope.account, scope.channel))
				}
				match, ok := router.Resolve(scope.routeScope())
				out := cmd.OutOrStdout()
				if !ok {
					if _, err := fmt.Fprintln(out, "no persona"); err != nil {
						return err
					}
					return clikit.NewExitCodeError(ExitNoPersona, errors.New("no binding or account default matches the scope"))
				}
				if _, err := fmt.Fprintf(out, "persona: %s\nscore: %d\nbinding: %s\n", match.Persona, match.Score, bindingLabel(scope, env.Bindings, match.Binding)); err != nil {
					return err
				}
				return ctx.Err()
			})
		},
	}
	flags := cmd.Flags()
	flags.StringVar(&scope.channel, "channel", BindingChannelDiscord, "channel the message arrives on")
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
	case BindingChannelDiscord:
		foreign = changedFlags(cmd, "chat", "topic")
	case BindingChannelTelegram:
		foreign = changedFlags(cmd, "guild", "channel-id", "thread")
		if s.topicID != "" && s.chatID == "" {
			return clikit.NewUsageError(fmt.Errorf("%s: --topic requires --chat", cmd.CommandPath()))
		}
	default:
		return clikit.NewUsageError(fmt.Errorf("%s: --channel %q is not supported; use %q or %q", cmd.CommandPath(), s.channel, BindingChannelDiscord, BindingChannelTelegram))
	}
	if len(foreign) > 0 {
		return clikit.NewUsageError(fmt.Errorf("%s: --%s does not apply to --channel %s", cmd.CommandPath(), strings.Join(foreign, ", --"), s.channel))
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

func (s *bindingScopeFlags) routeScope() *RouteScope {
	if s.channel == BindingChannelTelegram {
		return &RouteScope{
			Channel:  s.channel,
			Account:  s.account,
			ChatID:   s.chatID,
			ThreadID: s.topicID,
			UserID:   s.userID,
		}
	}
	return &RouteScope{
		Channel:   s.channel,
		Account:   s.account,
		GuildID:   s.guildID,
		ChannelID: s.channelID,
		ThreadID:  s.threadID,
		UserID:    s.userID,
	}
}

func bindingLabel(scope *bindingScopeFlags, bindings []Binding, index int) string {
	if index == AccountDefaultBinding || index < 0 || index >= len(bindings) {
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

func routeLabel(route string) string {
	if route == "" {
		return "runtime default"
	}
	return route
}

func orNone(value string) string {
	if value == "" {
		return "(none)"
	}
	return value
}

func soulSource(definition *Definition) string {
	switch {
	case definition.SoulFile != "":
		return definition.SoulFile
	case definition.Instructions != "":
		return "inline"
	default:
		return "(none)"
	}
}
