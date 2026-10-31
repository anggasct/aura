package command

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
	ReplyNotSwitchable = "That session can't be used in this conversation."
)

type Dispatcher struct {
	registry *Registry
	ports    Ports
	recorder Recorder
}

func NewDispatcher(registry *Registry, ports Ports, recorder Recorder) (*Dispatcher, error) {
	if registry == nil {
		return nil, errors.New("command: registry must not be nil")
	}
	if ports.Sessions == nil {
		return nil, errors.New("command: sessions port must not be nil")
	}
	return &Dispatcher{registry: registry, ports: ports, recorder: recorder}, nil
}

func (d *Dispatcher) Registry() *Registry { return d.registry }

func (d *Dispatcher) Execute(ctx context.Context, caller *Caller, inv *Invocation) Result {
	b, ok := d.registry.binding(caller.Channel)
	if !ok {
		return unknown(inv)
	}
	def, ok := d.registry.Lookup(inv.Name)
	if !ok {
		return unknown(inv)
	}
	style := b.binding.Style
	if def.Kind == KindTerminalOnly && style != StyleTerminal {
		_, finish := d.start(ctx, NameTerminalOnly, caller)
		finish(ResultUnavailable)
		return failure(CodeUnavailable, "/"+inv.Name+" only works in the terminal.")
	}
	if !b.names[def.Name] {
		return unknown(inv)
	}
	ctx, finish := d.start(ctx, def.Name, caller)
	result := d.execute(ctx, caller, inv, &def, &b.binding)
	finish(resultLabel(result.Code))
	return result
}

func (d *Dispatcher) start(ctx context.Context, name string, caller *Caller) (spanCtx context.Context, finish func(string)) {
	if d.recorder == nil {
		return ctx, func(string) {}
	}
	return d.recorder.Start(ctx, name, caller.Channel, string(caller.Class))
}

func (d *Dispatcher) execute(ctx context.Context, caller *Caller, inv *Invocation, def *Definition, b *Binding) Result {
	if !permits(def.Audience, caller.Class) {
		return failure(CodeDenied, ReplyDenied)
	}
	if def.Session && caller.SessionID == "" {
		return failure(CodeScope, "Use /"+def.Name+" inside an Aura thread or a DM.")
	}
	if len(inv.Args) < def.MinArgs {
		if def.Prompt != "" && b.Interactive && len(inv.Args) == def.MinArgs-1 {
			pending := Invocation{Name: def.Name, Token: inv.Token, Args: append([]string(nil), inv.Args...)}
			return Result{Lines: []string{def.Prompt}, Pending: &pending}
		}
		return usage(def)
	}
	if def.MaxArgs != Variadic && len(inv.Args) > def.MaxArgs {
		return usage(def)
	}
	if def.Control != ControlNone {
		return Result{Control: def.Control}
	}
	h := handler{ports: d.ports, caller: caller, style: b.Style, args: inv.Args}
	switch def.Name {
	case "help":
		return Result{Lines: d.registry.Help(caller.Channel, caller.Class)}
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
	default:
		return h.failed(def.Name, errors.New("command has no handler"))
	}
}

type handler struct {
	ports  Ports
	caller *Caller
	style  Style
	args   []string
}

func (h *handler) status(ctx context.Context) Result {
	events, err := h.ports.Sessions.EventCount(ctx, h.caller.SessionID)
	if err != nil {
		return h.failed("status", err)
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

func (h *handler) session(ctx context.Context) Result {
	if len(h.args) == 0 {
		return Result{Lines: []string{"session " + h.caller.SessionID}}
	}
	id := h.args[0]
	sess, err := h.ports.Sessions.Switch(ctx, h.caller, id)
	if err != nil {
		var cerr *Error
		if errors.As(err, &cerr) && cerr.Code == CodeNotSwitchable {
			if h.style == StyleTerminal {
				return failure(CodeNotSwitchable, "aura: session "+id+": "+cerr.Detail)
			}
			return failure(CodeNotSwitchable, ReplyNotSwitchable)
		}
		return h.failed("session", err)
	}
	return Result{Lines: []string{"switched to session " + sess.ID}, Diagnostic: true, Session: &sess}
}

func (h *handler) newSession(ctx context.Context) Result {
	sess, err := h.ports.Sessions.Create(ctx, h.caller)
	if err != nil {
		return h.failed("new", err)
	}
	return Result{Lines: []string{"new session " + sess.ID}, Diagnostic: true, Session: &sess}
}

func (h *handler) cancel(ctx context.Context) Result {
	if h.ports.Turns == nil {
		return Result{Lines: []string{"no active turn"}}
	}
	turnID, cancelled, err := h.ports.Turns.CancelActive(ctx, h.caller.SessionID)
	if err != nil {
		return h.failed("cancel", err)
	}
	if !cancelled {
		return Result{Lines: []string{"no active turn"}}
	}
	return Result{Lines: []string{"cancelled turn " + turnID}}
}

func (h *handler) skill(ctx context.Context) Result {
	name := h.args[0]
	if h.ports.Skills == nil {
		return h.skillUnavailable(name, "skill support is not available")
	}
	activation, err := h.ports.Skills.Activate(ctx, name)
	if err != nil {
		return h.skillUnavailable(name, err.Error())
	}
	return Result{Lines: []string{"skill " + activation.SkillID, activation.Caveat, activation.Text}}
}

func (h *handler) skillUnavailable(name, detail string) Result {
	if h.style == StyleTerminal {
		return failure(CodeSkillUnavailable, "aura: skill "+name+": "+detail)
	}
	return failure(CodeSkillUnavailable, "Skill "+name+" is not available.")
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
		return failure(CodeFailed, "aura: /"+name+": "+err.Error())
	}
	return failure(CodeFailed, "/"+name+" failed; check aura status.")
}

func usage(def *Definition) Result {
	return failure(CodeUsage, strings.TrimSpace("usage: /"+def.Name+" "+def.Usage))
}

func unknown(inv *Invocation) Result {
	return failure(CodeUnknown, "aura: unknown command "+inv.Token+" (try /help)")
}

func failure(code Code, line string) Result {
	return Result{Lines: []string{line}, Diagnostic: true, Code: code}
}

func resultLabel(code Code) string {
	switch code {
	case CodeOK:
		return ResultOK
	case CodeUsage:
		return ResultUsage
	case CodeDenied:
		return ResultDenied
	case CodeUnavailable:
		return ResultUnavailable
	case CodeScope:
		return ResultScope
	case CodeNotSwitchable:
		return ResultNotSwitchable
	case CodeSkillUnavailable:
		return ResultSkillUnavailable
	default:
		return ResultFailed
	}
}
