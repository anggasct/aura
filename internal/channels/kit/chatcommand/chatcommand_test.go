package chatcommand

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/platform/command"
	"github.com/anggasct/aura/internal/platform/effect"
	"github.com/anggasct/aura/internal/platform/principal"
	runtimeingress "github.com/anggasct/aura/internal/platform/turn/ingress"
)

const testChannel = "testchat"

type noSessions struct{}

func (noSessions) EventCount(context.Context, string) (int, error) { return 0, nil }

func (noSessions) Create(context.Context, *command.Caller) (command.Session, error) {
	return command.Session{}, errors.New("unused")
}

func (noSessions) Switch(context.Context, *command.Caller, string) (command.Session, error) {
	return command.Session{}, errors.New("unused")
}

type scriptedEffects struct {
	prepared []*effect.PrepareRequest
	invoke   bool
	state    effect.State
	receipt  json.RawMessage
	err      error
}

func (s *scriptedEffects) Execute(ctx context.Context, req *effect.PrepareRequest, provider effect.Provider) (*effect.Intent, error) {
	s.prepared = append(s.prepared, req)
	if s.err != nil {
		return nil, s.err
	}
	intent := &effect.Intent{ID: "intent-1", State: s.state, ProviderReceipt: s.receipt}
	if s.invoke {
		outcome, err := provider.Invoke(ctx, &effect.Invocation{})
		if err != nil {
			return nil, err
		}
		intent.State = effect.StateSucceeded
		intent.ProviderReceipt = outcome.Receipt
	}
	return intent, nil
}

func newRun(t *testing.T, effects Effects, line string) *Run {
	t.Helper()
	registry, err := command.New(command.Binding{Channel: testChannel, Style: command.StyleMessenger, Names: []string{"help", "session", "skill"}})
	if err != nil {
		t.Fatalf("command.New(): %v", err)
	}
	dispatcher, err := command.NewDispatcher(registry, command.Ports{Sessions: noSessions{}}, nil)
	if err != nil {
		t.Fatalf("NewDispatcher(): %v", err)
	}
	inv, ok := command.Parse(line)
	if !ok {
		t.Fatalf("Parse(%q) is not a command", line)
	}
	return &Run{
		Effects:        effects,
		Dispatcher:     dispatcher,
		Logger:         slog.New(slog.DiscardHandler),
		Caller:         &command.Caller{Channel: testChannel, PrincipalID: principal.OwnerID, Class: principal.ClassOwner, ConversationKey: "conv", SessionID: "sess", AgentID: "main"},
		Invocation:     &inv,
		IdempotencyKey: "src:update:1",
		EffectSession:  "sess",
	}
}

func TestExecuteRunsTheCommandOnceAndReturnsItsLines(t *testing.T) {
	effects := &scriptedEffects{invoke: true}
	outcome, err := Execute(t.Context(), newRun(t, effects, "/help"))
	if err != nil {
		t.Fatalf("Execute(): %v", err)
	}
	if outcome.Receipt == nil || outcome.IntentID != "intent-1" || len(outcome.Receipt.Lines) == 0 || !strings.HasPrefix(outcome.Receipt.Lines[0], "commands:") {
		t.Fatalf("outcome = %+v, want the help lines", outcome)
	}
	if len(effects.prepared) != 1 {
		t.Fatalf("prepared intents = %d, want 1", len(effects.prepared))
	}
	prepared := effects.prepared[0]
	if prepared.Provider != Provider || prepared.Operation != "help" || prepared.SessionID != "sess" || prepared.IdempotencyKey != "src:update:1" || prepared.EventKind != effect.EventKindChannelRequested || prepared.Classification != effect.ClassificationEffectful {
		t.Errorf("prepared = %+v", prepared)
	}
}

func TestExecuteCarriesTheSessionTargetInTheRequest(t *testing.T) {
	effects := &scriptedEffects{invoke: true}
	if _, err := Execute(t.Context(), newRun(t, effects, "/session other")); err != nil {
		t.Fatalf("Execute(): %v", err)
	}
	var request Request
	if err := json.Unmarshal(effects.prepared[0].Request, &request); err != nil {
		t.Fatalf("request: %v", err)
	}
	if request.Command != "session" || request.Conversation != "conv" || request.Target != "other" {
		t.Errorf("request = %+v", request)
	}
}

