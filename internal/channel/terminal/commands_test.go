package terminal

import (
	"context"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/command"
	"github.com/anggasct/aura/internal/principal"
)

type recordedCommand struct {
	name, channel, class, result string
}

type commandSpy struct {
	seen []recordedCommand
}

func (s *commandSpy) Start(ctx context.Context, name, channel, class string) (spanCtx context.Context, finish func(string)) {
	return ctx, func(result string) {
		s.seen = append(s.seen, recordedCommand{name: name, channel: channel, class: class, result: result})
	}
}

func runCommands(t *testing.T, sessions *fakeSessions, input string) (out, diag string, runner *fakeRunner, spy *commandSpy) {
	t.Helper()
	runner = &fakeRunner{eventsFor: func(string) []Event { return []Event{{Kind: "turn.completed"}} }}
	console, stdout, stderr, cancel := newConsoleForTest(runner, sessions, input)
	defer cancel()
	console.SetAgentID("aura")
	spy = &commandSpy{}
	console.SetCommandRecorder(spy)
	if err := console.Run(t.Context()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	return stdout.String(), stderr.String(), runner, spy
}

func TestTerminalHelpMatchesTheRegistryGolden(t *testing.T) {
	out, _, _, _ := runCommands(t, newFakeSessions(), "/help\n")
	want := `commands:
  /help                                show this help
  /status                              show the current session
  /session [id]                        show or switch to a session
  /new                                 start a new session
  /cancel                              cancel the active turn
  /skill <name> [request…]             activate a skill
  /skill-create <name> <description…>  stage a skill draft
  /exit, /quit                         leave the console
  /clear                               clear the screen
  .                                    compose a multi-line prompt in $EDITOR (interactive)
`
	if out != want {
		t.Errorf("help =\n%s\nwant\n%s", out, want)
	}
	registry, err := command.New(CommandBinding())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := strings.Join(registry.Help(command.ChannelTerminal, principal.ClassOwner), "\n") + "\n"; got != out {
		t.Errorf("terminal help is not the registry help:\n%s", got)
	}
}

func TestTerminalCanonicalStatusAndCancel(t *testing.T) {
	sessions := newFakeSessions()
	out, _, _, spy := runCommands(t, sessions, "/status\n/cancel\n")
	want := "session sess_1\npersona aura\nevents 0\nturn idle\nno active turn\n"
	if out != want {
		t.Errorf("stdout = %q, want %q", out, want)
	}
	if len(spy.seen) != 2 || spy.seen[0] != (recordedCommand{"status", "terminal", "owner", "ok"}) || spy.seen[1].name != "cancel" {
		t.Errorf("observed = %+v", spy.seen)
	}
}

func TestTerminalRejectsExtraArguments(t *testing.T) {
	sessions := newFakeSessions()
	out, diag, runner, spy := runCommands(t, sessions, "/new now\n/status please\n/exit later\nprompt\n")
	if len(sessions.sessions) != 1 {
		t.Errorf("sessions = %d, want only the initial session", len(sessions.sessions))
	}
	for _, want := range []string{"usage: /new\n", "usage: /status\n", "usage: /exit\n"} {
		if !strings.Contains(diag, want) {
			t.Errorf("stderr = %q, want %q", diag, want)
		}
	}
	if out != "" {
		t.Errorf("stdout = %q, want nothing before the turn", out)
	}
	if len(runner.calls) != 1 {
		t.Errorf("turns = %d, want the prompt after /exit later to run", len(runner.calls))
	}
	for _, seen := range spy.seen {
		if seen.result != "usage" {
			t.Errorf("observed = %+v, want usage", seen)
		}
	}
}

func TestTerminalQuitAliasLeaves(t *testing.T) {
	_, _, runner, spy := runCommands(t, newFakeSessions(), "/QUIT\nignored\n")
	if len(runner.calls) != 0 {
		t.Errorf("turns = %d after /quit, want 0", len(runner.calls))
	}
	if len(spy.seen) != 1 || spy.seen[0].name != "exit" || spy.seen[0].result != "ok" {
		t.Errorf("observed = %+v", spy.seen)
	}
}

func TestTerminalUnrecognizedSlashTokensStayDiagnostics(t *testing.T) {
	_, diag, runner, spy := runCommands(t, newFakeSessions(), "/bogus\n/a_b\n/\n")
	for _, want := range []string{"aura: unknown command /bogus (try /help)", "aura: unknown command /a_b (try /help)", "aura: unknown command / (try /help)"} {
		if !strings.Contains(diag, want) {
			t.Errorf("stderr = %q, want %q", diag, want)
		}
	}
	if len(runner.calls) != 0 || len(spy.seen) != 0 {
		t.Errorf("turns = %d observed = %+v, want none", len(runner.calls), spy.seen)
	}
}

func TestTerminalNewSessionNotesOnStderr(t *testing.T) {
	sessions := newFakeSessions()
	out, diag, runner, _ := runCommands(t, sessions, "/new\nprompt\n")
	if !strings.Contains(diag, "new session sess_2") || strings.Contains(out, "new session") {
		t.Errorf("stdout = %q stderr = %q", out, diag)
	}
	if len(runner.calls) != 1 || runner.calls[0].SessionID != "sess_2" || runner.calls[0].AgentID != "aura" {
		t.Errorf("turn = %+v, want sess_2 as aura", runner.calls)
	}
}
