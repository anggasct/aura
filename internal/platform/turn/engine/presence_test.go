package runtimeengine

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/anggasct/aura/internal/platform/turn"
	runtimechannelhost "github.com/anggasct/aura/internal/platform/turn/channelhost"
	runtimeingress "github.com/anggasct/aura/internal/platform/turn/ingress"
	"github.com/anggasct/aura/internal/platform/turn/turntest"
)

type presenceCapture struct {
	mu          sync.Mutex
	started     int
	startedReqs []*runtimechannelhost.DeliveryRequest
	updates     []runtimechannelhost.PresenceUpdate
	ended       []string
	endOutcomes []runtimechannelhost.PresenceEnd
	delivers    int
	onDeliver   func()
}

func (a *presenceCapture) Start(_ context.Context, _ runtimeingress.IngressSink) error {
	return nil
}

func (a *presenceCapture) Deliver(_ context.Context, _ *runtimechannelhost.DeliveryRequest) (runtimechannelhost.ProviderReceipt, error) {
	a.mu.Lock()
	a.delivers++
	hook := a.onDeliver
	a.mu.Unlock()
	if hook != nil {
		hook()
	}
	return runtimechannelhost.ProviderReceipt{ProviderID: "fake", ExternalID: "ext-1"}, nil
}

func (a *presenceCapture) Health(_ context.Context) runtimechannelhost.ChannelHealth {
	return runtimechannelhost.ChannelHealth{Status: runtimechannelhost.ChannelHealthy}
}

func (a *presenceCapture) PresenceStarted(_ context.Context, req *runtimechannelhost.DeliveryRequest) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.started++
	if req != nil {
		copied := *req
		a.startedReqs = append(a.startedReqs, &copied)
	}
	return nil
}

func (a *presenceCapture) PresenceUpdated(_ context.Context, _ *runtimechannelhost.DeliveryRequest, update runtimechannelhost.PresenceUpdate) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.updates = append(a.updates, runtimechannelhost.PresenceUpdate{
		CallIDs:  slices.Clone(update.CallIDs),
		Activity: update.Activity,
		Outcome:  update.Outcome,
	})
	return nil
}

func (a *presenceCapture) PresenceEnded(_ context.Context, turnID string, end runtimechannelhost.PresenceEnd) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.ended = append(a.ended, turnID)
	a.endOutcomes = append(a.endOutcomes, end)
	return nil
}

func (a *presenceCapture) snapshotUpdates() []runtimechannelhost.PresenceUpdate {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.updates)
}

func (a *presenceCapture) counts() (started, failed int, ended []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, update := range a.updates {
		if update.Outcome == runtimechannelhost.StepFailed {
			failed++
		}
	}
	return a.started, failed, append([]string(nil), a.ended...)
}

type callSpec struct {
	id   string
	name string
	args map[string]any
}

type responseSpec struct {
	id       string
	name     string
	response map[string]any
}

func adkCallStep(text string, calls ...callSpec) turntest.FakeStep {
	parts := make([]any, 0, len(calls)+1)
	if text != "" {
		parts = append(parts, map[string]any{"text": text})
	}
	for _, call := range calls {
		args := call.args
		if args == nil {
			args = map[string]any{}
		}
		parts = append(parts, map[string]any{
			"functionCall": map[string]any{
				"id":   call.id,
				"name": call.name,
				"args": args,
			},
		})
	}
	raw, err := json.Marshal(map[string]any{
		"content": map[string]any{
			"role":  "model",
			"parts": parts,
		},
	})
	if err != nil {
		panic("test call payload is not serializable")
	}
	return turntest.FakeStep{Kind: "adk_event", Payload: raw}
}

func adkResponseStep(responses ...responseSpec) turntest.FakeStep {
	parts := make([]any, 0, len(responses))
	for _, resp := range responses {
		parts = append(parts, map[string]any{
			"functionResponse": map[string]any{
				"id":       resp.id,
				"name":     resp.name,
				"response": resp.response,
			},
		})
	}
	raw, err := json.Marshal(map[string]any{
		"content": map[string]any{
			"role":  "user",
			"parts": parts,
		},
	})
	if err != nil {
		panic("test response payload is not serializable")
	}
	return turntest.FakeStep{Kind: "adk_event", Payload: raw}
}

func adkTextStep(text string) turntest.FakeStep {
	return adkCallStep(text, callSpec{id: "call-1", name: "read_file"})
}

type failingPresenceExecutor struct {
	err error
}

