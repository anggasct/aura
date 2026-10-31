package runtimeengine

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"sync"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/durable"
	"github.com/anggasct/aura/internal/runtime"
	"github.com/anggasct/aura/internal/store"
)

type stubSkillRenderer struct {
	mu    sync.Mutex
	body  string
	err   error
	calls int
}

func (r *stubSkillRenderer) RenderSkill(_ context.Context, _ string, _ *runtime.SkillPin) (caveat, body string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	if r.err != nil {
		return "", "", r.err
	}
	return "untrusted caveat", r.body, nil
}

type pinRecordingExecutor struct {
	mu   sync.Mutex
	pins []*runtime.SkillPin
}

func (x *pinRecordingExecutor) Execute(_ context.Context, req *runtime.TurnRequest) iter.Seq2[store.RuntimeEvent, error] {
	x.mu.Lock()
	x.pins = append(x.pins, req.Skill)
	x.mu.Unlock()
	return func(yield func(store.RuntimeEvent, error) bool) {
		yield(store.RuntimeEvent{Kind: runtime.EventKindMessageCompleted, Payload: []byte(`{}`)}, nil)
	}
}

func (x *pinRecordingExecutor) started() []*runtime.SkillPin {
	x.mu.Lock()
	defer x.mu.Unlock()
	return append([]*runtime.SkillPin(nil), x.pins...)
}

func samplePin() *runtime.SkillPin {
	return &runtime.SkillPin{SkillID: "local/pdf@0123456789ab", Digest: "0123456789abcdef", InvocationID: "inv-1", PolicyVersion: "1"}
}

func pinnedRequest(sessionID, turnID string) *runtime.TurnRequest {
	req := sampleRequest(sessionID, turnID)
	req.Skill = samplePin()
	return req
}

func kinds(events []store.RuntimeEvent) []string {
	out := make([]string, len(events))
	for i := range events {
		out[i] = events[i].Kind
	}
	return out
}

func assertActivation(t *testing.T, ev *store.RuntimeEvent, turnID, body string) {
	t.Helper()
	if ev.Kind != runtime.EventKindSkillActivated || ev.Author != runtime.AuthorSystem || ev.TurnID != turnID {
		t.Fatalf("activation event = %+v", ev)
	}
	var payload runtime.SkillActivation
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		t.Fatalf("payload: %v", err)
	}
	want := runtime.SkillActivation{SkillID: "local/pdf@0123456789ab", Digest: "0123456789abcdef", InvocationID: "inv-1", PolicyVersion: "1", TurnID: turnID, Caveat: "untrusted caveat", Body: body}
	if payload != want {
		t.Fatalf("payload = %+v, want %+v", payload, want)
	}
}

