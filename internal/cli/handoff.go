package cli

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/anggasct/aura/internal/channel/discord"
	"github.com/anggasct/aura/internal/config"
	runtimechannelhost "github.com/anggasct/aura/internal/runtime/channelhost"
	"github.com/anggasct/aura/internal/store"
	toolsbuiltin "github.com/anggasct/aura/internal/tools/builtin"
)

const (
	accessActionHandoffReply   = "handoff_reply"
	accessActionHandoffRelease = "handoff_release"
	handoffReplyAuthorFallback = "aura"
)

var deliverText = discord.DeliverText

type handoffNotifier interface {
	NotifyHandoff(ctx context.Context, sessionID, reason string) (discord.HandoffTarget, error)
}

type handoffService struct {
	handoffs  store.HandoffStore
	notifiers []handoffNotifier
	now       func() time.Time
}

func newHandoffService(db *sql.DB, notifiers []handoffNotifier) *handoffService {
	return &handoffService{handoffs: store.NewHandoffStore(db), notifiers: notifiers, now: time.Now}
}

func (s *handoffService) OpenHandoff(ctx context.Context, request *toolsbuiltin.HandoffRequest) error {
	for _, notifier := range s.notifiers {
		target, err := notifier.NotifyHandoff(ctx, request.SessionID, request.Reason)
		if errors.Is(err, discord.ErrNoHandoffTarget) {
			continue
		}
		if err != nil {
			return err
		}
		return s.handoffs.Open(ctx, &store.Handoff{
			SessionID: request.SessionID, Reason: request.Reason, PrincipalID: request.PrincipalID, AgentID: request.AgentID,
			Account: target.Account, ReplyChannelID: target.ChannelID, OpenedAt: s.now().UTC(),
		})
	}
	return errors.New("handoff: no channel knows where this session's requester is")
}

type handoffGate struct {
	handoffs store.HandoffStore
	timeout  time.Duration
	now      func() time.Time
}

func (g *handoffGate) Check(ctx context.Context, sessionID string) (string, error) {
	state, _, err := g.handoffs.Check(ctx, sessionID, g.timeout, g.now().UTC())
	if err != nil {
		return discord.HandoffStateNone, err
	}
	switch state {
	case store.HandoffOpen:
		return discord.HandoffStateOpen, nil
	case store.HandoffExpired:
		return discord.HandoffStateExpired, nil
	default:
		return discord.HandoffStateNone, nil
	}
}

func handoffNotifiers(ports []runtimechannelhost.ChannelPort) []handoffNotifier {
	var notifiers []handoffNotifier
	for _, port := range ports {
		if notifier, ok := port.(handoffNotifier); ok {
			notifiers = append(notifiers, notifier)
		}
	}
	return notifiers
}

func discordAccount(cfg *config.Config, name string) (*config.DiscordAccount, bool) {
	for i := range cfg.Channels.Discord.Accounts {
		if cfg.Channels.Discord.Accounts[i].Name == name {
			return &cfg.Channels.Discord.Accounts[i], true
		}
	}
	return nil, false
}

func openHandoff(ctx context.Context, db *sql.DB, sessionID string) (store.Handoff, error) {
	handoff, found, err := store.NewHandoffStore(db).Get(ctx, sessionID)
	if err != nil {
		return store.Handoff{}, err
	}
	if !found || !handoff.ReleasedAt.IsZero() {
		return store.Handoff{}, &exitCodeError{code: 1, err: errors.New("handoff_not_open: this session has no open handoff")}
	}
	return handoff, nil
}

func appendPersonaReply(ctx context.Context, db *sql.DB, handoff *store.Handoff, author, text string, at time.Time) error {
	id := fmt.Sprintf("handoff-reply-%d", at.UnixNano())
	event, err := store.RuntimeEventFromADK(handoff.SessionID, id, sessionEventWithText(id, author, text, at))
	if err != nil {
		return err
	}
	_, err = store.NewEventStore(db).AppendSequenced(ctx, handoff.SessionID, &event)
	return err
}

func sessionEventWithText(id, author, text string, at time.Time) *session.Event {
	event := &session.Event{ID: id, InvocationID: id, Author: author, Timestamp: at}
	event.Content = genai.NewContentFromText(text, genai.RoleModel)
	return event
}

