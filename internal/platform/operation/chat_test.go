package operation_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/platform/capability/capabilitytest"
	"github.com/anggasct/aura/internal/platform/command"
	"github.com/anggasct/aura/internal/platform/effect"
	"github.com/anggasct/aura/internal/platform/operation"
	"github.com/anggasct/aura/internal/platform/operation/operationtest"
	"github.com/anggasct/aura/internal/platform/principal"
)

func chatRig(t *testing.T) (*operationtest.Rig, *operation.Chat) {
	t.Helper()
	rig := operationtest.NewRig(t, nil)
	chat, err := operation.NewChat(&operation.ChatDeps{Registry: rig.Registry, Capabilities: capabilitytest.Host(t)})
	if err != nil {
		t.Fatalf("NewChat: %v", err)
	}
	return rig, chat
}

func ownerChat() *command.Caller {
	return &command.Caller{Channel: "telegram", PrincipalID: operationtest.OwnerID, Class: principal.ClassOwner, SessionID: operationtest.SessionID, AgentID: "main", Direct: true}
}

func tokens(t *testing.T, text string) []string {
	t.Helper()
	parsed, err := command.Tokenize(text)
	if err != nil {
		t.Fatalf("Tokenize(%q): %v", text, err)
	}
	return parsed
}

func TestChatGroupsProjectOnlyGroupsWithAChatOperation(t *testing.T) {
	hostOnly := operation.Catalog{Group: operation.Group{Name: "hostonly", Summary: "host things"}, Descriptors: []operation.Descriptor{{
		ID: "hostonly.run", Summary: "run", Exposure: operation.ExposureHost, Risk: operation.RiskRead, Commit: operation.CommitNone,
	}}}
	definitions := operation.ChatGroups([]operation.Catalog{operationtest.Catalog(), hostOnly})
	if len(definitions) != 1 {
		t.Fatalf("definitions = %+v, want only the stub group", definitions)
	}
	got := definitions[0]
	if got.Name != operationtest.Group || got.Kind != command.KindOperation || got.Audience != command.AudienceOwner || got.Summary != "stub operations for tests" {
		t.Errorf("definition = %+v", got)
	}
	if _, err := command.NewRegistry(definitions); err != nil {
		t.Errorf("the generated definitions do not register: %v", err)
	}
}

func TestChatPlanBuildsTheCanonicalInputFromTheGrammar(t *testing.T) {
	_, chat := chatRig(t)
	tests := []struct {
		name  string
		text  string
		id    string
		input string
		apply bool
	}{
		{"a positional", "read notes", operationtest.Read, `{"subject":"notes"}`, false},
		{"a quoted positional", `read "two words"`, operationtest.Read, `{"subject":"two words"}`, false},
		{"the owner-only operation is reachable", "owner-only x", operationtest.OwnerOnly, `{"subject":"x"}`, false},
		{"a destructive preview", "delete set1", operationtest.Delete, `{"subject":"set1"}`, false},
		{"apply after the positional", "delete set1 --apply", operationtest.Delete, `{"subject":"set1"}`, true},
		{"apply before the positional", "delete --apply set1", operationtest.Delete, `{"subject":"set1"}`, true},
		{"apply with a value", "delete set1 --apply=true", operationtest.Delete, `{"subject":"set1"}`, true},
		{"apply set to false", "delete set1 --apply=false", operationtest.Delete, `{"subject":"set1"}`, false},
		{"a dashed value after the literal marker", "read -- --odd", operationtest.Read, `{"subject":"--odd"}`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan := chat.Plan(operationtest.Group, tokens(t, tt.text))
			if plan.Refusal != nil || plan.Missing != nil {
				t.Fatalf("Plan = %+v, want an executable plan", plan)
			}
			if plan.Operation != tt.id || string(plan.Input) != tt.input || plan.Apply != tt.apply {
				t.Errorf("Plan = %s %s apply=%t, want %s %s apply=%t", plan.Operation, plan.Input, plan.Apply, tt.id, tt.input, tt.apply)
			}
		})
	}
}

