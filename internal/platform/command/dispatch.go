package command

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/fault"
)

const (
	ResultOK               = "ok"
	ResultUsage            = "usage"
	ResultDenied           = "denied"
	ResultUnavailable      = "unavailable"
	ResultScope            = "scope"
	ResultNotSwitchable    = "not_switchable"
	ResultSkillUnavailable = "skill_unavailable"
	ResultFailed           = "failed"
	ResultReplayed         = "replayed"
)

const NameTerminalOnly = "terminal_only"

const (
	ReplyDenied        = "That command is only available to the owner."
	ReplyNotSwitchable = "That conversation can't be used here."
	ReplyGuestsOnly    = "That command is not available to the owner."
	ReplyForgetMe      = "Send /forgetme confirm to erase everything you shared with this bot."
	ReplyConflict      = "An earlier message with this id was already handled differently, so nothing was run."
)

type Dispatcher struct {
	registry *Registry
	ports    Ports
	recorder Recorder
}

func NewDispatcher(registry *Registry, ports *Ports, recorder Recorder) (*Dispatcher, error) {
	if registry == nil {
		return nil, Errorf(ErrorCodeInvalidArgument, "command: registry must not be nil")
	}
	if ports == nil {
		return nil, Errorf(ErrorCodeInvalidArgument, "command: ports must not be nil")
	}
	if ports.Sessions == nil {
		return nil, Errorf(ErrorCodeInvalidArgument, "command: sessions port must not be nil")
	}
	return &Dispatcher{registry: registry, ports: *ports, recorder: recorder}, nil
}

func (d *Dispatcher) Registry() *Registry { return d.registry }

func (d *Dispatcher) CancelTurn(ctx context.Context, sessionID, turnID string) (bool, error) {
	if d == nil || d.ports.Turns == nil {
		return false, nil
	}
	return d.ports.Turns.CancelTurn(ctx, sessionID, turnID)
}

func (d *Dispatcher) Execute(ctx context.Context, caller *Caller, inv *Invocation) Result {
	if caller == nil {
		return failure(ErrorCodeInvalidArgument, "aura: caller must not be nil")
	}
	if inv == nil {
		return failure(ErrorCodeInvalidArgument, "aura: invocation must not be nil")
	}
	b, ok := d.registry.binding(caller.Channel)
	if !ok {
		return unknown(inv)
	}
	style := b.binding.Style
	def, ok := d.registry.Lookup(inv.Name)
	if !ok {
		return d.unknownReply(caller, inv, style)
	}
	if def.Kind == KindTerminalOnly && style != StyleTerminal {
		_, finish := d.start(ctx, NameTerminalOnly, caller)
		finish(ResultUnavailable)
		return failure(ErrorCodeUnavailable, d.formatMessage(caller.AgentID, agent.MessageCommandUnavailable, agent.CommandPlaceholder, inv.Name))
	}
	if !b.names[def.Name] {
		return d.unknownReply(caller, inv, style)
	}
	ctx, finish := d.start(ctx, def.Name, caller)
	result := d.execute(ctx, caller, inv, &def, &b.binding)
	finish(ResultOf(result.Code))
	return result
}

func (d *Dispatcher) Replayed(ctx context.Context, caller *Caller, inv *Invocation) {
	if caller == nil || inv == nil {
		return
	}
	_, finish := d.start(ctx, d.registry.MetricName(caller.Channel, inv.Name), caller)
	finish(ResultReplayed)
}

func (d *Dispatcher) start(ctx context.Context, name string, caller *Caller) (spanCtx context.Context, finish func(string)) {
	if recorded, _ := ctx.Value(recordedKey{}).(bool); d.recorder == nil || recorded {
		return ctx, func(string) {}
	}
	return d.recorder.Start(ctx, name, caller.Channel, string(caller.Class))
}

func (d *Dispatcher) message(agentID, key string) string {
	if d.ports.Texts != nil && agentID != "" {
		if text := d.ports.Texts.Message(agentID, key); text != "" {
			return text
		}
	}
	return agent.DefaultMessage(key)
}

func (d *Dispatcher) formatMessage(agentID, key string, pairs ...string) string {
	text := d.message(agentID, key)
	if len(pairs) == 0 {
		return text
	}
	return strings.NewReplacer(pairs...).Replace(text)
}

func (d *Dispatcher) personaName(agentID string) string {
	if d.ports.Texts != nil && agentID != "" {
		if name := d.ports.Texts.Persona(agentID); name != "" {
			return name
		}
	}
	return agentID
}

func (d *Dispatcher) unknownReply(caller *Caller, inv *Invocation, style Style) Result {
	if style == StyleMessenger {
		return failure(ErrorCodeUnknown, d.message(caller.AgentID, agent.MessageCommandUnknown))
	}
	return unknown(inv)
}

func (d *Dispatcher) usageReply(caller *Caller, def *Definition, style Style) Result {
	if style == StyleMessenger {
		line := d.formatMessage(caller.AgentID, agent.MessageCommandUsage,
			agent.CommandPlaceholder, def.Name,
			agent.UsagePlaceholder, def.Usage,
		)
		return failure(ErrorCodeUsage, strings.TrimSpace(line))
	}
	return usage(def)
}