func (f failingPresenceExecutor) Execute(_ context.Context, _ *turn.TurnRequest) iter.Seq2[turn.Event, error] {
	return func(yield func(turn.Event, error) bool) {
		yield(turn.Event{}, f.err)
	}
}

func runPresenceTurn(t *testing.T, durableMode bool, turnID string, executor TurnExecutor, wantTerminal string) *presenceCapture {
	t.Helper()
	capture := &presenceCapture{}
	if durableMode {
		rig := newDurableHostRig(t, Deps{Config: testLimits(4, 16), Channels: []runtimechannelhost.ChannelPort{capture}}, executor)
		mustCreateSession(t, rig.db, "channel:333")
		rig.startHost()
		outcome := callTurn(context.WithoutCancel(t.Context()), rig.engine, channelTurnRequest("channel:333", turnID))
		awaitTerminal(t, outcome, wantTerminal)
		return capture
	}
	engine, db, _ := newTestRuntime(t, Deps{Config: testLimits(2, 4), Channels: []runtimechannelhost.ChannelPort{capture}}, executor)
	mustCreateSession(t, db, "channel:333")
	req := channelTurnRequest("channel:333", turnID)
	if _, err := collect(t, engine, req); err != nil {
		t.Fatalf("Run: %v", err)
	}
	return capture
}

func TestEngineReportsStepOutcomesByCallID(t *testing.T) {
	modes := []struct {
		name    string
		durable bool
	}{
		{name: "in-memory", durable: false},
		{name: "durable", durable: true},
	}

	for _, mode := range modes {
		t.Run(mode.name, func(t *testing.T) {
			t.Run("parallel outcomes, out-of-order pairing, orphan response, and no args", func(t *testing.T) {
				secretGoal := "Cari berita terbaru soal Go 1.27 di web, ringkas hasilnya"
				steps := []turntest.FakeStep{
					adkCallStep("Checking both files",
						callSpec{id: "c1", name: "read_file", args: map[string]any{"goal": secretGoal}},
						callSpec{id: "c2", name: "read_file", args: map[string]any{"path": "notes.md"}},
					),
					adkResponseStep(responseSpec{id: "c1", name: "read_file", response: map[string]any{"error": "permission denied"}}),
					adkResponseStep(responseSpec{id: "c2", name: "read_file", response: map[string]any{"result": "ok"}}),
					adkCallStep("Reading workspace",
						callSpec{id: "c3", name: "read_file", args: map[string]any{"goal": secretGoal}},
						callSpec{id: "c4", name: "read_file", args: map[string]any{"path": "todo.md"}},
					),
					adkResponseStep(responseSpec{id: "c3", name: "read_file", response: map[string]any{"result": "ok"}}),
					adkResponseStep(responseSpec{id: "c-orphan", name: "read_file", response: map[string]any{"error": "unmatched"}}),
					adkResponseStep(responseSpec{id: "c4", name: "read_file", response: map[string]any{"result": "ok"}}),
					adkCallStep("Step alpha", callSpec{id: "ca", name: "lookup"}),
					adkCallStep("Step beta", callSpec{id: "cb", name: "lookup"}),
					adkResponseStep(responseSpec{id: "cb", name: "lookup", response: map[string]any{"error": "boom"}}),
					adkResponseStep(responseSpec{id: "ca", name: "lookup", response: map[string]any{"result": "ok"}}),
					textStep("final answer"),
				}

				capture := runPresenceTurn(t, mode.durable, "turn-outcomes-"+mode.name, turntest.NewFakeExecutor(steps), turn.EventKindTurnCompleted)
				got := capture.snapshotUpdates()

				want := []runtimechannelhost.PresenceUpdate{
					{CallIDs: []string{"c1", "c2"}, Activity: "Checking both files", Outcome: runtimechannelhost.StepRunning},
					{CallIDs: []string{"c1", "c2"}, Activity: "Checking both files", Outcome: runtimechannelhost.StepFailed},
					{CallIDs: []string{"c3", "c4"}, Activity: "Reading workspace", Outcome: runtimechannelhost.StepRunning},
					{CallIDs: []string{"c3", "c4"}, Activity: "Reading workspace", Outcome: runtimechannelhost.StepDone},
					{CallIDs: []string{"ca"}, Activity: "Step alpha", Outcome: runtimechannelhost.StepRunning},
					{CallIDs: []string{"cb"}, Activity: "Step beta", Outcome: runtimechannelhost.StepRunning},
					{CallIDs: []string{"cb"}, Activity: "Step beta", Outcome: runtimechannelhost.StepFailed},
					{CallIDs: []string{"ca"}, Activity: "Step alpha", Outcome: runtimechannelhost.StepDone},
				}

				if len(got) != len(want) {
					t.Fatalf("updates = %+v (len %d), want %+v (len %d)", got, len(got), want, len(want))
				}
				for i := range want {
					if !slices.Equal(got[i].CallIDs, want[i].CallIDs) || got[i].Activity != want[i].Activity || got[i].Outcome != want[i].Outcome {
						t.Errorf("update[%d] = %+v, want %+v", i, got[i], want[i])
					}
					if strings.Contains(got[i].Activity, secretGoal) || strings.Contains(got[i].Activity, "notes.md") {
						t.Errorf("update[%d] leaked tool arguments: %+v", i, got[i])
					}
				}
			})

			t.Run("failed turn emits no global failure update", func(t *testing.T) {
				capture := runPresenceTurn(t, mode.durable, "turn-failed-noglobal-"+mode.name, failingPresenceExecutor{err: errors.New("model exploded")}, turn.EventKindTurnFailed)
				if got := capture.snapshotUpdates(); len(got) != 0 {
					t.Fatalf("updates on failed turn without steps = %+v, want none", got)
				}
			})
		})
	}
}

