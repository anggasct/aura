package app

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	appconfig "github.com/anggasct/aura/internal/app/config"
	"github.com/anggasct/aura/internal/channels/discord"
	"github.com/anggasct/aura/internal/channels/email"
	"github.com/anggasct/aura/internal/channels/kit/access"
	"github.com/anggasct/aura/internal/channels/kit/approval"
	"github.com/anggasct/aura/internal/channels/telegram"
	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/modules/handoff"
	auraagent "github.com/anggasct/aura/internal/platform/agent"
	runtimechannelhost "github.com/anggasct/aura/internal/platform/turn/channelhost"
)

const (
	accessActionHandoffReply   = handoff.ActionReply
	accessActionHandoffRelease = handoff.ActionRelease
	handoffReplyAuthorFallback = "aura"
)

var (
	deliverText         = discord.DeliverText
	deliverTelegramText = telegram.DeliverText
	deliverEmailText    = email.DeliverText
)

type channelHandoffTarget interface {
	NotifyHandoff(ctx context.Context, sessionID, reason string) (access.HandoffTarget, error)
}

type channelHandoffNotifier struct {
	target channelHandoffTarget
}

func (n *channelHandoffNotifier) NotifyHandoff(ctx context.Context, sessionID, reason string) (handoff.Target, error) {
	target, err := n.target.NotifyHandoff(ctx, sessionID, reason)
	if errors.Is(err, access.ErrNoHandoffTarget) {
		return handoff.Target{}, handoff.ErrNoTarget
	}
	if err != nil {
		return handoff.Target{}, err
	}
	return handoff.Target{
		Channel:   target.Channel,
		Account:   target.Account,
		ChannelID: target.ChannelID,
		ThreadID:  target.ThreadID,
	}, nil
}

func handoffNotifiers(ports []runtimechannelhost.ChannelPort) []handoff.Notifier {
	var notifiers []handoff.Notifier
	for _, port := range ports {
		if target, ok := port.(channelHandoffTarget); ok {
			notifiers = append(notifiers, &channelHandoffNotifier{target: target})
		}
	}
	return notifiers
}

type ownerHandoffNotifier interface {
	personaServer
	NotifyOwnerHandoff(ctx context.Context, requester *approval.Requester, sessionID, reason string) error
}

type ownerHandoffAdapter struct {
	notifier ownerHandoffNotifier
}

func (a *ownerHandoffAdapter) ServesPersona(persona string) bool {
	return a.notifier.ServesPersona(persona)
}

func (a *ownerHandoffAdapter) NotifyOwnerHandoff(ctx context.Context, requester *handoff.OwnerRequester, sessionID, reason string) error {
	return a.notifier.NotifyOwnerHandoff(ctx, &approval.Requester{
		Principal: requester.Principal,
		Class:     requester.Class,
		Persona:   requester.Persona,
	}, sessionID, reason)
}

type emailOwnerHandoffRouter struct {
	router *handoff.OwnerRouter
}

func (r *emailOwnerHandoffRouter) NotifyOwner(ctx context.Context, requester *approval.Requester, sessionID, reason string) error {
	return r.router.NotifyOwner(ctx, &handoff.OwnerRequester{
		Principal: requester.Principal,
		Class:     requester.Class,
		Persona:   requester.Persona,
	}, sessionID, reason)
}

func newOwnerHandoffRouter(notifiers []ownerHandoffNotifier) *handoff.OwnerRouter {
	adapted := make([]handoff.OwnerNotifier, 0, len(notifiers))
	for _, notifier := range notifiers {
		adapted = append(adapted, &ownerHandoffAdapter{notifier: notifier})
	}
	return handoff.NewOwnerRouter(adapted)
}

type httpRequesters interface {
	RequesterOf(sessionID string) (approval.Requester, bool)
}

type httpRequesterLookupAdapter struct {
	requesters httpRequesters
}

func (a *httpRequesterLookupAdapter) RequesterOf(sessionID string) (handoff.OwnerRequester, bool) {
	req, ok := a.requesters.RequesterOf(sessionID)
	if !ok {
		return handoff.OwnerRequester{}, false
	}
	return handoff.OwnerRequester{Principal: req.Principal, Class: req.Class, Persona: req.Persona}, true
}

func newHTTPHandoffNotifier(requesters httpRequesters, adapters []approvalAdapter) (*handoff.HTTPNotifier, error) {
	notifiers := make([]ownerHandoffNotifier, 0, len(adapters))
	for _, adapter := range adapters {
		notifiers = append(notifiers, adapter)
	}
	return handoff.NewHTTPNotifier(&httpRequesterLookupAdapter{requesters: requesters}, newOwnerHandoffRouter(notifiers))
}