func TestChatPlanRefusesWhatTheGrammarDoesNotAllow(t *testing.T) {
	_, chat := chatRig(t)
	tests := []struct {
		name string
		text string
		code command.ErrorCode
		want string
	}{
		{"no verb", "", command.ErrorCodeUsage, "usage: /stub <change|delete|external|fresh|grant|idempotent|notice|owner-only|read>"},
		{"an unknown verb", "frobnicate x", command.ErrorCodeUsage, "usage: /stub <change|"},
		{"a host operation", "host x", command.ErrorCodeUnavailable, "operation_not_exposed: run it on the host with aura stub host <subject>"},
		{"a missing positional", "read", command.ErrorCodeUsage, "operation_input_invalid: parameter subject is required"},
		{"an extra positional", "read a b", command.ErrorCodeUsage, "operation_input_invalid: the command has more arguments than this operation takes"},
		{"an unknown flag", "read a --nope x", command.ErrorCodeUsage, "operation_input_invalid: the command has a flag this operation does not have"},
		{"apply on an operation without a preview", "read a --apply", command.ErrorCodeUsage, "operation_input_invalid: the command has a flag this operation does not have"},
		{"apply with a bad value", "delete a --apply=maybe", command.ErrorCodeUsage, "operation_input_invalid: the apply flag takes true or false"},
		{"a value over its length", "read " + strings.Repeat("x", 65), command.ErrorCodeUsage, "operation_input_invalid: parameter subject is longer than its maximum length"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan := chat.Plan(operationtest.Group, tokens(t, tt.text))
			if plan.Refusal == nil || plan.Operation != "" || plan.Input != nil {
				t.Fatalf("Plan = %+v, want a refusal carrying no operation", plan)
			}
			if plan.Refusal.Code != tt.code || !plan.Refusal.Diagnostic || !strings.HasPrefix(plan.Refusal.Lines[0], tt.want) {
				t.Errorf("refusal = %+v, want %s %q", plan.Refusal, tt.code, tt.want)
			}
		})
	}
	t.Run("an unknown group", func(t *testing.T) {
		if plan := chat.Plan("nothing", nil); plan.Refusal == nil || plan.Refusal.Code != command.ErrorCodeUnavailable {
			t.Errorf("Plan = %+v, want unavailable", plan)
		}
	})
}

func TestChatPlanAsksForTheOnlyMissingValueThatHasAPrompt(t *testing.T) {
	prompted := operation.Catalog{Group: operation.Group{Name: "notes", Summary: "notes"}, Descriptors: []operation.Descriptor{{
		ID: "notes.create", Summary: "create", Exposure: operation.ExposureAgent, Risk: operation.RiskRead, Commit: operation.CommitNone,
		Params: []operation.Param{
			{Name: "name", Kind: operation.KindString, Summary: "name", Required: true, MinLength: 1, MaxLength: 32},
			{Name: "description", Kind: operation.KindString, Summary: "text", Required: true, MinLength: 1, MaxLength: 64, Prompt: "describe it in one line:"},
		},
		Output: operation.Output{Kind: operation.OutputRecord},
	}}}
	rig := operationtest.NewRig(t, &operationtest.Options{Catalogs: []operation.Catalog{prompted}, Bindings: func(*operationtest.Stubs) []operation.Binding {
		return []operation.Binding{{ID: "notes.create", Handle: func(_ context.Context, _ *operation.Call) (operation.Result, error) {
			return operation.Result{Data: json.RawMessage(`{"ok":true}`)}, nil
		}}}
	}})
	chat, err := operation.NewChat(&operation.ChatDeps{Registry: rig.Registry})
	if err != nil {
		t.Fatalf("NewChat: %v", err)
	}
	t.Run("the prompted value is the only one missing", func(t *testing.T) {
		plan := chat.Plan("notes", tokens(t, "create --name todo"))
		if plan.Missing == nil || plan.Missing.Text != "describe it in one line:" || plan.Missing.Flag != "--description" || !strings.HasPrefix(plan.Missing.Usage, "usage: /notes create") || plan.Operation != "" {
			t.Fatalf("Plan = %+v, want the prompt for --description", plan)
		}
		answered := chat.Plan("notes", append(tokens(t, "create --name todo"), "--description", "keep tidy notes"))
		if answered.Missing != nil || answered.Refusal != nil || string(answered.Input) != `{"name":"todo","description":"keep tidy notes"}` {
			t.Errorf("answered plan = %+v", answered)
		}
	})
	t.Run("two missing values are a usage error", func(t *testing.T) {
		plan := chat.Plan("notes", tokens(t, "create"))
		if plan.Missing != nil || plan.Refusal == nil || plan.Refusal.Code != command.ErrorCodeUsage {
			t.Errorf("Plan = %+v, want a usage refusal", plan)
		}
	})
}

