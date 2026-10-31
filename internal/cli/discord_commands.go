package cli

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

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
	pointers store.ConversationSessionStore
	now      func() time.Time
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

func (s *discordCommandSessions) Create(ctx context.Context, caller *command.Caller) (command.Session, error) {
	if caller.ConversationKey == "" {
		return command.Session{}, errors.New("conversation key must not be empty")
	}
	suffix, err := conversationSessionSuffix()
	if err != nil {
		return command.Session{}, err
	}
	now := s.clock()
	sess := store.Session{
		ID:        caller.ConversationKey + conversationSessionSeparator + suffix,
		OwnerID:   caller.PrincipalID,
		AgentID:   caller.AgentID,
		Metadata:  json.RawMessage(`{}`),
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.sessions.Create(ctx, &sess); err != nil {
		return command.Session{}, err
	}
	if err := s.pointers.Point(ctx, caller.ConversationKey, sess.ID); err != nil {
		return command.Session{}, err
	}
	return command.Session{ID: sess.ID, OwnerID: sess.OwnerID, AgentID: sess.AgentID}, nil
}

func (s *discordCommandSessions) Switch(ctx context.Context, caller *command.Caller, id string) (command.Session, error) {
	key := caller.ConversationKey
	if key == "" || (id != key && !strings.HasPrefix(id, key+conversationSessionSeparator)) {
		return command.Session{}, command.Errorf(command.ErrorCodeNotSwitchable, "outside this conversation")
	}
	sess, err := s.sessions.Get(ctx, id)
	if err != nil {
		if missingSession(err) {
			return command.Session{}, command.Errorf(command.ErrorCodeNotSwitchable, "not found")
		}
		return command.Session{}, err
	}
	if sess.OwnerID != caller.PrincipalID || sess.AgentID != caller.AgentID {
		return command.Session{}, command.Errorf(command.ErrorCodeNotSwitchable, "owned by another principal or persona")
	}
	if id == key {
		err = s.pointers.Clear(ctx, key)
	} else {
		err = s.pointers.Point(ctx, key, id)
	}
	if err != nil {
		if missingSession(err) {
			return command.Session{}, command.Errorf(command.ErrorCodeNotSwitchable, "not found")
		}
		return command.Session{}, err
	}
	return command.Session{ID: sess.ID, OwnerID: sess.OwnerID, AgentID: sess.AgentID}, nil
}

func (s *discordCommandSessions) clock() time.Time {
	if s.now != nil {
		return s.now().UTC()
	}
	return time.Now().UTC()
}

const conversationSessionSeparator = "~"

func conversationSessionSuffix() (string, error) {
	var raw [10]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("conversation session suffix: %w", err)
	}
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw[:])), nil
}

func missingSession(err error) bool {
	code, ok := store.CodeOf(err)
	return errors.Is(err, sql.ErrNoRows) || (ok && code == store.ErrorCodeSessionNotFound)
}

type discordConversationResolver struct {
	pointers store.ConversationSessionStore
}

func (r *discordConversationResolver) ActiveSession(ctx context.Context, conversationKey string) (string, error) {
	id, found, err := r.pointers.Resolve(ctx, conversationKey)
	if err != nil {
		return "", err
	}
	if !found {
		return conversationKey, nil
	}
	return id, nil
}

func wireDiscordCommands(ports []runtimechannelhost.ChannelPort, engine sessionActivity, sessions store.SessionService, events sequenceReader, pointers store.ConversationSessionStore, recorder command.Recorder) error {
	registry, err := command.New(discord.CommandBinding())
	if err != nil {
		return err
	}
	dispatcher, err := command.NewDispatcher(registry, command.Ports{
		Sessions: &discordCommandSessions{sessions: sessions, events: events, pointers: pointers},
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
