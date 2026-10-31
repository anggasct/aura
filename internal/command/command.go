package command

import (
	"context"
	"fmt"

	"github.com/anggasct/aura/internal/principal"
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

type Code string

const (
	CodeOK               Code = ""
	CodeUnknown          Code = "command_unknown"
	CodeUsage            Code = "command_usage"
	CodeDenied           Code = "command_denied"
	CodeUnavailable      Code = "command_unavailable"
	CodeScope            Code = "command_scope"
	CodeNotSwitchable    Code = "session_not_switchable"
	CodeSkillUnavailable Code = "skill_unavailable"
	CodeFailed           Code = "command_failed"
)

type Error struct {
	Code   Code
	Detail string
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Detail)
}

func Errorf(code Code, format string, args ...any) error {
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
}

type Session struct {
	ID      string
	OwnerID string
	AgentID string
}

type Result struct {
	Lines      []string
	Diagnostic bool
	Code       Code
	Control    Control
	Session    *Session
	Pending    *Invocation
}

type Activation struct {
	SkillID string
	Caveat  string
	Text    string
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
	Activate(ctx context.Context, name string) (Activation, error)
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