func newReplyCmd(gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "reply <session> <text>",
		Short: "Reply as the persona in a handed-off session",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			text := strings.TrimSpace(strings.Join(args[1:], " "))
			if text == "" {
				return &usageError{err: errors.New("reply text must not be empty")}
			}
			return withStorage(cmd, gf, true, func(ctx context.Context, _ *slog.Logger, cfg *config.Config, db *sql.DB) error {
				handoff, err := openHandoff(ctx, db, args[0])
				if err != nil {
					return err
				}
				account, ok := discordAccount(cfg, handoff.Account)
				if !ok {
					return &exitCodeError{code: 1, err: fmt.Errorf("handoff account %q is not configured", handoff.Account)}
				}
				if err := deliverText(ctx, account, handoff.ReplyChannelID, text); err != nil {
					return err
				}
				now := time.Now().UTC()
				registry, err := buildAgentRegistry(cfg)
				if err != nil {
					return err
				}
				definition, _ := registry.Lookup(handoff.AgentID)
				if err := appendPersonaReply(ctx, db, &handoff, cmp.Or(definition.Name, handoffReplyAuthorFallback), text, now); err != nil {
					return err
				}
				if err := recordAccessAction(ctx, db, &accessAction{Action: accessActionHandoffReply, Principal: handoff.PrincipalID, Persona: handoff.AgentID, Account: handoff.Account}, "reply-"+handoff.SessionID+"-"+now.Format(time.RFC3339Nano), now); err != nil {
					return err
				}
				return writeChildLines(cmd, []string{"replied in " + handoff.SessionID})
			})
		},
	}
}

func newHandoffCmd(gf *globalFlags) *cobra.Command {
	cmd := &cobra.Command{Use: "handoff", Short: "Inspect and release sessions handed off to the owner"}
	cmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List open handoffs",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withStorage(cmd, gf, true, func(ctx context.Context, _ *slog.Logger, _ *config.Config, db *sql.DB) error {
				open, err := store.NewHandoffStore(db).ListOpen(ctx)
				if err != nil {
					return err
				}
				if len(open) == 0 {
					return writeChildLines(cmd, []string{"no open handoffs"})
				}
				lines := []string{"SESSION\tPRINCIPAL\tPERSONA\tOPENED\tREASON"}
				for i := range open {
					lines = append(lines, fmt.Sprintf("%s\t%s\t%s\t%s\t%s", open[i].SessionID, open[i].PrincipalID, open[i].AgentID, open[i].OpenedAt.UTC().Format(time.RFC3339), open[i].Reason))
				}
				return writeChildLines(cmd, lines)
			})
		},
	}, &cobra.Command{
		Use:   "release <session>",
		Short: "Hand the conversation back to the persona",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withStorage(cmd, gf, true, func(ctx context.Context, logger *slog.Logger, cfg *config.Config, db *sql.DB) error {
				if _, err := openHandoff(ctx, db, args[0]); err != nil {
					return err
				}
				now := time.Now().UTC()
				handoff, released, err := store.NewHandoffStore(db).Release(ctx, args[0], now)
				if err != nil {
					return err
				}
				if !released {
					return &exitCodeError{code: 1, err: errors.New("handoff_not_open: this session has no open handoff")}
				}
				registry, err := buildAgentRegistry(cfg)
				if err != nil {
					return err
				}
				definition, _ := registry.Lookup(handoff.AgentID)
				if account, ok := discordAccount(cfg, handoff.Account); ok {
					if err := deliverText(ctx, account, handoff.ReplyChannelID, definition.Message(config.MessageHandoffClosed)); err != nil {
						logger.WarnContext(ctx, "handoff release notice failed", "component", "access")
					}
				}
				if err := recordAccessAction(ctx, db, &accessAction{Action: accessActionHandoffRelease, Principal: handoff.PrincipalID, Persona: handoff.AgentID, Account: handoff.Account}, "release-"+handoff.SessionID+"-"+now.Format(time.RFC3339Nano), now); err != nil {
					return err
				}
				return writeChildLines(cmd, []string{"released " + handoff.SessionID})
			})
		},
	})
	return cmd
}