func (d *Dispatcher) execute(ctx context.Context, caller *Caller, inv *Invocation, def *Definition, b *Binding) Result {
	if !permits(def.Audience, caller.Class) {
		if def.Audience == AudienceGuests {
			if b.Style == StyleMessenger {
				return failure(ErrorCodeDenied, d.message(caller.AgentID, agent.MessageCommandGuestsOnly))
			}
			return failure(ErrorCodeDenied, ReplyGuestsOnly)
		}
		if b.Style == StyleMessenger {
			return failure(ErrorCodeDenied, d.message(caller.AgentID, agent.MessageCommandDenied))
		}
		return failure(ErrorCodeDenied, ReplyDenied)
	}
	if def.Kind == KindOperation {
		return d.runOperation(ctx, &operationCall{caller: caller, inv: inv, def: def, binding: b})
	}
	if def.Session && def.Kind == KindLocal && caller.SessionID == "" {
		if b.Style == StyleMessenger {
			return failure(ErrorCodeScope, d.formatMessage(caller.AgentID, agent.MessageCommandScope, agent.CommandPlaceholder, def.Name))
		}
		return failure(ErrorCodeScope, "Use /"+def.Name+" inside an Aura thread or a DM.")
	}
	if len(inv.Args) < def.MinArgs {
		if def.Prompt != "" && b.Interactive && len(inv.Args) == def.MinArgs-1 {
			pending := Invocation{Name: def.Name, Token: inv.Token, Args: append([]string(nil), inv.Args...)}
			return Result{Lines: []string{def.Prompt}, Pending: &pending}
		}
		return d.usageReply(caller, def, b.Style)
	}
	if def.MaxArgs != Variadic && len(inv.Args) > def.MaxArgs {
		return d.usageReply(caller, def, b.Style)
	}
	if def.Control != ControlNone {
		return Result{Control: def.Control}
	}
	h := handler{dispatcher: d, ports: d.ports, caller: caller, style: b.Style, args: inv.Args, text: inv.Text}
	switch def.Name {
	case "help":
		return Result{Lines: d.registry.Help(caller.Channel, caller.Class, caller.Direct)}
	case "status":
		return h.status(ctx)
	case "session":
		return h.session(ctx)
	case "new":
		return h.newSession(ctx)
	case "cancel":
		return h.cancel(ctx)
	case "skill":
		return h.skill(ctx)
	case "skill-create":
		return h.skillCreate(ctx)
	case NameForgetMe:
		if b.Style == StyleMessenger {
			return Result{Lines: []string{h.message(agent.MessageForgetmeConfirm)}}
		}
		return Result{Lines: []string{ReplyForgetMe}}
	default:
		return h.failed(def.Name, errors.New("command has no handler"))
	}
}

type handler struct {
	dispatcher *Dispatcher
	ports      Ports
	caller     *Caller
	style      Style
	args       []string
	text       string
}

func (h *handler) message(key string) string {
	return h.dispatcher.message(h.caller.AgentID, key)
}

func (h *handler) format(key string, pairs ...string) string {
	return h.dispatcher.formatMessage(h.caller.AgentID, key, pairs...)
}

func (h *handler) personaName() string {
	return h.dispatcher.personaName(h.caller.AgentID)
}

func (h *handler) status(ctx context.Context) Result {
	events, err := h.ports.Sessions.EventCount(ctx, h.caller.SessionID)
	if err != nil {
		return h.failed("status", err)
	}
	if h.style == StyleMessenger {
		return h.messengerStatus()
	}
	turn := "turn idle"
	if h.ports.Turns != nil {
		if active, queued := h.ports.Turns.Activity(h.caller.SessionID); active != "" {
			turn = "turn running " + active
			if queued > 0 {
				turn += fmt.Sprintf(" queued %d", queued)
			}
		}
	}
	return Result{Lines: []string{
		"session " + h.caller.SessionID,
		"persona " + h.caller.AgentID,
		fmt.Sprintf("events %d", events),
		turn,
	}}
}

func (h *handler) messengerStatus() Result {
	personaLine := h.format(agent.MessageCommandStatusPersona, agent.PersonaPlaceholder, h.personaName())
	activityLine := h.message(agent.MessageCommandIdle)
	if h.ports.Turns != nil {
		if active, queued := h.ports.Turns.Activity(h.caller.SessionID); active != "" {
			if queued > 0 {
				activityLine = h.format(agent.MessageCommandStatusRunningQueued, agent.CountPlaceholder, strconv.Itoa(queued))
			} else {
				activityLine = h.message(agent.MessageCommandStatusRunning)
			}
		}
	}
	return Result{Lines: []string{personaLine, activityLine}}
}