func runChat(t *testing.T, chat *operation.Chat, text, key string, caller *command.Caller) command.OperationReply {
	t.Helper()
	plan := chat.Plan(operationtest.Group, tokens(t, text))
	if plan.Refusal != nil || plan.Missing != nil {
		t.Fatalf("Plan(%q) = %+v, want an executable plan", text, plan)
	}
	return chat.Run(t.Context(), &command.OperationRun{Caller: caller, Plan: &plan, IdempotencyKey: key})
}

func TestChatRunsOperationsThroughTheRegistryAndRendersTheCLIOutput(t *testing.T) {
	rig, chat := chatRig(t)

	read := runChat(t, chat, "read notes", "telegram:1", ownerChat())
	change := runChat(t, chat, "change notes", "telegram:2", ownerChat())

	want := []string{"done: true", "operation: " + operationtest.Read}
	if read.Code != command.ErrorCodeOK || !slices.Equal(read.Lines, want) || len(read.Record) == 0 {
		t.Fatalf("read = %+v, want %q with a recorded result", read, want)
	}
	if change.Code != command.ErrorCodeOK || change.Lines[1] != "operation: "+operationtest.Change {
		t.Fatalf("change = %+v", change)
	}
	if ledger, _ := rig.Stubs.Ledger(t.Context(), operationtest.Change); ledger != 1 {
		t.Errorf("ledger rows = %d, want the one change", ledger)
	}
	if n := receiptCount(t, rig); n != 1 {
		t.Errorf("receipts = %d, want the change's receipt under the message key", n)
	}
	var recorded struct {
		ToolName string `json:"tool_name"`
		Output   struct {
			Lines []string `json:"lines"`
		} `json:"output"`
	}
	if err := json.Unmarshal(change.Record, &recorded); err != nil || recorded.ToolName != operationtest.Change || !slices.Equal(recorded.Output.Lines, change.Lines) {
		t.Errorf("record = %s (%v), want the rendered lines under the operation", change.Record, err)
	}
}

func TestChatRepeatsOfAMessageApplyOnceAndAnotherRequestUnderItsKeyFailsClosed(t *testing.T) {
	rig, chat := chatRig(t)
	first := runChat(t, chat, "change notes", "telegram:9", ownerChat())
	second := runChat(t, chat, "change notes", "telegram:9", ownerChat())
	if !slices.Equal(first.Lines, second.Lines) {
		t.Errorf("second reply = %q, want the recorded %q", second.Lines, first.Lines)
	}
	if ledger, _ := rig.Stubs.Ledger(t.Context(), operationtest.Change); ledger != 1 {
		t.Errorf("ledger rows = %d, want one application for one message", ledger)
	}
	other := runChat(t, chat, "change elsewhere", "telegram:9", ownerChat())
	if other.Code != command.ErrorCodeFailed || other.Lines[0] != "operation_key_reused" {
		t.Errorf("another request under the key = %+v, want operation_key_reused", other)
	}
}

func TestChatDestructiveOperationsPreviewUntilApplied(t *testing.T) {
	rig, chat := chatRig(t)
	preview := runChat(t, chat, "delete set1", "telegram:1", ownerChat())
	if !slices.Equal(preview.Lines, []string{"subject: set1", "cannot be undone"}) || rig.Stubs.Handled(operationtest.Delete) != 0 {
		t.Fatalf("preview = %+v handled %d, want the preview lines and nothing deleted", preview, rig.Stubs.Handled(operationtest.Delete))
	}
	applied := runChat(t, chat, "delete set1 --apply", "telegram:2", ownerChat())
	if applied.Code != command.ErrorCodeOK || applied.Lines[1] != "operation: "+operationtest.Delete || rig.Stubs.Handled(operationtest.Delete) != 1 {
		t.Fatalf("applied = %+v handled %d, want one deletion", applied, rig.Stubs.Handled(operationtest.Delete))
	}
}

func TestChatTrustGrantRunsDirectlyBecauseTheOwnerTypedIt(t *testing.T) {
	rig, chat := chatRig(t)
	got := runChat(t, chat, "grant set1", "telegram:1", ownerChat())
	if got.Code != command.ErrorCodeOK || rig.Stubs.Handled(operationtest.Grant) != 1 || len(rig.Prompts()) != 0 || rig.Stubs.Previews() != 0 {
		t.Fatalf("grant = %+v handled %d previews %d cards %d, want a direct run with no card and no preview", got, rig.Stubs.Handled(operationtest.Grant), rig.Stubs.Previews(), len(rig.Prompts()))
	}
}

