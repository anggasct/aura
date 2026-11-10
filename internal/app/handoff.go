package app

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

	"github.com/anggasct/aura/internal/channels/discord"
	"github.com/anggasct/aura/internal/channels/email"
	"github.com/anggasct/aura/internal/channels/kit/access"
	"github.com/anggasct/aura/internal/channels/telegram"
	"github.com/anggasct/aura/internal/config"
	toolsbuiltin "github.com/anggasct/aura/internal/modules/builtin"
	runtimechannelhost "github.com/anggasct/aura/internal/platform/turn/channelhost"
	"github.com/anggasct/aura/internal/store"
)

const (
	accessActionHandoffReply   = "handoff_reply"
	accessActionHandoffRelease = "handoff_release"
	handoffReplyAuthorFallback = "aura"
)

var (
	deliverText         = discord.DeliverText
	deliverTelegramText = telegram.DeliverText
	deliverEmailText    = email.DeliverText
)

type handoffNotifier interface {
	NotifyHandoff(ctx context.Context, sessionID, reason string) (access.HandoffTarget, error)
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
		if errors.Is(err, access.ErrNoHandoffTarget) {
			continue
		}
		if err != nil {
			return err
		}
		return s.handoffs.Open(ctx, &store.Handoff{
			SessionID: request.SessionID, Reason: request.Reason, PrincipalID: request.PrincipalID, AgentID: request.AgentID,
			Account: target.Account, Channel: target.Channel, ReplyChannelID: target.ChannelID, ReplyThreadID: target.ThreadID, OpenedAt: s.now().UTC(),
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
		return access.HandoffStateNone, err
	}
	switch state {
	case store.HandoffOpen:
		return access.HandoffStateOpen, nil
	case store.HandoffExpired:
		return access.HandoffStateExpired, nil
	default:
		return access.HandoffStateNone, nil
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

func telegramAccount(cfg *config.Config, name string) (*config.TelegramAccount, bool) {
	for i := range cfg.Channels.Telegram.Accounts {
		if cfg.Channels.Telegram.Accounts[i].Name == name {
			return &cfg.Channels.Telegram.Accounts[i], true
		}
	}
	return nil, false
}

func emailAccount(cfg *config.Config, name string) (*config.EmailAccount, bool) {
	for i := range cfg.Channels.Email.Accounts {
		if cfg.Channels.Email.Accounts[i].Name == name {
			return &cfg.Channels.Email.Accounts[i], true
		}
	}
	return nil, false
}

func deliverToHandoff(ctx context.Context, cfg *config.Config, handoff *store.Handoff, text string) error {
	switch handoff.Channel {
	case store.HandoffChannelTelegram:
		account, ok := telegramAccount(cfg, handoff.Account)
		if !ok {
			return &exitCodeError{code: 1, err: fmt.Errorf("handoff account %q is not configured", handoff.Account)}
		}
		return deliverTelegramText(ctx, account, handoff.ReplyChannelID, handoff.ReplyThreadID, text)
	case store.HandoffChannelHTTP:
		return nil
	case store.HandoffChannelEmail:
		account, ok := emailAccount(cfg, handoff.Account)
		if !ok {
			return &exitCodeError{code: 1, err: fmt.Errorf("handoff account %q is not configured", handoff.Account)}
		}
		return deliverEmailText(ctx, account, &email.TextReply{To: handoff.ReplyChannelID, Thread: email.ParseThreadReference(handoff.ReplyThreadID), Text: text})
	case store.HandoffChannelDiscord:
		account, ok := discordAccount(cfg, handoff.Account)
		if !ok {
			return &exitCodeError{code: 1, err: fmt.Errorf("handoff account %q is not configured", handoff.Account)}
		}
		return deliverText(ctx, account, handoff.ReplyChannelID, text)
	default:
		return &exitCodeError{code: 1, err: fmt.Errorf("handoff channel %q is not supported", handoff.Channel)}
	}
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
	return appendSessionText(ctx, db, handoff.SessionID, store.HandoffReplyEventID(at), author, text, at)
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
				if err := deliverToHandoff(ctx, cfg, &handoff, text); err != nil {
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
				if err := deliverToHandoff(ctx, cfg, &handoff, definition.Message(config.MessageHandoffClosed)); err != nil {
					logger.WarnContext(ctx, "handoff release notice failed", "component", "access")
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
