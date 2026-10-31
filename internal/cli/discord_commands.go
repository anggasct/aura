package cli

import (
	"context"
	"errors"
	"math"

	"github.com/anggasct/aura/internal/channel/discord"
	"github.com/anggasct/aura/internal/command"
	runtimechannelhost "github.com/anggasct/aura/internal/runtime/channelhost"
	"github.com/anggasct/aura/internal/store"
)

type sessionActivity interface {
	CancelActive(ctx context.Context, sessionID string) (turnID string, cancelled bool, err error)
	SessionActivity(sessionID string) (activeTurnID string, queued int)
}

type engineCommandTurns struct {
	engine sessionActivity
}

func (t engineCommandTurns) CancelActive(ctx context.Context, sessionID string) (turnID string, cancelled bool, err error) {
	return t.engine.CancelActive(ctx, sessionID)
}

func (t engineCommandTurns) Activity(sessionID string) (activeTurnID string, queued int) {
	return t.engine.SessionActivity(sessionID)
}

type sequenceReader interface {
	LastSequence(ctx context.Context, sessionID string) (uint64, error)
}

type discordCommandSessions struct {
	sessions store.SessionService
	events   sequenceReader
}

func (s *discordCommandSessions) EventCount(ctx context.Context, sessionID string) (int, error) {
	last, err := s.events.LastSequence(ctx, sessionID)
	if err != nil {
		return 0, err
	}
	if last > math.MaxInt32 {
		return math.MaxInt32, nil
	}
	return int(last), nil
}

func (s *discordCommandSessions) Create(context.Context, *command.Caller) (command.Session, error) {
	return command.Session{}, errors.New("new conversation sessions are not available")
}

func (s *discordCommandSessions) Switch(ctx context.Context, caller *command.Caller, id string) (command.Session, error) {
	if id != caller.ConversationKey {
		return command.Session{}, command.Errorf(command.ErrorCodeNotSwitchable, "outside this conversation")
	}
	sess, err := s.sessions.Get(ctx, id)
	if err != nil {
		return command.Session{}, err
	}
	if sess.OwnerID != caller.PrincipalID || sess.AgentID != caller.AgentID {
		return command.Session{}, command.Errorf(command.ErrorCodeNotSwitchable, "owned by another principal or persona")
	}
	return command.Session{ID: sess.ID, OwnerID: sess.OwnerID, AgentID: sess.AgentID}, nil
}

func wireDiscordCommands(ports []runtimechannelhost.ChannelPort, engine sessionActivity, sessions store.SessionService, events sequenceReader, recorder command.Recorder) error {
	registry, err := command.New(discord.CommandBinding())
	if err != nil {
		return err
	}
	dispatcher, err := command.NewDispatcher(registry, command.Ports{
		Sessions: &discordCommandSessions{sessions: sessions, events: events},
		Turns:    engineCommandTurns{engine: engine},
	}, recorder)
	if err != nil {
		return err
	}
	for _, port := range ports {
		if adapter, ok := port.(*discord.Adapter); ok {
			adapter.SetCommands(dispatcher)
		}
	}
	return nil
}