func TestSilentCallStartsAStepWithTheWorkingText(t *testing.T) {
	modes := []struct {
		name    string
		durable bool
	}{
		{name: "in-memory", durable: false},
		{name: "durable", durable: true},
	}

	for _, mode := range modes {
		t.Run(mode.name, func(t *testing.T) {
			steps := []turntest.FakeStep{
				adkCallStep("", callSpec{id: "call-silent", name: "read_file", args: map[string]any{"path": "secret.txt"}}),
				adkResponseStep(responseSpec{id: "call-silent", name: "read_file", response: map[string]any{"result": "ok"}}),
				textStep("done"),
			}
			capture := runPresenceTurn(t, mode.durable, "turn-silent-"+mode.name, turntest.NewFakeExecutor(steps), turn.EventKindTurnCompleted)
			got := capture.snapshotUpdates()
			want := []runtimechannelhost.PresenceUpdate{
				{CallIDs: []string{"call-silent"}, Activity: "", Outcome: runtimechannelhost.StepRunning},
				{CallIDs: []string{"call-silent"}, Activity: "", Outcome: runtimechannelhost.StepDone},
			}
			if len(got) != len(want) {
				t.Fatalf("updates = %+v, want %+v", got, want)
			}
			for i := range want {
				if !slices.Equal(got[i].CallIDs, want[i].CallIDs) || got[i].Activity != want[i].Activity || got[i].Outcome != want[i].Outcome {
					t.Errorf("update[%d] = %+v, want %+v", i, got[i], want[i])
				}
			}
		})
	}
}

func TestEngineDrivesPresenceForSlowTurn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		executor := turntest.NewFakeExecutor([]turntest.FakeStep{
			adkTextStep("Looking through notes"),
			textStep("final answer"),
		})
		capture := &presenceCapture{}
		engine, db, _ := newTestRuntime(t, Deps{Config: testLimits(2, 4), Channels: []runtimechannelhost.ChannelPort{capture}}, executor)
		mustCreateSession(t, db, "channel:333")
		req := channelTurnRequest("channel:333", "turn-presence-1")
		if _, err := collect(t, engine, req); err != nil {
			t.Fatalf("Run: %v", err)
		}
		capture.mu.Lock()
		defer capture.mu.Unlock()
		if capture.started != 1 {
			t.Fatalf("started = %d, want 1", capture.started)
		}
		if len(capture.ended) != 1 || capture.ended[0] != "turn-presence-1" {
			t.Fatalf("ended = %v, want turn-presence-1", capture.ended)
		}
		found := false
		for _, update := range capture.updates {
			if update.Outcome == runtimechannelhost.StepRunning && update.Activity == "Looking through notes" {
				found = true
			}
		}
		if !found {
			t.Fatalf("updates = %+v, want model snippet", capture.updates)
		}
	})
}

type namedPresenceCapture struct {
	*presenceCapture
	name string
}

func (n *namedPresenceCapture) ChannelName() string {
	return n.name
}

