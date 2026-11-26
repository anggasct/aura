package app

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
	"sync/atomic"
	"time"

	"github.com/anggasct/aura/internal/channels/discord"
	"github.com/anggasct/aura/internal/channels/telegram"
	"github.com/anggasct/aura/internal/platform/command"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/turn"
	turnstore "github.com/anggasct/aura/internal/platform/turn/store"
)

type sessionActivity interface {
	CancelActive(ctx context.Context, sessionID string) (turnID string, cancelled bool, err error)
	SessionActivity(sessionID string) (activeTurnID string, queued int)
}

type deferredCommandTurns struct {
	engine atomic.Pointer[sessionActivity]
}

func (t *deferredCommandTurns) bind(engine sessionActivity) {
	if t != nil && engine != nil {
		t.engine.Store(&engine)
	}
}

func (t *deferredCommandTurns) CancelActive(ctx context.Context, sessionID string) (turnID string, cancelled bool, err error) {
	var engine *sessionActivity
	if t != nil {
		engine = t.engine.Load()
	}
	if engine == nil {
		return "", false, command.Errorf(command.ErrorCodeUnavailable, "turn engine is not ready")
	}
	return (*engine).CancelActive(ctx, sessionID)
}

func (t *deferredCommandTurns) Activity(sessionID string) (activeTurnID string, queued int) {
	var engine *sessionActivity
	if t != nil {
		engine = t.engine.Load()
	}
	if engine == nil {
		return "", 0
	}
	return (*engine).SessionActivity(sessionID)
}

var errCommandSkillsUnavailable = errors.New("skill support is not available")

type deferredCommandSkills struct {
	skills atomic.Pointer[command.Skills]
}

func (s *deferredCommandSkills) bind(skills command.Skills) {
	if s != nil && skills != nil {
		s.skills.Store(&skills)
	}
}

func (s *deferredCommandSkills) Activate(ctx context.Context, name string) (command.SkillPin, error) {
	var skills *command.Skills
	if s != nil {
		skills = s.skills.Load()
	}
	if skills == nil {
		return command.SkillPin{}, errCommandSkillsUnavailable
	}
	return (*skills).Activate(ctx, name)
}

func (s *deferredCommandSkills) StageDraft(ctx context.Context, name, body string) (command.Draft, error) {
	var skills *command.Skills
	if s != nil {
		skills = s.skills.Load()
	}
	if skills == nil {
		return command.Draft{}, errCommandSkillsUnavailable
	}
	return (*skills).StageDraft(ctx, name, body)
}

type sequenceReader interface {
	LastSequence(ctx context.Context, sessionID string) (uint64, error)
}

type discordCommandSessions struct {
	sessions turnstore.SessionService
	events   sequenceReader
	pointers turnstore.ConversationSessionStore
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
	sess := turn.Session{
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
	if id == key {
		if err := s.pointers.Clear(ctx, key); err != nil {
			return command.Session{}, err
		}
		return command.Session{ID: key, OwnerID: caller.PrincipalID, AgentID: caller.AgentID}, nil
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
	err = s.pointers.Point(ctx, key, id)
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
	code, ok := fault.CodeOf[turnstore.ErrorCode](err)
	return errors.Is(err, sql.ErrNoRows) || (ok && code == turnstore.ErrorCodeSessionNotFound)
}

func newChatCommandDispatcher(sessions turnstore.SessionService, events sequenceReader, pointers turnstore.ConversationSessionStore, turns command.Turns, skills command.Skills, recorder command.Recorder) (*command.Dispatcher, error) {
	registry, err := command.New(discord.CommandBinding(), telegram.CommandBinding())
	if err != nil {
		return nil, err
	}
	return command.NewDispatcher(registry, command.Ports{
		Sessions: &discordCommandSessions{sessions: sessions, events: events, pointers: pointers},
		Turns:    turns,
		Skills:   skills,
	}, recorder)
}
