package command

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/anggasct/aura/internal/platform/effect"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/principal"
	runtimeingress "github.com/anggasct/aura/internal/platform/turn/ingress"
)

const (
	ChannelTerminal = "terminal"
	NameForgetMe    = "forgetme"
	ArgConfirm      = "confirm"
)

type Audience string

const (
	AudienceOwner  Audience = "owner"
	AudienceAny    Audience = "any"
	AudienceGuests Audience = "guests"
)

type Kind string

const (
	KindLocal        Kind = "local"
	KindTurn         Kind = "turn"
	KindTerminalOnly Kind = "terminal_only"
	KindOperation    Kind = "operation"
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
	ErrorCodeConflict         ErrorCode = "command_conflict"
	ErrorCodeInvalidArgument  ErrorCode = "invalid_argument"

	ErrorCodeTooLong          ErrorCode = "command_too_long"
	ErrorCodeTooManyArguments ErrorCode = "command_too_many_arguments"
	ErrorCodeOpenQuote        ErrorCode = "command_open_quote"
	ErrorCodeDanglingEscape   ErrorCode = "command_dangling_escape"
	ErrorCodeMissingToken     ErrorCode = "command_missing_token"
)

func Errorf(code ErrorCode, format string, args ...any) error {
	return fault.New(code, fmt.Sprintf(format, args...))
}

type Caller struct {
	Channel         string
	PrincipalID     string
	Class           principal.Class
	ConversationKey string
	SessionID       string
	AgentID         string
	Direct          bool
}

type Invocation struct {
	Name           string
	Token          string
	Args           []string
	Text           string
	IdempotencyKey string
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
	Record     json.RawMessage
	Fate       OperationFate
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
	CancelTurn(ctx context.Context, sessionID, turnID string) (cancelled bool, err error)
	Activity(sessionID string) (activeTurnID string, queued int)
}

type Skills interface {
	Activate(ctx context.Context, name string) (SkillPin, error)
	StageDraft(ctx context.Context, name, description string) (Draft, error)
}

type OperationPrompt struct {
	Text  string
	Flag  string
	Usage string
}

type OperationPlan struct {
	Operation string
	Input     json.RawMessage
	Apply     bool
	Code      string
	Missing   *OperationPrompt
	Refusal   *Result
}

type OperationRef struct {
	ID    string
	Input json.RawMessage
}

type OperationRecovery struct {
	Provider effect.TakeoverProvider
	Settle   func(lines []string, record json.RawMessage) (effect.Outcome, error)
}

type ResumeRequest struct {
	Caller     *Caller
	Invocation *Invocation
	Intent     *effect.Intent
	Recovery   *OperationRecovery
}

type OperationRun struct {
	Caller         *Caller
	Plan           *OperationPlan
	IdempotencyKey string
	Intent         *effect.Intent
	Recovery       *OperationRecovery
}

type OperationFate string

const (
	FatePerformed    OperationFate = ""
	FateNotPerformed OperationFate = "not_performed"
	FateAmbiguous    OperationFate = "ambiguous"
)

type OperationReply struct {
	Lines      []string
	Code       ErrorCode
	Fate       OperationFate
	Record     json.RawMessage
	InProgress bool
}

type Operations interface {
	Plan(group string, tokens []string) OperationPlan
	Run(ctx context.Context, run *OperationRun) OperationReply
}

type Ports struct {
	Sessions   Sessions
	Turns      Turns
	Skills     Skills
	Operations Operations
}

type Recorder interface {
	Start(ctx context.Context, name, channel, class string) (spanCtx context.Context, finish func(result string))
}

func MessengerNames() []string {
	return []string{"help", NameForgetMe, "status", "session", "new", "cancel", "skill", "skill-create"}
}

func Catalog() []Definition {
	return []Definition{
		{Name: "help", Summary: "show this help", Audience: AudienceAny, Kind: KindLocal},
		{Name: NameForgetMe, Usage: "[confirm]", Summary: "erase everything you shared with this bot", Audience: AudienceGuests, Kind: KindLocal, MaxArgs: 1},
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
