package terminal

import (
	"context"
	"fmt"
	"strings"
)

// dispatch runs one local slash command and reports whether the console
// should continue reading. Slash commands never become model input; an
// unknown command is a diagnostic, not a turn.
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
			return true, c.tty.ClearScreen()
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
	default:
		return true, writeLinef(c.diag, "aura: unknown command %s (try /help)", fields[0])
	}
}

// newSession starts a fresh durable conversation owned by the local principal.
func (c *Console) newSession(ctx context.Context) error {
	sess, err := c.sessions.Create(ctx, c.principal)
	if err != nil {
		return fmt.Errorf("terminal: create session: %w", err)
	}
	c.sessionID = sess.ID
	return writeLinef(c.diag, "new session %s", sess.ID)
}

// sessionCommand switches to a named session after validating local owner
// access, or prints the current session when no argument is given.
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

// status prints the current session and the bounded tail of its event log.
func (c *Console) status(ctx context.Context) error {
	events, err := c.sessions.ListEvents(ctx, c.sessionID, 0, c.config.InMemoryHistory)
	if err != nil {
		// A session with no events is a plain status, not an error.
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
		"  .                  compose a multi-line prompt in $EDITOR (interactive)\n"
}