func discordAccount(cfg *appconfig.Config, name string) (*config.DiscordAccount, bool) {
	for i := range cfg.Channels.Discord.Accounts {
		if cfg.Channels.Discord.Accounts[i].Name == name {
			return &cfg.Channels.Discord.Accounts[i], true
		}
	}
	return nil, false
}

func telegramAccount(cfg *appconfig.Config, name string) (*config.TelegramAccount, bool) {
	for i := range cfg.Channels.Telegram.Accounts {
		if cfg.Channels.Telegram.Accounts[i].Name == name {
			return &cfg.Channels.Telegram.Accounts[i], true
		}
	}
	return nil, false
}

func emailAccount(cfg *appconfig.Config, name string) (*config.EmailAccount, bool) {
	for i := range cfg.Channels.Email.Accounts {
		if cfg.Channels.Email.Accounts[i].Name == name {
			return &cfg.Channels.Email.Accounts[i], true
		}
	}
	return nil, false
}

func deliverToHandoff(ctx context.Context, cfg *appconfig.Config, h *handoff.Handoff, text string) error {
	switch h.Channel {
	case handoff.ChannelTelegram:
		account, ok := telegramAccount(cfg, h.Account)
		if !ok {
			return &exitCodeError{code: 1, err: fmt.Errorf("handoff account %q is not configured", h.Account)}
		}
		return deliverTelegramText(ctx, account, h.ReplyChannelID, h.ReplyThreadID, text)
	case handoff.ChannelHTTP:
		return nil
	case handoff.ChannelEmail:
		account, ok := emailAccount(cfg, h.Account)
		if !ok {
			return &exitCodeError{code: 1, err: fmt.Errorf("handoff account %q is not configured", h.Account)}
		}
		return deliverEmailText(ctx, account, &email.TextReply{To: h.ReplyChannelID, Thread: email.ParseThreadReference(h.ReplyThreadID), Text: text})
	case handoff.ChannelDiscord:
		account, ok := discordAccount(cfg, h.Account)
		if !ok {
			return &exitCodeError{code: 1, err: fmt.Errorf("handoff account %q is not configured", h.Account)}
		}
		return deliverText(ctx, account, h.ReplyChannelID, text)
	default:
		return &exitCodeError{code: 1, err: fmt.Errorf("handoff channel %q is not supported", h.Channel)}
	}
}

func appendPersonaReply(ctx context.Context, db *sql.DB, h *handoff.Handoff, author, text string, at time.Time) error {
	return appendSessionText(ctx, db, h.SessionID, handoff.ReplyEventID(at), author, text, at)
}

func sessionEventWithText(id, author, text string, at time.Time) *session.Event {
	event := &session.Event{ID: id, InvocationID: id, Author: author, Timestamp: at}
	event.Content = genai.NewContentFromText(text, genai.RoleModel)
	return event
}

func handoffOpener(gf *globalFlags) handoff.CommandOpener {
	return func(cmd *cobra.Command, fn func(ctx context.Context, deps *handoff.CommandDeps) error) error {
		result, err := appconfig.Load(gf.configPath)
		if err != nil {
			return err
		}
		return withApp(cmd, &commandInput{result: &result}, func(ctx context.Context, app *App) error {
			store, err := handoff.NewSQLiteStore(app.db)
			if err != nil {
				return err
			}
			registry, err := buildAgentRegistry(result.Config)
			if err != nil {
				return err
			}
			return fn(ctx, &handoff.CommandDeps{
				Store: store,
				Deliver: func(ctx context.Context, h *handoff.Handoff, text string) error {
					return deliverToHandoff(ctx, result.Config, h, text)
				},
				PersonaAuthor: func(agentID string) string {
					definition, _ := registry.Lookup(agentID)
					return cmp.Or(definition.Name, handoffReplyAuthorFallback)
				},
				ClosedMessage: func(agentID string) string {
					definition, _ := registry.Lookup(agentID)
					return definition.Message(auraagent.MessageHandoffClosed)
				},
				AppendReply: func(ctx context.Context, h *handoff.Handoff, author, text string, at time.Time) error {
					return appendPersonaReply(ctx, app.db, h, author, text, at)
				},
				RecordAction: func(ctx context.Context, action string, h *handoff.Handoff, eventID string, at time.Time) error {
					return recordAccessAction(ctx, app.db, &accessAction{
						Action:    action,
						Principal: h.PrincipalID,
						Persona:   h.AgentID,
						Account:   h.Account,
					}, eventID, at)
				},
				Logger: app.logger,
			})
		})
	}
}
