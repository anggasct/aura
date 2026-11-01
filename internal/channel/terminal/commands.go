package terminal

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/anggasct/aura/internal/command"
	"github.com/anggasct/aura/internal/principal"
)

func CommandBinding() command.Binding {
	return command.Binding{
		Channel:     command.ChannelTerminal,
		Style:       command.StyleTerminal,
		Interactive: true,
		Names:       []string{"help", "status", "session", "new", "cancel", "skill", "skill-create", "exit", "clear"},
		Notes:       []command.Note{{Syntax: ".", Summary: "compose a multi-line prompt in $EDITOR (interactive)"}},
	}
}

func (c *Console) buildCommands() error {
	registry, err := command.New(CommandBinding())
	if err != nil {
		return err
	}
	dispatcher, err := command.NewDispatcher(registry, command.Ports{
		Sessions: consoleSessions{console: c},
		Turns:    idleTurns{},
		Skills:   c.skills,
	}, c.recorder)
	if err != nil {
		return err
	}
	c.commands = dispatcher
	return nil
}

func (c *Console) caller() *command.Caller {
	return &command.Caller{
		Channel:     command.ChannelTerminal,
		PrincipalID: c.principal,
		Class:       principal.ClassOwner,
		SessionID:   c.sessionID,
		AgentID:     c.agentID,
	}
}

func (c *Console) dispatch(ctx context.Context, raw string) (bool, error) {
	inv, ok := command.Parse(raw)
	if !ok {
		return true, writeLinef(c.diag, "aura: unknown command %s (try /help)", strings.Fields(raw)[0])
	}
	return c.execute(ctx, &inv)
}

func (c *Console) resumePending(ctx context.Context, line string) (bool, error) {
	inv := *c.pending
	c.pending = nil
	inv.Args = append(inv.Args, line)
	return c.execute(ctx, &inv)
}

func (c *Console) execute(ctx context.Context, inv *command.Invocation) (bool, error) {
	result := c.commands.Execute(ctx, c.caller(), inv)
	switch result.Control {
	case command.ControlExit:
		return false, nil
	case command.ControlClear:
		if c.tty != nil {
			return true, c.tty.clearScreen(ctx)
		}
		return true, writeLine(c.out, "")
	case command.ControlNone:
	}
	if result.Session != nil {
		c.sessionID = result.Session.ID
		if result.Session.AgentID != "" {
			c.agentID = result.Session.AgentID
		}
	}
	c.pending = result.Pending
	if result.Turn != nil {
		return true, c.runTurn(ctx, result.Turn.Text, result.Turn.Skill)
	}
	w := c.out
	if result.Diagnostic {
		w = c.diag
	}
	for _, line := range result.Lines {
		if err := writeLine(w, line); err != nil {
			return true, err
		}
	}
	return true, nil
}

type consoleSessions struct {
	console *Console
}

func (s consoleSessions) EventCount(ctx context.Context, sessionID string) (int, error) {
	events, err := s.console.sessions.ListEvents(ctx, sessionID, 0, s.console.config.InMemoryHistory)
	if err != nil {
		if code := codeOf(err); code != "session_not_found" && code != "" {
			return 0, fmt.Errorf("session events: %w", err)
		}
	}
	return len(events), nil
}

func (s consoleSessions) Create(ctx context.Context, caller *command.Caller) (command.Session, error) {
	sess, err := s.console.sessions.Create(ctx, caller.PrincipalID, caller.AgentID)
	if err != nil {
		return command.Session{}, fmt.Errorf("create session: %w", err)
	}
	return command.Session{ID: sess.ID, OwnerID: sess.OwnerID, AgentID: sess.AgentID}, nil
}

func (s consoleSessions) Switch(ctx context.Context, caller *command.Caller, id string) (command.Session, error) {
	sess, err := s.console.sessions.Get(ctx, id)
	if err != nil {
		if errors.Is(err, ErrSessionNotFound) {
			return command.Session{}, command.Errorf(command.ErrorCodeNotSwitchable, "not found")
		}
		return command.Session{}, err
	}
	if caller.PrincipalID == "" || sess.OwnerID != caller.PrincipalID {
		return command.Session{}, command.Errorf(command.ErrorCodeNotSwitchable, "not owned by %s", caller.PrincipalID)
	}
	return command.Session{ID: sess.ID, OwnerID: sess.OwnerID, AgentID: sess.AgentID}, nil
}

type idleTurns struct{}

func (idleTurns) CancelActive(context.Context, string) (turnID string, cancelled bool, err error) {
	return "", false, nil
}

func (idleTurns) Activity(string) (activeTurnID string, queued int) {
	return "", 0
}

func codeOf(err error) string {
	type coder interface{ Code() string }
	if c, ok := err.(coder); ok {
		return c.Code()
	}
	return ""
}
