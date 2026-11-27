package httpchat

import (
	"log/slog"
	"time"

	"github.com/anggasct/aura/internal/channels/kit/access"
	"github.com/anggasct/aura/internal/channels/kit/approval"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/turn"
)

type Deps struct {
	Config        *Config
	Keys          KeyStore
	Sessions      SessionStore
	Replies       HandoffReplySource
	Personas      Personas
	Runtime       turn.AgentRuntime
	Access        access.Settings
	Approvals     *approval.OwnerRoute
	AnswerTexts   func(events []turn.Event) []string
	ShutdownGrace time.Duration
	Logger        *slog.Logger
}

func New(deps *Deps) (*Server, error) {
	if deps == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "http chat deps must not be nil")
	}
	if deps.Config == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "http chat config must not be nil")
	}
	chat, err := NewChat(&ChatOptions{
		Runtime:            deps.Runtime,
		Sessions:           deps.Sessions,
		Personas:           deps.Personas,
		Budgets:            deps.Access.Budgets,
		Evals:              deps.Access.Evals,
		Handoffs:           deps.Access.Handoffs,
		Approvals:          deps.Approvals,
		Denied:             deps.Access.Denied,
		AnswerTexts:        deps.AnswerTexts,
		MaxHistoryMessages: deps.Config.MaxHistoryMessages,
		MaxHistoryBytes:    deps.Config.MaxHistoryBytes,
		KeepaliveInterval:  time.Duration(deps.Config.KeepaliveInterval),
		Logger:             deps.Logger,
	})
	if err != nil {
		return nil, err
	}
	replies, err := NewReplies(&RepliesOptions{
		Sessions: deps.Sessions,
		Handoffs: deps.Access.Handoffs,
		Replies:  deps.Replies,
		Logger:   deps.Logger,
	})
	if err != nil {
		return nil, err
	}
	handler, err := NewHandler(&Options{
		Keys:                deps.Keys,
		Chat:                chat,
		Replies:             replies,
		MaxBodyBytes:        deps.Config.MaxBodyBytes,
		RequestsPerMinute:   deps.Config.RequestsPerMinute,
		MaxConcurrentPerKey: deps.Config.MaxConcurrentPerKey,
		Logger:              deps.Logger,
	})
	if err != nil {
		return nil, err
	}
	return NewServer(handler, deps.Config.ListenAddress, deps.ShutdownGrace, deps.Logger)
}
