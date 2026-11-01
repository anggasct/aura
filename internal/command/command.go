package command

import (
	"context"
	"errors"
	"fmt"

	"github.com/anggasct/aura/internal/principal"
	runtimeingress "github.com/anggasct/aura/internal/runtime/ingress"
)

const ChannelTerminal = "terminal"

type Audience string

const (
	AudienceOwner Audience = "owner"
	AudienceAny   Audience = "any"
)

type Kind string

const (
	KindLocal        Kind = "local"
	KindTurn         Kind = "turn"
	KindTerminalOnly Kind = "terminal_only"
)

type Control string

const (
	ControlNone  Control = ""
	ControlExit  Control = "exit"
	ControlClear Control = "clear"
)

const Variadic = -1

type Definition struct {
	Name     string
	Aliases  []string
	Usage    string
	Summary  string
	Audience Audience
	Kind     Kind
	Session  bool
	MinArgs  int
	MaxArgs  int
	Prompt   string
	Control  Control
}

type Style string

const (
	StyleTerminal  Style = "terminal"
	StyleMessenger Style = "messenger"
)

type Note struct {
	Syntax  string
	Summary string
}

type Binding struct {
	Channel     string
	Style       Style
	Interactive bool
	Names       []string
	Notes       []Note
}

type ErrorCode string

const (
	ErrorCodeOK               ErrorCode = ""
	ErrorCodeUnknown          ErrorCode = "command_unknown"
	ErrorCodeUsage            ErrorCode = "command_usage"
	ErrorCodeDenied           ErrorCode = "command_denied"
	ErrorCodeUnavailable      ErrorCode = "command_unavailable"
	ErrorCodeScope            ErrorCode = "command_scope"
	ErrorCodeNotSwitchable    ErrorCode = "session_not_switchable"
	ErrorCodeSkillUnavailable ErrorCode = "skill_unavailable"
	ErrorCodeFailed           ErrorCode = "command_failed"
	ErrorCodeInvalidArgument  ErrorCode = "invalid_argument"
)

type Error struct {
	Code   ErrorCode
	Detail string
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Detail)
}

func CodeOf(err error) (ErrorCode, bool) {
	var target *Error
	if !errors.As(err, &target) {
		return "", false
	}
	return target.Code, true
}

func Errorf(code ErrorCode, format string, args ...any) error {
	return &Error{Code: code, Detail: fmt.Sprintf(format, args...)}
}

type Caller struct {
	Channel         string
	PrincipalID     string
	Class           principal.Class
	ConversationKey string
	SessionID       string
	AgentID         string
}

type Invocation struct {
	Name  string
	Token string
	Args  []string
	Text  string
}

type Session struct {
	ID      string
	OwnerID string
	AgentID string
}

type Result struct {
	Lines      []string
	Diagnostic bool
	Code       ErrorCode
	Control    Control
	Session    *Session
	Pending    *Invocation
	Turn       *TurnStart
}

type SkillPin = runtimeingress.SkillPin

type TurnStart struct {
	Text  string
	Skill *SkillPin
}

type Draft struct {
	ID     string
	Digest string
}

type Sessions interface {
	EventCount(ctx context.Context, sessionID string) (int, error)
	Create(ctx context.Context, caller *Caller) (Session, error)
	Switch(ctx context.Context, caller *Caller, id string) (Session, error)
}

type Turns interface {
	CancelActive(ctx context.Context, sessionID string) (turnID string, cancelled bool, err error)
	Activity(sessionID string) (activeTurnID string, queued int)
}

type Skills interface {
	Activate(ctx context.Context, name string) (SkillPin, error)
	StageDraft(ctx context.Context, name, description string) (Draft, error)
}

type Ports struct {
	Sessions Sessions
	Turns    Turns
	Skills   Skills
}

type Recorder interface {
	Start(ctx context.Context, name, channel, class string) (spanCtx context.Context, finish func(result string))
}

func Catalog() []Definition {
	return []Definition{
		{Name: "help", Summary: "show this help", Audience: AudienceAny, Kind: KindLocal},
		{Name: "status", Summary: "show the current session", Audience: AudienceOwner, Kind: KindLocal, Session: true},
		{Name: "session", Usage: "[id]", Summary: "show or switch to a session", Audience: AudienceOwner, Kind: KindLocal, Session: true, MaxArgs: 1},
		{Name: "new", Summary: "start a new session", Audience: AudienceOwner, Kind: KindLocal, Session: true},
		{Name: "cancel", Summary: "cancel the active turn", Audience: AudienceOwner, Kind: KindLocal, Session: true},
		{Name: "skill", Usage: "<name> [request…]", Summary: "activate a skill", Audience: AudienceOwner, Kind: KindTurn, Session: true, MinArgs: 1, MaxArgs: Variadic},
		{Name: "skill-create", Usage: "<name> <description…>", Summary: "stage a skill draft", Audience: AudienceOwner, Kind: KindLocal, MinArgs: 2, MaxArgs: Variadic, Prompt: "describe the skill in one line:"},
		{Name: "exit", Aliases: []string{"quit"}, Summary: "leave the console", Audience: AudienceOwner, Kind: KindTerminalOnly, Control: ControlExit},
		{Name: "clear", Summary: "clear the screen", Audience: AudienceOwner, Kind: KindTerminalOnly, Control: ControlClear},
	}
}
