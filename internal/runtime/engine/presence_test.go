package runtimeengine

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/anggasct/aura/internal/runtime"
	"github.com/anggasct/aura/internal/runtime/channelhost"
	"github.com/anggasct/aura/internal/runtime/ingress"
	"github.com/anggasct/aura/internal/store"
)

type presenceCapture struct {
	mu       sync.Mutex
	started  int
	updates  []presenceUpdate
	ended    []string
	delivers int
}

type presenceUpdate struct {
	text   string
	failed bool
}

func (a *presenceCapture) Start(_ context.Context, _ runtimeingress.IngressSink) error {
	return nil
}

func (a *presenceCapture) Deliver(_ context.Context, _ *runtimechannelhost.DeliveryRequest) (runtimechannelhost.ProviderReceipt, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.delivers++
	return runtimechannelhost.ProviderReceipt{ProviderID: "fake", ExternalID: "ext-1"}, nil
}

func (a *presenceCapture) Health(_ context.Context) runtimechannelhost.ChannelHealth {
	return runtimechannelhost.ChannelHealth{Status: runtimechannelhost.ChannelHealthy}
}

func (a *presenceCapture) PresenceStarted(_ context.Context, _ *runtimechannelhost.DeliveryRequest) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.started++
	return nil
}

func (a *presenceCapture) PresenceUpdated(_ context.Context, _ *runtimechannelhost.DeliveryRequest, text string, failed bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.updates = append(a.updates, presenceUpdate{text: text, failed: failed})
	return nil
}

func (a *presenceCapture) PresenceEnded(_ context.Context, turnID string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.ended = append(a.ended, turnID)
	return nil
}

func adkTextStep(text string) runtime.FakeStep {
	raw, err := json.Marshal(map[string]any{
		"content": map[string]any{
			"role":  "model",
			"parts": []any{map[string]any{"text": text}, map[string]any{"functionCall": map[string]any{"name": "read_file", "args": map[string]any{}}}},
		},
	})
	if err != nil {
		panic("test payload is not serializable")
	}
	return runtime.FakeStep{Kind: "adk_event", Payload: raw}
}

func TestEngineDrivesPresenceForSlowTurn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		executor := runtime.NewFakeExecutor([]runtime.FakeStep{
			adkTextStep("Looking through notes"),
			textStep("final answer"),
		})
		engine, db, _ := newTestRuntime(t, Config{MaxActiveTurns: 2, MaxPendingTurns: 4}, executor)
		mustCreateSession(t, db, "channel:333")
		capture := &presenceCapture{}
		engine.SetChannelAdapters([]runtimechannelhost.ChannelPort{capture})
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
			if !update.failed && update.text == "Looking through notes" {
				found = true
			}
		}
		if !found {
			t.Fatalf("updates = %+v, want model snippet", capture.updates)
		}
	})
}

func TestEnginePresenceFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		raw, err := json.Marshal(map[string]any{
			"content": map[string]any{
				"role": "model",
				"parts": []any{
					map[string]any{
						"functionResponse": map[string]any{
							"name":     "read_file",
							"response": map[string]any{"error": "boom"},
						},
					},
				},
			},
		})
		if err != nil {
			t.Fatalf("marshal failure fixture: %v", err)
		}
		executor := runtime.NewFakeExecutor([]runtime.FakeStep{
			{Kind: "adk_event", Payload: raw},
			textStep("recovered answer"),
		})
		engine, db, _ := newTestRuntime(t, Config{MaxActiveTurns: 2, MaxPendingTurns: 4}, executor)
		mustCreateSession(t, db, "channel:333")
		capture := &presenceCapture{}
		engine.SetChannelAdapters([]runtimechannelhost.ChannelPort{capture})
		req := channelTurnRequest("channel:333", "turn-presence-fail")
		if _, err := collect(t, engine, req); err != nil {
			t.Fatalf("Run: %v", err)
		}
		capture.mu.Lock()
		defer capture.mu.Unlock()
		failed := false
		for _, update := range capture.updates {
			if update.failed {
				failed = true
			}
		}
		if !failed {
			t.Fatalf("updates = %+v, want a failed presence update", capture.updates)
		}
	})
}

func TestExtractPresenceHelpers(t *testing.T) {
	raw, err := json.Marshal(map[string]any{
		"content": map[string]any{"role": "model", "parts": []any{map[string]any{"text": "Checking tasks"}, map[string]any{"functionCall": map[string]any{"name": "read_file"}}}},
	})
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	ev := &store.RuntimeEvent{Kind: store.EventKindADK, Payload: raw}
	if text, ok := extractPresenceText(ev); !ok || text != "Checking tasks" {
		t.Fatalf("extract = %q,%v", text, ok)
	}
	if isPresenceFailureEvent(ev) {
		t.Fatal("plain text misclassified as failure")
	}
}