func TestExecuteReplaysAJournaledReceiptWithoutRunningTheCommand(t *testing.T) {
	receipt := json.RawMessage(`{"lines":["journaled"]}`)
	outcome, err := Execute(t.Context(), newRun(t, &scriptedEffects{state: effect.StateSucceeded, receipt: receipt}, "/help"))
	if err != nil {
		t.Fatalf("Execute(): %v", err)
	}
	if outcome.Receipt == nil || outcome.Receipt.Lines[0] != "journaled" {
		t.Errorf("outcome = %+v, want the journaled lines", outcome)
	}
}

func TestExecuteReturnsTheSkillPinFromAReceipt(t *testing.T) {
	receipt, err := json.Marshal(Receipt{Skill: &runtimeingress.SkillPin{SkillID: "review"}})
	if err != nil {
		t.Fatalf("Marshal(): %v", err)
	}
	outcome, err := Execute(t.Context(), newRun(t, &scriptedEffects{state: effect.StateSucceeded, receipt: receipt}, "/skill review"))
	if err != nil {
		t.Fatalf("Execute(): %v", err)
	}
	if outcome.Receipt == nil || outcome.Receipt.Skill == nil || outcome.Receipt.Skill.SkillID != "review" {
		t.Errorf("outcome = %+v, want the skill pin", outcome)
	}
}

func TestExecuteStaysSilentWhenTheIntentDidNotSucceedOrTheReceiptIsUnreadable(t *testing.T) {
	cases := []struct {
		name    string
		effects *scriptedEffects
	}{
		{"failed intent", &scriptedEffects{state: effect.StateFailed}},
		{"unknown intent", &scriptedEffects{state: effect.StateUnknown}},
		{"garbage receipt", &scriptedEffects{state: effect.StateSucceeded, receipt: json.RawMessage(`nope`)}},
		{"empty receipt", &scriptedEffects{state: effect.StateSucceeded, receipt: json.RawMessage(`{"lines":[]}`)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			outcome, err := Execute(t.Context(), newRun(t, tc.effects, "/help"))
			if err != nil || outcome.Receipt != nil {
				t.Errorf("Execute() = %+v, %v; want a silent outcome", outcome, err)
			}
		})
	}
}

func TestExecuteWrapsEffectFailures(t *testing.T) {
	cause := errors.New("journal down")
	_, err := Execute(t.Context(), newRun(t, &scriptedEffects{err: cause}, "/help"))
	if !errors.Is(err, cause) {
		t.Errorf("Execute() error = %v, want the journal failure", err)
	}
}

func TestExecuteRejectsIncompleteRuns(t *testing.T) {
	run := newRun(t, &scriptedEffects{}, "/help")
	cases := map[string]*Run{
		"nil run":         nil,
		"no effects":      {Dispatcher: run.Dispatcher, Logger: run.Logger, Caller: run.Caller, Invocation: run.Invocation},
		"no dispatcher":   {Effects: run.Effects, Logger: run.Logger, Caller: run.Caller, Invocation: run.Invocation},
		"no caller":       {Effects: run.Effects, Dispatcher: run.Dispatcher, Logger: run.Logger, Invocation: run.Invocation},
		"no invocation":   {Effects: run.Effects, Dispatcher: run.Dispatcher, Logger: run.Logger, Caller: run.Caller},
		"unknown command": {Effects: run.Effects, Dispatcher: run.Dispatcher, Logger: run.Logger, Caller: run.Caller, Invocation: &command.Invocation{Name: "nope"}},
	}
	for name, candidate := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Execute(t.Context(), candidate)
			var target *Error
			if !errors.As(err, &target) || target.Code != ErrorCodeInvalidArgument {
				t.Errorf("Execute() error = %v, want invalid_argument", err)
			}
		})
	}
}
