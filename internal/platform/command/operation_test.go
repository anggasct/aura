package command

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/platform/effect"
	"github.com/anggasct/aura/internal/platform/principal"
)

type fakeOperations struct {
	planned   [][]string
	runs      []*OperationRun
	plan      func(tokens []string) OperationPlan
	reply     OperationReply
	groupSeen string
}

func (f *fakeOperations) Plan(group string, tokens []string) OperationPlan {
	f.groupSeen = group
	f.planned = append(f.planned, slices.Clone(tokens))
	if f.plan != nil {
		return f.plan(tokens)
	}
	return OperationPlan{Operation: group + ".list", Input: json.RawMessage(`{}`)}
}

func (f *fakeOperations) Run(_ context.Context, run *OperationRun) OperationReply {
	f.runs = append(f.runs, run)
	return f.reply
}

func operationDispatcher(t *testing.T, ops *fakeOperations, interactive bool) *Dispatcher {
	t.Helper()
	group := Definition{Name: "cron", Usage: "<verb> [arguments]", Summary: "manage jobs", Audience: AudienceOwner, Kind: KindOperation, MinArgs: 1, MaxArgs: Variadic}
	messenger := messengerBinding()
	messenger.Names = append(messenger.Names, "cron")
	terminal := terminalBinding()
	terminal.Interactive = interactive
	terminal.Names = append(terminal.Names, "cron")
	registry, err := NewRegistry(append(Catalog(), group), terminal, messenger)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	ports := &spyPorts{}
	dispatcher, err := NewDispatcher(registry, &Ports{Sessions: ports, Turns: ports, Skills: ports, Operations: ops}, nil)
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	return dispatcher
}

func direct(channel string, class principal.Class) *Caller {
	c := caller(channel, class)
	c.Direct = true
	return c
}

func TestOperationCommandRunsForTheOwnerInADirectChat(t *testing.T) {
	ops := &fakeOperations{reply: OperationReply{Lines: []string{"ID\tNAME", "j1\tnightly"}, Record: json.RawMessage(`{"r":1}`)}}
	d := operationDispatcher(t, ops, false)
	inv, _ := Parse(`/cron add "two words" --name='a b' tail`)
	inv.IdempotencyKey = "telegram:update:9"

	got := d.Execute(t.Context(), direct("discord", principal.ClassOwner), &inv)

	if got.Code != ErrorCodeOK || !slices.Equal(got.Lines, ops.reply.Lines) || string(got.Record) != `{"r":1}` || got.Diagnostic {
		t.Fatalf("Execute = %+v, want the reply of the operation", got)
	}
	if ops.groupSeen != "cron" || !slices.Equal(ops.planned[0], []string{"add", "two words", "--name=a b", "tail"}) {
		t.Errorf("planned group %q tokens %q", ops.groupSeen, ops.planned)
	}
	if len(ops.runs) != 1 || ops.runs[0].IdempotencyKey != "telegram:update:9" || ops.runs[0].Intent != nil || ops.runs[0].Plan.Operation != "cron.list" {
		t.Errorf("runs = %+v, want one fresh run under the message key", ops.runs)
	}
}

func TestOperationCommandIsRefusedOutsideADirectChatWithoutTouchingOperations(t *testing.T) {
	tests := []struct {
		name   string
		caller *Caller
		code   ErrorCode
		line   string
	}{
		{"owner in a group", caller("discord", principal.ClassOwner), ErrorCodeScope, ReplyDirectOnly},
		{"trusted in a direct chat", direct("discord", principal.ClassTrusted), ErrorCodeDenied, ReplyDenied},
		{"public in a direct chat", direct("discord", principal.ClassPublic), ErrorCodeDenied, ReplyDenied},
		{"trusted in a group", caller("discord", principal.ClassTrusted), ErrorCodeDenied, ReplyDenied},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ops := &fakeOperations{reply: OperationReply{Lines: []string{"secret rows"}}}
			d := operationDispatcher(t, ops, false)
			inv, _ := Parse("/cron list")

			got := d.Execute(t.Context(), tt.caller, &inv)

			if got.Code != tt.code || !slices.Equal(got.Lines, []string{tt.line}) || !got.Diagnostic {
				t.Fatalf("Execute = %+v, want %s %q", got, tt.code, tt.line)
			}
			if len(ops.planned) != 0 || len(ops.runs) != 0 {
				t.Errorf("operations were planned %d times and run %d times for a refused command", len(ops.planned), len(ops.runs))
			}
			if _, ok := d.Operation(tt.caller, &inv); ok {
				t.Error("Operation reported a reference for a refused command")
			}
		})
	}
}