func TestChatRepliesCarryACodedSafeMessage(t *testing.T) {
	rig, chat := chatRig(t)
	rig.Stubs.FailWith(operationtest.Read, errors.New("open /etc/shadow: permission denied sk-live-secret"))
	rig.Stubs.FailWith(operationtest.Idempotent, operation.Fail(command.ErrorCodeFailed, errors.New("database path /var/lib/aura/x.db is locked")))
	tests := []struct {
		name string
		text string
		code command.ErrorCode
		want string
	}{
		{"a raw handler error", "read x", command.ErrorCodeFailed, "operation_failed"},
		{"a module code", "idempotent x", command.ErrorCodeFailed, "operation_failed: command_failed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := runChat(t, chat, tt.text, "telegram:"+tt.name, ownerChat())
			if got.Code != tt.code || !slices.Equal(got.Lines, []string{tt.want}) || got.Record != nil {
				t.Fatalf("reply = %+v, want only %q", got, tt.want)
			}
			for _, line := range got.Lines {
				if strings.Contains(line, "/etc") || strings.Contains(line, "sk-live") || strings.Contains(line, "/var/lib") {
					t.Errorf("reply %q leaks handler text", line)
				}
			}
		})
	}
}

func TestChatNeverRunsOutsideADirectChatOrForANonOwner(t *testing.T) {
	rig, chat := chatRig(t)
	plan := chat.Plan(operationtest.Group, tokens(t, "change notes"))
	groupCaller := ownerChat()
	groupCaller.Direct = false
	trusted := ownerChat()
	trusted.Class = principal.ClassTrusted
	for name, caller := range map[string]*command.Caller{"a group chat": groupCaller, "a trusted principal": trusted} {
		t.Run(name, func(t *testing.T) {
			got := chat.Run(t.Context(), &command.OperationRun{Caller: caller, Plan: &plan, IdempotencyKey: "k-" + name})
			if got.Code != command.ErrorCodeDenied || got.Lines[0] != "operation_denied" {
				t.Fatalf("reply = %+v, want operation_denied", got)
			}
		})
	}
	if rig.Stubs.Handled(operationtest.Change) != 0 {
		t.Error("a handler ran for a refused caller")
	}
}

func TestChatRunRejectsIncompleteRuns(t *testing.T) {
	_, chat := chatRig(t)
	for name, run := range map[string]*command.OperationRun{
		"nil run":      nil,
		"no caller":    {Plan: &command.OperationPlan{Operation: operationtest.Read}},
		"no plan":      {Caller: ownerChat()},
		"unknown id":   {Caller: ownerChat(), Plan: &command.OperationPlan{Operation: "stub.nothing"}},
		"empty resume": {Caller: ownerChat(), Plan: &command.OperationPlan{Operation: operationtest.Read, Input: json.RawMessage(`{"subject":"x"}`)}, Intent: &effect.Intent{ID: "i"}},
	} {
		t.Run(name, func(t *testing.T) {
			got := chat.Run(t.Context(), run)
			if len(got.Lines) != 1 || got.Code == command.ErrorCodeOK {
				t.Errorf("Run = %+v, want a coded refusal", got)
			}
		})
	}
	if _, err := operation.NewChat(nil); err == nil {
		t.Error("NewChat accepted nil dependencies")
	}
}

func TestChatCanonicalInputEqualsTheCLIAndToolInput(t *testing.T) {
	_, chat := chatRig(t)
	catalog := operationtest.Catalog()
	d := &catalog.Descriptors[1]
	tool, err := d.Validate(operation.SurfaceAgent, json.RawMessage(`{ "subject": "two words" }`))
	if err != nil {
		t.Fatalf("tool validation: %v", err)
	}
	in, err := operation.NewInput(d, operation.SurfaceCLI)
	if err != nil {
		t.Fatalf("NewInput: %v", err)
	}
	if err := in.Set("subject", "two words"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	cli, err := in.Canonical()
	if err != nil {
		t.Fatalf("Canonical: %v", err)
	}
	plan := chat.Plan(operationtest.Group, tokens(t, `change "two words"`))
	if string(plan.Input) != string(tool) || string(plan.Input) != string(cli) || operation.Digest(plan.Input) != operation.Digest(tool) {
		t.Errorf("chat %s, tool %s, cli %s: want one canonical input and digest", plan.Input, tool, cli)
	}
}