func (h *handler) session(ctx context.Context) Result {
	if len(h.args) == 0 {
		if h.style == StyleMessenger {
			return Result{Lines: []string{h.message(agent.MessageCommandSessionCurrent)}}
		}
		return Result{Lines: []string{"session " + h.caller.SessionID}}
	}
	id := h.args[0]
	sess, err := h.ports.Sessions.Switch(ctx, h.caller, id)
	if err != nil {
		if fault.HasCode(err, ErrorCodeNotSwitchable) {
			if h.style == StyleTerminal {
				var cerr *fault.Error[ErrorCode]
				if errors.As(err, &cerr) {
					return failure(ErrorCodeNotSwitchable, "aura: session "+id+": "+cerr.Detail)
				}
				return failure(ErrorCodeNotSwitchable, ReplyNotSwitchable)
			}
			return failure(ErrorCodeNotSwitchable, h.message(agent.MessageSessionNotSwitchable))
		}
		return h.failed("session", err)
	}
	if h.style == StyleMessenger {
		return Result{Lines: []string{h.message(agent.MessageCommandSessionSwitched)}, Diagnostic: true, Session: &sess}
	}
	return Result{Lines: []string{"switched to session " + sess.ID}, Diagnostic: true, Session: &sess}
}

func (h *handler) newSession(ctx context.Context) Result {
	sess, err := h.ports.Sessions.Create(ctx, h.caller)
	if err != nil {
		return h.failed("new", err)
	}
	if h.style == StyleMessenger {
		return Result{Lines: []string{h.message(agent.MessageCommandNew)}, Diagnostic: true, Session: &sess}
	}
	return Result{Lines: []string{"new session " + sess.ID}, Diagnostic: true, Session: &sess}
}

func (h *handler) cancel(ctx context.Context) Result {
	if h.ports.Turns == nil {
		if h.style == StyleMessenger {
			return Result{Lines: []string{h.message(agent.MessageCommandIdle)}}
		}
		return Result{Lines: []string{"no active turn"}}
	}
	turnID, cancelled, err := h.ports.Turns.CancelActive(ctx, h.caller.SessionID)
	if err != nil {
		return h.failed("cancel", err)
	}
	if !cancelled {
		if h.style == StyleMessenger {
			return Result{Lines: []string{h.message(agent.MessageCommandIdle)}}
		}
		return Result{Lines: []string{"no active turn"}}
	}
	if h.style == StyleMessenger {
		return Result{Lines: []string{h.message(agent.MessageCommandCancelled)}}
	}
	return Result{Lines: []string{"cancelled turn " + turnID}}
}

func (h *handler) skill(ctx context.Context) Result {
	name := h.args[0]
	if h.ports.Skills == nil {
		return h.skillUnavailable(name, "skill support is not available")
	}
	pin, err := h.ports.Skills.Activate(ctx, name)
	if err != nil {
		return h.skillUnavailable(name, err.Error())
	}
	return Result{Turn: &TurnStart{Text: h.text, Skill: &pin}}
}

func (h *handler) skillUnavailable(name, detail string) Result {
	if h.style == StyleTerminal {
		return failure(ErrorCodeSkillUnavailable, "aura: skill "+name+": "+detail)
	}
	return failure(ErrorCodeSkillUnavailable, h.format(agent.MessageSkillUnavailable, agent.SkillPlaceholder, name))
}

func (h *handler) skillCreate(ctx context.Context) Result {
	if h.ports.Skills == nil {
		return h.failed("skill-create", errors.New("skill support is not available"))
	}
	draft, err := h.ports.Skills.StageDraft(ctx, h.args[0], strings.Join(h.args[1:], " "))
	if err != nil {
		return h.failed("skill-create", err)
	}
	return Result{Lines: []string{
		"draft " + draft.ID,
		"digest " + draft.Digest,
		"review with: aura skills review " + draft.ID,
	}}
}

func (h *handler) failed(name string, err error) Result {
	if h.style == StyleTerminal {
		return failure(ErrorCodeFailed, "aura: /"+name+": "+err.Error())
	}
	return failure(ErrorCodeFailed, h.format(agent.MessageCommandFailed, agent.CommandPlaceholder, name))
}

func usage(def *Definition) Result {
	return failure(ErrorCodeUsage, strings.TrimSpace("usage: /"+def.Name+" "+def.Usage))
}

func unknown(inv *Invocation) Result {
	return failure(ErrorCodeUnknown, "aura: unknown command "+inv.Token+" (try /help)")
}

func failure(code ErrorCode, line string) Result {
	return Result{Lines: []string{line}, Diagnostic: true, Code: code}
}

func ResultOf(code ErrorCode) string {
	switch code {
	case ErrorCodeOK:
		return ResultOK
	case ErrorCodeUsage:
		return ResultUsage
	case ErrorCodeDenied:
		return ResultDenied
	case ErrorCodeUnavailable:
		return ResultUnavailable
	case ErrorCodeScope:
		return ResultScope
	case ErrorCodeNotSwitchable:
		return ResultNotSwitchable
	case ErrorCodeSkillUnavailable:
		return ResultSkillUnavailable
	default:
		return ResultFailed
	}
}