func TestPinnedTurnRecordsTheActivationBeforeExecution(t *testing.T) {
	executor := &pinRecordingExecutor{}
	renderer := &stubSkillRenderer{body: "Do things."}
	engine, db, _ := newTestRuntime(t, Config{MaxActiveTurns: 2, MaxPendingTurns: 4, Skills: renderer}, executor)
	mustCreateSession(t, db, "session-a")
	events, err := collect(t, engine, pinnedRequest("session-a", "turn-1"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	want := []string{runtime.EventKindTurnAccepted, runtime.EventKindSkillActivated, runtime.EventKindMessageCompleted, runtime.EventKindTurnCompleted}
	if got := kinds(events); len(got) != len(want) || got[1] != want[1] || got[3] != want[3] {
		t.Fatalf("kinds = %v, want %v", got, want)
	}
	assertActivation(t, &events[1], "turn-1", "Do things.")
	if pins := executor.started(); len(pins) != 1 || pins[0] == nil || *pins[0] != *samplePin() {
		t.Fatalf("executor pins = %+v", pins)
	}
	plain, err := collect(t, engine, sampleRequest("session-a", "turn-2"))
	if err != nil {
		t.Fatalf("plain run: %v", err)
	}
	for _, ev := range plain {
		if ev.Kind == runtime.EventKindSkillActivated {
			t.Fatal("unpinned turn recorded an activation")
		}
	}
	if renderer.calls != 1 {
		t.Errorf("renderer calls = %d, want 1", renderer.calls)
	}
}

func TestPinnedTurnFailsClosedWhenTheSkillIsUnavailable(t *testing.T) {
	for name, renderer := range map[string]SkillRenderer{
		"not configured": nil,
		"render fails":   &stubSkillRenderer{err: errors.New("skill_unavailable: digest changed")},
	} {
		t.Run(name, func(t *testing.T) {
			executor := &pinRecordingExecutor{}
			engine, db, _ := newTestRuntime(t, Config{MaxActiveTurns: 2, MaxPendingTurns: 4, Skills: renderer}, executor)
			mustCreateSession(t, db, "session-a")
			events, err := collect(t, engine, pinnedRequest("session-a", "turn-1"))
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			last := events[len(events)-1]
			var failed struct {
				Code string `json:"code"`
			}
			_ = json.Unmarshal(last.Payload, &failed)
			if last.Kind != runtime.EventKindTurnFailed || failed.Code != string(runtime.ErrorCodeSkillUnavailable) {
				t.Fatalf("terminal = %s %s", last.Kind, last.Payload)
			}
			if len(executor.started()) != 0 {
				t.Fatal("executor ran without its pinned skill")
			}
		})
	}
}

func TestDurablePinnedTurnRecordsOneActivation(t *testing.T) {
	executor := &pinRecordingExecutor{}
	renderer := &stubSkillRenderer{body: "Do things."}
	engine, _, db := newDurableLiveTestRuntime(t, Config{MaxActiveTurns: 2, MaxPendingTurns: 4, Skills: renderer}, executor)
	envelope := sampleEnvelope("conv-1", "msg-1")
	envelope.Skill = samplePin()
	mustCreateSession(t, db, envelope.ConversationID)
	ref, err := engine.Accept(context.Background(), envelope)
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	events := collectDurableTerminal(t, db, ref.TurnID)
	var activations []store.RuntimeEvent
	for i := range events {
		if events[i].Kind == runtime.EventKindSkillActivated {
			activations = append(activations, events[i])
		}
	}
	if len(activations) != 1 || events[1].Kind != runtime.EventKindSkillActivated {
		t.Fatalf("kinds = %v", kinds(events))
	}
	assertActivation(t, &activations[0], ref.TurnID, "Do things.")
	if pins := executor.started(); len(pins) != 1 || pins[0] == nil || pins[0].InvocationID != "inv-1" {
		t.Fatalf("executor pins = %+v", pins)
	}
}

type journalInvocation struct {
	mu      sync.Mutex
	journal map[string][]byte
}

func (j *journalInvocation) Run() durable.RunRef { return durable.RunRef{} }
func (j *journalInvocation) Payload() []byte     { return nil }
func (j *journalInvocation) Signal(context.Context, string) ([]byte, bool) {
	return nil, false
}
func (j *journalInvocation) Sleep(time.Duration) error            { return nil }
func (j *journalInvocation) Timer(time.Duration) <-chan time.Time { return nil }
func (j *journalInvocation) Wait(context.Context, string, time.Duration) (payload []byte, timedOut, ok bool) {
	return nil, false, false
}

func (j *journalInvocation) RunAction(ctx context.Context, key string, fn func(ctx context.Context) ([]byte, error)) ([]byte, error) {
	j.mu.Lock()
	if out, ok := j.journal[key]; ok {
		j.mu.Unlock()
		return out, nil
	}
	j.mu.Unlock()
	out, err := fn(ctx)
	if err != nil {
		return nil, err
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.journal == nil {
		j.journal = map[string][]byte{}
	}
	j.journal[key] = out
	return out, nil
}

func TestDurableReplayReusesTheJournaledActivation(t *testing.T) {
	renderer := &stubSkillRenderer{body: "Original body."}
	engine, db, events := newTestRuntime(t, Config{MaxActiveTurns: 2, MaxPendingTurns: 4, Skills: renderer}, &pinRecordingExecutor{})
	mustCreateSession(t, db, "session-a")
	inv := &journalInvocation{}
	req := pinnedRequest("session-a", "turn-1")
	attempt := func() error {
		scope := durable.NewTurnScope(inv)
		persist := func(ctx context.Context, ev *store.RuntimeEvent) error {
			ev.ID = scope.NextEventID(req.TurnID)
			ev.SessionID, ev.TurnID, ev.SchemaVersion, ev.CreatedAt = req.SessionID, req.TurnID, 1, time.Unix(0, 0).UTC()
			_, _, err := events.UpsertEvent(ctx, ev)
			return err
		}
		return engine.persistSkillActivation(context.Background(), req, journaledSkillActivation(scope, engine.renderSkillActivation), persist)
	}
	if err := attempt(); err != nil {
		t.Fatalf("first attempt: %v", err)
	}
	renderer.mu.Lock()
	renderer.body, renderer.err = "Changed body.", errors.New("skill_unavailable: quarantined")
	renderer.mu.Unlock()
	if err := attempt(); err != nil {
		t.Fatalf("replayed attempt: %v", err)
	}
	stored, err := store.NewDedupeStore(db).ListTurnEvents(context.Background(), "turn-1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(stored) != 1 {
		t.Fatalf("stored = %v, want one activation", kinds(stored))
	}
	assertActivation(t, &stored[0], "turn-1", "Original body.")
	if renderer.calls != 1 {
		t.Errorf("renderer calls = %d, want the replay to reuse the journal", renderer.calls)
	}
}
