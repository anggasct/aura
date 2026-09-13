package terminal

import (
	"context"
	"fmt"
	"strings"
)

type SkillContext struct {
	ID     string
	Caveat string
	Text   string
}

type Skills interface {
	ActivateSkill(ctx context.Context, name string) (SkillContext, error)
	CreateSkill(ctx context.Context, name, description string) (SkillDraft, error)
}

type SkillDraft struct {
	ID     string
	Digest string
}

func (c *Console) dispatch(ctx context.Context, raw string) (bool, error) {
	fields := strings.Fields(raw)
	command := strings.ToLower(strings.TrimPrefix(fields[0], "/"))
	args := fields[1:]
	switch command {
	case "help":
		return true, writeLine(c.out, helpText())
	case "exit", "quit":
		return false, nil
	case "clear":
		if c.tty != nil {
			return true, c.tty.clearScreen(ctx)
		}
		return true, writeLine(c.out, "")
	case "new":
		return true, c.newSession(ctx)
	case "session":
		return true, c.sessionCommand(ctx, args)
	case "cancel":
		c.cancelTurn()
		return true, nil
	case "status":
		return true, c.status(ctx)
	case "skill":
		return true, c.skillCommand(ctx, args)
	case "skill-create":
		return true, c.skillCreateCommand(ctx, args)
	default:
		return true, writeLinef(c.diag, "aura: unknown command %s (try /help)", fields[0])
	}
}

func (c *Console) newSession(ctx context.Context) error {
	sess, err := c.sessions.Create(ctx, c.principal)
	if err != nil {
		return fmt.Errorf("terminal: create session: %w", err)
	}
	c.sessionID = sess.ID
	return writeLinef(c.diag, "new session %s", sess.ID)
}

func (c *Console) sessionCommand(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return writeLinef(c.out, "session %s", c.sessionID)
	}
	id := args[0]
	sess, err := c.sessions.Get(ctx, id)
	if err != nil {
		return fmt.Errorf("terminal: session %s: %w", id, err)
	}
	if c.principal == "" || sess.OwnerID != c.principal {
		return fmt.Errorf("terminal: session %s is not owned by %s", id, c.principal)
	}
	c.sessionID = sess.ID
	return writeLinef(c.diag, "switched to session %s", sess.ID)
}

func (c *Console) skillCommand(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return writeLine(c.diag, "usage: /skill <name>")
	}
	if c.skills == nil {
		return writeLine(c.diag, "skill support is not available")
	}
	activated, err := c.skills.ActivateSkill(ctx, args[0])
	if err != nil {
		return writeLinef(c.diag, "aura: skill %s: %v", args[0], err)
	}
	if err := writeLinef(c.out, "skill %s", activated.ID); err != nil {
		return err
	}
	if err := writeLine(c.out, activated.Caveat); err != nil {
		return err
	}
	return writeLine(c.out, activated.Text)
}

func (c *Console) skillCreateCommand(ctx context.Context, args []string) error {
	if c.skills == nil {
		return writeLine(c.diag, "skill support is not available")
	}
	switch len(args) {
	case 0:
		return writeLine(c.diag, "usage: /skill-create <name> [description]")
	case 1:
		c.pendingSkill = args[0]
		return writeLine(c.out, "describe the skill in one line:")
	default:
		return c.finishSkillCreate(ctx, args[0], strings.Join(args[1:], " "))
	}
}

func (c *Console) finishSkillCreate(ctx context.Context, name, description string) error {
	if c.skills == nil {
		return writeLine(c.diag, "skill support is not available")
	}
	draft, err := c.skills.CreateSkill(ctx, name, description)
	if err != nil {
		return writeLinef(c.diag, "aura: skill-create %s: %v", name, err)
	}
	if err := writeLinef(c.out, "draft %s", draft.ID); err != nil {
		return err
	}
	if err := writeLinef(c.out, "digest %s", draft.Digest); err != nil {
		return err
	}
	return writeLine(c.out, "review with: aura skills review "+draft.ID)
}

func (c *Console) status(ctx context.Context) error {
	events, err := c.sessions.ListEvents(ctx, c.sessionID, 0, c.config.InMemoryHistory)
	if err != nil {
		if code := codeOf(err); code != "session_not_found" && code != "" {
			return fmt.Errorf("terminal: session events: %w", err)
		}
	}
	if err := writeLinef(c.out, "session %s", c.sessionID); err != nil {
		return err
	}
	return writeLinef(c.out, "events %d", len(events))
}

func codeOf(err error) string {
	type coder interface{ Code() string }
	if c, ok := err.(coder); ok {
		return c.Code()
	}
	return ""
}

func helpText() string {
	return "commands:\n" +
		"  /help              show this help\n" +
		"  /exit, /quit       leave the console\n" +
		"  /clear             clear the screen\n" +
		"  /new               start a new session\n" +
		"  /session [id]      show or switch to a session\n" +
		"  /cancel            cancel the active turn\n" +
		"  /status            show the current session\n" +
		"  /skill <name>      activate a skill\n" +
		"  /skill-create      stage a skill draft\n" +
		"  .                  compose a multi-line prompt in $EDITOR (interactive)\n"
}