func TestOperationCommandFailsClosedWithoutAnOperationsPort(t *testing.T) {
	d := operationDispatcher(t, nil, false)
	d.ports.Operations = nil
	inv, _ := Parse("/cron list")

	got := d.Execute(t.Context(), direct("discord", principal.ClassOwner), &inv)

	if got.Code != ErrorCodeUnavailable || !slices.Equal(got.Lines, []string{ReplyOperationsUnavailable}) {
		t.Fatalf("Execute = %+v, want unavailable", got)
	}
}

func TestOperationCommandRefusesTextTheTokenizerCannotRead(t *testing.T) {
	tests := []struct {
		name string
		text string
		line string
	}{
		{"open quote", `/cron add "x`, ReplyOpenQuote},
		{"dangling escape", `/cron add x\`, ReplyDanglingEscape},
		{"too long", "/cron add " + strings.Repeat("a", MaxCommandBytes), ReplyCommandTooLong},
		{"too many", "/cron " + strings.Repeat("a ", MaxTokens+1), ReplyTooManyArguments},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ops := &fakeOperations{}
			d := operationDispatcher(t, ops, false)
			inv, _ := Parse(tt.text)

			got := d.Execute(t.Context(), direct("discord", principal.ClassOwner), &inv)

			if got.Code != ErrorCodeUsage || !slices.Equal(got.Lines, []string{tt.line}) || len(ops.planned) != 0 {
				t.Fatalf("Execute = %+v planned %d, want usage %q", got, len(ops.planned), tt.line)
			}
		})
	}
}

func TestOperationCommandReturnsThePlanRefusalAsTheReply(t *testing.T) {
	refusal := Result{Lines: []string{"usage: /cron <list>"}, Diagnostic: true, Code: ErrorCodeUsage}
	ops := &fakeOperations{plan: func([]string) OperationPlan { return OperationPlan{Refusal: &refusal} }}
	d := operationDispatcher(t, ops, false)
	inv, _ := Parse("/cron nope")

	got := d.Execute(t.Context(), direct("discord", principal.ClassOwner), &inv)

	if got.Code != ErrorCodeUsage || !slices.Equal(got.Lines, refusal.Lines) || len(ops.runs) != 0 {
		t.Fatalf("Execute = %+v runs %d, want the plan refusal and no run", got, len(ops.runs))
	}
}

func TestOperationReferenceCarriesTheOperationAndItsInput(t *testing.T) {
	ops := &fakeOperations{plan: func([]string) OperationPlan {
		return OperationPlan{Operation: "cron.pause", Input: json.RawMessage(`{"id":"j1"}`)}
	}}
	d := operationDispatcher(t, ops, false)
	inv, _ := Parse("/cron pause j1")

	ref, ok := d.Operation(direct("discord", principal.ClassOwner), &inv)

	if !ok || ref.ID != "cron.pause" || string(ref.Input) != `{"id":"j1"}` {
		t.Fatalf("Operation = %+v, %v", ref, ok)
	}
	plain, _ := Parse("/status")
	if _, ok := d.Operation(direct("discord", principal.ClassOwner), &plain); ok {
		t.Error("a plain command reported an operation reference")
	}
	missing := &fakeOperations{plan: func([]string) OperationPlan {
		return OperationPlan{Missing: &OperationPrompt{Text: "ask", Usage: "usage"}}
	}}
	incomplete := operationDispatcher(t, missing, false)
	if _, ok := incomplete.Operation(direct("discord", principal.ClassOwner), &inv); ok {
		t.Error("an incomplete plan reported an operation reference")
	}
}

func TestOperationHelpListsGroupsOnlyForTheOwnerInADirectChat(t *testing.T) {
	d := operationDispatcher(t, &fakeOperations{}, false)
	help := func(c *Caller) string {
		inv, _ := Parse("/help")
		return strings.Join(d.Execute(t.Context(), c, &inv).Lines, "\n")
	}
	if got := help(direct("discord", principal.ClassOwner)); !strings.Contains(got, "/cron <verb> [arguments]             manage jobs") {
		t.Errorf("owner direct help = %q, want the cron group", got)
	}
	for name, c := range map[string]*Caller{
		"owner in a group":   caller("discord", principal.ClassOwner),
		"trusted in direct":  direct("discord", principal.ClassTrusted),
		"public in direct":   direct("discord", principal.ClassPublic),
		"trusted in a group": caller("discord", principal.ClassTrusted),
	} {
		if got := help(c); strings.Contains(got, "/cron") {
			t.Errorf("%s help lists the operation group: %q", name, got)
		}
	}
}

func TestOperationCommandAsksForTheOnlyMissingValueWhereTheBindingIsInteractive(t *testing.T) {
	missing := func(tokens []string) OperationPlan {
		if len(tokens) > 0 && tokens[len(tokens)-1] == "text" {
			return OperationPlan{Operation: "cron.add", Input: json.RawMessage(`{}`)}
		}
		return OperationPlan{Missing: &OperationPrompt{Text: "describe it:", Flag: "--description", Usage: "usage: /cron create --name <name> --description <text>"}}
	}
	t.Run("an interactive binding keeps the invocation pending", func(t *testing.T) {
		ops := &fakeOperations{plan: missing, reply: OperationReply{Lines: []string{"created"}}}
		d := operationDispatcher(t, ops, true)
		inv, _ := Parse("/cron create --name notes")

		got := d.Execute(t.Context(), direct(ChannelTerminal, principal.ClassOwner), &inv)

		if !slices.Equal(got.Lines, []string{"describe it:"}) || got.Pending == nil || got.Code != ErrorCodeOK {
			t.Fatalf("Execute = %+v, want the prompt with a pending invocation", got)
		}
		if !slices.Equal(got.Pending.Args, []string{"create", "--name", "notes", "--description"}) || got.Pending.Text != "" {
			t.Fatalf("pending = %+v", got.Pending)
		}
		answered := *got.Pending
		answered.Args = append(answered.Args, "text")
		final := d.Execute(t.Context(), direct(ChannelTerminal, principal.ClassOwner), &answered)
		if final.Pending != nil || !slices.Equal(final.Lines, []string{"created"}) || len(ops.runs) != 1 {
			t.Fatalf("answered = %+v runs %d, want the operation run once", final, len(ops.runs))
		}
	})
	t.Run("a messenger binding replies with the usage", func(t *testing.T) {
		ops := &fakeOperations{plan: missing}
		d := operationDispatcher(t, ops, false)
		inv, _ := Parse("/cron create --name notes")

		got := d.Execute(t.Context(), direct("discord", principal.ClassOwner), &inv)

		if got.Pending != nil || got.Code != ErrorCodeUsage || !strings.HasPrefix(got.Lines[0], "usage: /cron create") || len(ops.runs) != 0 {
			t.Fatalf("Execute = %+v", got)
		}
	})
}

func TestResumeReportsALiveLeaseAsReplayedAndRunsNothingElse(t *testing.T) {
	ops := &fakeOperations{reply: OperationReply{InProgress: true}}
	d := operationDispatcher(t, ops, false)
	inv, _ := Parse("/cron pause j1")
	inv.IdempotencyKey = "k"
	intent := &effect.Intent{ID: "intent-1", State: effect.StateStarted}
	recovery := &OperationRecovery{}

	got, replayed := d.Resume(t.Context(), &ResumeRequest{Caller: direct("discord", principal.ClassOwner), Invocation: &inv, Intent: intent, Recovery: recovery})

	if !replayed || len(got.Lines) != 0 {
		t.Fatalf("Resume = %+v, %v; want replayed with no reply", got, replayed)
	}
	if len(ops.runs) != 1 || ops.runs[0].Intent != intent || ops.runs[0].Recovery != recovery || ops.runs[0].IdempotencyKey != "k" {
		t.Errorf("runs = %+v, want the intent and recovery passed through", ops.runs)
	}
}

func TestResumeRepeatsTheScopeChecksBeforeTouchingOperations(t *testing.T) {
	ops := &fakeOperations{}
	d := operationDispatcher(t, ops, false)
	inv, _ := Parse("/cron pause j1")
	intent := &effect.Intent{ID: "intent-1", State: effect.StateStarted}

	got, replayed := d.Resume(t.Context(), &ResumeRequest{Caller: caller("discord", principal.ClassOwner), Invocation: &inv, Intent: intent, Recovery: &OperationRecovery{}})

	if replayed || got.Code != ErrorCodeScope || len(ops.runs) != 0 {
		t.Fatalf("Resume = %+v, %v runs %d, want the scope refusal", got, replayed, len(ops.runs))
	}
	if got, _ := d.Resume(t.Context(), &ResumeRequest{Invocation: &inv, Intent: intent, Recovery: &OperationRecovery{}}); got.Code != ErrorCodeInvalidArgument {
		t.Errorf("Resume without a caller = %+v, want invalid_argument", got)
	}
}

func TestRegistryRequiresOperationCommandsToBeOwnerOnly(t *testing.T) {
	_, err := NewRegistry([]Definition{{Name: "cron", Summary: "jobs", Audience: AudienceAny, Kind: KindOperation, MinArgs: 1, MaxArgs: Variadic}})
	if err == nil || !strings.Contains(err.Error(), "owner-only") {
		t.Fatalf("NewRegistry = %v, want the owner-only refusal", err)
	}
}

func TestRegistryListsTheDefinitionsBoundToAChannel(t *testing.T) {
	d := operationDispatcher(t, &fakeOperations{}, false)
	got := d.Registry().Definitions("discord")
	names := make([]string, 0, len(got))
	for i := range got {
		names = append(names, got[i].Name)
	}
	if !slices.Contains(names, "cron") || slices.Contains(names, "exit") || len(d.Registry().Definitions("nowhere")) != 0 {
		t.Errorf("definitions = %v", names)
	}
}