func TestEngineSkipsPresenceForNonChannelOrigin(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		executor := turntest.NewFakeExecutor([]turntest.FakeStep{
			adkTextStep("Looking through notes"),
			textStep("final answer"),
		})
		capture := &presenceCapture{}
		engine, db, _ := newTestRuntime(t, Deps{Config: testLimits(2, 4), Channels: []runtimechannelhost.ChannelPort{capture}}, executor)
		mustCreateSession(t, db, "channel:333")
		req := channelTurnRequest("channel:333", "turn-presence-terminal")
		req.Origin = turn.OriginTerminal
		if _, err := collect(t, engine, req); err != nil {
			t.Fatalf("Run: %v", err)
		}
		capture.mu.Lock()
		defer capture.mu.Unlock()
		if capture.started != 0 || len(capture.updates) != 0 || len(capture.ended) != 0 {
			t.Fatalf("presence ran for terminal origin: %+v", capture)
		}
	})
}

func TestEngineRoutesPresenceToOwningAdapter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		executor := turntest.NewFakeExecutor([]turntest.FakeStep{
			adkTextStep("Looking through notes"),
			textStep("final answer"),
		})
		discordCapture := &namedPresenceCapture{presenceCapture: &presenceCapture{}, name: "discord"}
		slackCapture := &namedPresenceCapture{presenceCapture: &presenceCapture{}, name: "slack"}
		engine, db, _ := newTestRuntime(t, Deps{Config: testLimits(2, 4), Channels: []runtimechannelhost.ChannelPort{slackCapture, discordCapture}}, executor)
		mustCreateSession(t, db, "channel:333")
		req := channelTurnRequest("channel:333", "turn-presence-owner")
		if _, err := collect(t, engine, req); err != nil {
			t.Fatalf("Run: %v", err)
		}
		discordCapture.mu.Lock()
		defer discordCapture.mu.Unlock()
		slackCapture.mu.Lock()
		defer slackCapture.mu.Unlock()
		if discordCapture.started != 1 || len(discordCapture.updates) == 0 {
			t.Fatalf("discord capture = %+v, want presence", discordCapture)
		}
		if slackCapture.started != 0 || len(slackCapture.updates) != 0 || len(slackCapture.ended) != 0 {
			t.Fatalf("slack capture = %+v, want none", slackCapture)
		}
	})
}

func TestCancelledDurableTurnClosesItsPresence(t *testing.T) {
	gate := make(chan struct{})
	t.Cleanup(func() { close(gate) })
	executor := turntest.NewFakeExecutor([]turntest.FakeStep{{Kind: turn.EventKindModelStarted, Payload: []byte(`{}`), Block: gate}})
	capture := &presenceCapture{}
	rig := newDurableHostRig(t, Deps{Config: testLimits(4, 16), Channels: []runtimechannelhost.ChannelPort{capture}}, executor)
	mustCreateSession(t, rig.db, "channel:333")
	rig.startHost()
	outcome := callTurn(context.WithoutCancel(t.Context()), rig.engine, channelTurnRequest("channel:333", "turn-cancelled"))
	eventually(t, "the turn reaches the executor", func() bool { return executor.StartCount() == 1 })
	if _, cancelled, err := rig.engine.CancelActive(t.Context(), "channel:333"); err != nil || !cancelled {
		t.Fatalf("CancelActive = %v, %v; want the turn cancelled", cancelled, err)
	}
	awaitTerminal(t, outcome, turn.EventKindTurnCancelled)
	started, failed, ended := capture.counts()
	if started != 1 || failed != 0 {
		t.Errorf("presence starts = %d, failed updates = %d; want 1 and 0", started, failed)
	}
	if len(ended) != 1 || ended[0] != "turn-cancelled" {
		t.Errorf("presence ends = %v, want one for the turn", ended)
	}
}

func TestPreemptedDurableTurnEndsItsPresenceWithoutFailingIt(t *testing.T) {
	executor := newTwoInvocationExecutor()
	capture := &presenceCapture{}
	rig := newShortLeaseHostRig(t, Deps{Config: testLimits(4, 16), Channels: []runtimechannelhost.ChannelPort{capture}}, executor)
	mustCreateSession(t, rig.db, "channel:333")
	rig.startHost()
	outcome := callTurn(context.WithoutCancel(t.Context()), rig.engine, channelTurnRequest("channel:333", "turn-preempted"))
	executor.waitBlocked(t)
	loseHostLease(t, rig.db)
	awaitTerminal(t, outcome, turn.EventKindTurnCompleted)
	started, failed, ended := capture.counts()
	if failed != 0 {
		t.Errorf("failed presence updates = %d, want none for a preempted invocation", failed)
	}
	if started != 2 || len(ended) != 2 {
		t.Errorf("presence starts = %d, ends = %v; want two of each", started, ended)
	}
}
