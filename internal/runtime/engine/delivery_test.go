package runtimeengine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/anggasct/aura/internal/runtime"
	"github.com/anggasct/aura/internal/runtime/channelhost"
	"github.com/anggasct/aura/internal/runtime/ingress"
	"github.com/anggasct/aura/internal/store"
)

var errDeliveryBoom = errors.New("boom delivery")

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type capturingAdapter struct {
	mu         sync.Mutex
	requests   []*runtimechannelhost.DeliveryRequest
	deliverErr error
}

func (a *capturingAdapter) Start(_ context.Context, _ runtimeingress.IngressSink) error {
	return nil
}

func (a *capturingAdapter) Deliver(_ context.Context, req *runtimechannelhost.DeliveryRequest) (runtimechannelhost.ProviderReceipt, error) {
	if req != nil {
		copied := *req
		if req.Parts != nil {
			parts := make([]runtimechannelhost.OutputPart, len(req.Parts))
			copy(parts, req.Parts)
			copied.Parts = parts
		}
		if req.ReplyContext != nil {
			raw := make(json.RawMessage, len(req.ReplyContext))
			copy(raw, req.ReplyContext)
			copied.ReplyContext = raw
		}
		a.mu.Lock()
		a.requests = append(a.requests, &copied)
		a.mu.Unlock()
	}
	if a.deliverErr != nil {
		return runtimechannelhost.ProviderReceipt{}, a.deliverErr
	}
	return runtimechannelhost.ProviderReceipt{ProviderID: "fake", ExternalID: "ext-1", At: time.Now().UTC()}, nil
}

func (a *capturingAdapter) Health(_ context.Context) runtimechannelhost.ChannelHealth {
	return runtimechannelhost.ChannelHealth{Status: runtimechannelhost.ChannelHealthy}
}

func (a *capturingAdapter) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.requests)
}

func (a *capturingAdapter) first() *runtimechannelhost.DeliveryRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.requests) == 0 {
		return nil
	}
	return a.requests[0]
}

func channelTurnRequest(sessionID, turnID string) *runtime.TurnRequest {
	return &runtime.TurnRequest{
		TurnID:       turnID,
		SessionID:    sessionID,
		PrincipalID:  "user-1",
		Origin:       runtime.Origin("discord:test"),
		Parts:        []runtimeingress.InputPart{{Text: "hello"}},
		ReplyContext: json.RawMessage(`{"channel_id":"333","message_id":"999"}`),
	}
}

func textStep(text string) runtime.FakeStep {
	raw := json.RawMessage(`{"text":` + strconv.Quote(text) + `}`)
	return runtime.FakeStep{Kind: runtime.EventKindMessageCompleted, Payload: raw}
}

func TestDeliveryExcludesToolEventText(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		executor := runtime.NewFakeExecutor([]runtime.FakeStep{
			adkTextStep("Looking through notes"),
			textStep("final answer"),
		})
		engine, db, _ := newTestRuntime(t, Config{MaxActiveTurns: 2, MaxPendingTurns: 4}, executor)
		mustCreateSession(t, db, "channel:333")
		capture := &capturingAdapter{}
		engine.SetChannelAdapters([]runtimechannelhost.ChannelPort{capture})

		req := channelTurnRequest("channel:333", "turn-deliver-tool-text")
		if _, err := collect(t, engine, req); err != nil {
			t.Fatalf("Run: %v", err)
		}
		if capture.count() != 1 {
			t.Fatalf("deliveries = %d, want 1", capture.count())
		}
		parts := capture.first().Parts
		if len(parts) != 1 || parts[0].Text != "final answer" {
			t.Fatalf("parts = %+v, want only the final answer", parts)
		}
	})
}

func TestDeliverCompletedTextTurn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		executor := runtime.NewFakeExecutor([]runtime.FakeStep{textStep("hello from model")})
		engine, db, _ := newTestRuntime(t, Config{MaxActiveTurns: 2, MaxPendingTurns: 4}, executor)
		mustCreateSession(t, db, "channel:333")
		capture := &capturingAdapter{}
		engine.SetChannelAdapters([]runtimechannelhost.ChannelPort{capture})

		req := channelTurnRequest("channel:333", "turn-deliver-1")
		events, err := collect(t, engine, req)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if last := events[len(events)-1]; last.Kind != runtime.EventKindTurnCompleted {
			t.Fatalf("terminal kind = %q, want turn.completed", last.Kind)
		}
		if got := capture.count(); got != 1 {
			t.Fatalf("Deliver calls = %d, want 1", got)
		}
		delivered := capture.first()
		if delivered.Channel != "discord:test" {
			t.Errorf("Channel = %q, want discord:test", delivered.Channel)
		}
		if delivered.ConversationID != "channel:333" {
			t.Errorf("ConversationID = %q, want channel:333", delivered.ConversationID)
		}
		if string(delivered.ReplyContext) != `{"channel_id":"333","message_id":"999"}` {
			t.Errorf("ReplyContext = %s, want origin reference", delivered.ReplyContext)
		}
		if len(delivered.Parts) != 1 || delivered.Parts[0].Text != "hello from model" {
			t.Errorf("Parts = %+v, want single hello part", delivered.Parts)
		}
		if delivered.IdempotencyKey != "turn-deliver-1" || delivered.EffectID != "turn-deliver-1" {
			t.Errorf("idempotency = %q/%q, want turn-deliver-1", delivered.IdempotencyKey, delivered.EffectID)
		}
	})
}

func TestReprocessDoesNotRedeliver(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		executor := runtime.NewFakeExecutor([]runtime.FakeStep{textStep("once")})
		engine, db, _ := newTestRuntime(t, Config{MaxActiveTurns: 2, MaxPendingTurns: 4}, executor)
		mustCreateSession(t, db, "channel:333")
		capture := &capturingAdapter{}
		engine.SetChannelAdapters([]runtimechannelhost.ChannelPort{capture})

		req := channelTurnRequest("channel:333", "turn-once")
		req.IdempotencyKey = "ext-once"
		if _, err := collect(t, engine, req); err != nil {
			t.Fatalf("first Run: %v", err)
		}
		if got := capture.count(); got != 1 {
			t.Fatalf("first Deliver calls = %d, want 1", got)
		}
		second := channelTurnRequest("channel:333", "turn-once-retry")
		second.IdempotencyKey = "ext-once"
		if _, err := collect(t, engine, second); err != nil {
			t.Fatalf("second Run: %v", err)
		}
		if got := capture.count(); got != 1 {
			t.Fatalf("Deliver calls after reprocess = %d, want 1", got)
		}
		if err := engine.deliverCompletedTurn(context.Background(), req, nil); err != nil {
			code, ok := runtime.CodeOf(err)
			if !ok {
				t.Fatalf("re-deliver returned untyped %v", err)
			}
			_ = code
		}
		if got := capture.count(); got != 1 {
			t.Fatalf("Deliver calls after direct reprocess = %d, want 1", got)
		}
		if executor.StartCount() != 1 {
			t.Errorf("executor starts = %d, want 1", executor.StartCount())
		}
	})
}

func TestDeliveryFailureMarksTurnFailed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		executor := runtime.NewFakeExecutor([]runtime.FakeStep{textStep("needs reply")})
		engine, db, _ := newTestRuntime(t, Config{MaxActiveTurns: 2, MaxPendingTurns: 4}, executor)
		mustCreateSession(t, db, "channel:333")
		var logs lockedBuffer
		engine.logger = slog.New(slog.NewTextHandler(&logs, nil))
		capture := &capturingAdapter{deliverErr: errDeliveryBoom}
		engine.SetChannelAdapters([]runtimechannelhost.ChannelPort{capture})

		req := channelTurnRequest("channel:333", "turn-fail-delivery")
		events, err := collect(t, engine, req)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		last := events[len(events)-1]
		if last.Kind != runtime.EventKindTurnFailed {
			t.Fatalf("terminal kind = %q, want turn.failed", last.Kind)
		}
		var payload struct {
			Code runtime.ErrorCode `json:"code"`
		}
		if err := json.Unmarshal(last.Payload, &payload); err != nil {
			t.Fatalf("decode terminal payload: %v", err)
		}
		if payload.Code != runtime.ErrorCodeDeliveryFailed {
			t.Fatalf("terminal code = %q, want delivery_failed", payload.Code)
		}
		if got := capture.count(); got != 1 {
			t.Fatalf("Deliver calls = %d, want 1 without blind retry", got)
		}
		logged := logs.String()
		if !strings.Contains(logged, "turn delivery failed") || !strings.Contains(logged, "turn-fail-delivery") || !strings.Contains(logged, "boom delivery") {
			t.Errorf("missing error log carrying the delivery failure: %q", logged)
		}
		stored, err := engine.dedupe.ListTurnEvents(context.Background(), "turn-fail-delivery")
		if err != nil {
			t.Fatalf("ListTurnEvents: %v", err)
		}
		var completed, failed int
		var messagePayload json.RawMessage
		for _, ev := range stored {
			switch ev.Kind {
			case runtime.EventKindTurnCompleted:
				completed++
			case runtime.EventKindTurnFailed:
				failed++
			case runtime.EventKindMessageCompleted:
				messagePayload = ev.Payload
			}
		}
		if completed != 0 || failed != 1 {
			t.Errorf("stored completed=%d failed=%d, want 0 and 1", completed, failed)
		}
		if string(messagePayload) == "" {
			t.Error("model text event missing after failed delivery")
		}
		var decoded map[string]string
		if err := json.Unmarshal(messagePayload, &decoded); err != nil || decoded["text"] != "needs reply" {
			t.Errorf("history payload = %s, want original text intact", messagePayload)
		}
	})
}

func TestEmptyTextSkipsDelivery(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		executor := runtime.NewFakeExecutor([]runtime.FakeStep{{Kind: runtime.EventKindMessageCompleted, Payload: json.RawMessage(`{}`)}})
		engine, db, _ := newTestRuntime(t, Config{MaxActiveTurns: 2, MaxPendingTurns: 4}, executor)
		mustCreateSession(t, db, "channel:333")
		capture := &capturingAdapter{}
		engine.SetChannelAdapters([]runtimechannelhost.ChannelPort{capture})

		req := channelTurnRequest("channel:333", "turn-empty")
		events, err := collect(t, engine, req)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if last := events[len(events)-1]; last.Kind != runtime.EventKindTurnCompleted {
			t.Fatalf("terminal kind = %q, want turn.completed", last.Kind)
		}
		if got := capture.count(); got != 0 {
			t.Fatalf("Deliver calls = %d, want 0", got)
		}
		skipErr := engine.deliverCompletedTurn(context.Background(), &runtime.TurnRequest{
			TurnID:       "turn-empty-direct",
			SessionID:    "channel:333",
			Origin:       runtime.Origin("discord:test"),
			ReplyContext: json.RawMessage(`{"channel_id":"333","message_id":"999"}`),
		}, []store.RuntimeEvent{{Kind: runtime.EventKindMessageCompleted, Payload: json.RawMessage(`{}`)}})
		code, ok := runtime.CodeOf(skipErr)
		if !ok || code != runtime.ErrorCodeDeliverySkipped {
			t.Fatalf("skip code = %q (ok=%v), want delivery_skipped", code, ok)
		}
	})
}

func TestMissingAdapterFailsChannelTurn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		executor := runtime.NewFakeExecutor([]runtime.FakeStep{textStep("hello")})
		engine, db, _ := newTestRuntime(t, Config{MaxActiveTurns: 2, MaxPendingTurns: 4}, executor)
		mustCreateSession(t, db, "channel:333")

		req := channelTurnRequest("channel:333", "turn-no-adapter")
		events, err := collect(t, engine, req)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if last := events[len(events)-1]; last.Kind != runtime.EventKindTurnFailed {
			t.Fatalf("terminal kind = %q, want turn.failed", last.Kind)
		}
		var payload struct {
			Code runtime.ErrorCode `json:"code"`
		}
		if err := json.Unmarshal(events[len(events)-1].Payload, &payload); err != nil {
			t.Fatalf("decode terminal payload: %v", err)
		}
		if payload.Code != runtime.ErrorCodeDeliveryFailed {
			t.Fatalf("terminal code = %q, want delivery_failed", payload.Code)
		}
	})
}

func TestTerminalTurnSkipsWithoutAdapter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		executor := runtime.NewFakeExecutor([]runtime.FakeStep{textStep("terminal text")})
		engine, db, _ := newTestRuntime(t, Config{MaxActiveTurns: 2, MaxPendingTurns: 4}, executor)
		mustCreateSession(t, db, "session-a")

		req := sampleRequest("session-a", "turn-terminal-skip")
		events, err := collect(t, engine, req)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if last := events[len(events)-1]; last.Kind != runtime.EventKindTurnCompleted {
			t.Fatalf("terminal kind = %q, want turn.completed", last.Kind)
		}
	})
}

func TestDeliveryExtractsADKModelText(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		adkPayload := json.RawMessage(`{"content":{"role":"model","parts":[{"text":"adk hello"}]},"partial":false}`)
		executor := runtime.NewFakeExecutor([]runtime.FakeStep{{Kind: store.EventKindADK, Payload: adkPayload}})
		engine, db, _ := newTestRuntime(t, Config{MaxActiveTurns: 2, MaxPendingTurns: 4}, executor)
		mustCreateSession(t, db, "channel:333")
		capture := &capturingAdapter{}
		engine.SetChannelAdapters([]runtimechannelhost.ChannelPort{capture})

		req := channelTurnRequest("channel:333", "turn-adk-text")
		events, err := collect(t, engine, req)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if last := events[len(events)-1]; last.Kind != runtime.EventKindTurnCompleted {
			t.Fatalf("terminal kind = %q, want turn.completed", last.Kind)
		}
		if got := capture.count(); got != 1 {
			t.Fatalf("Deliver calls = %d, want 1", got)
		}
		if parts := capture.first().Parts; len(parts) != 1 || parts[0].Text != "adk hello" {
			t.Errorf("Parts = %+v, want adk hello", parts)
		}
	})
}

func TestDeliveryRetryAfterFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		executor := runtime.NewFakeExecutor(nil)
		engine, db, _ := newTestRuntime(t, Config{MaxActiveTurns: 2, MaxPendingTurns: 4}, executor)
		mustCreateSession(t, db, "channel:333")
		capture := &capturingAdapter{deliverErr: errDeliveryBoom}
		engine.SetChannelAdapters([]runtimechannelhost.ChannelPort{capture})

		req := channelTurnRequest("channel:333", "turn-retry-failure")
		events := []store.RuntimeEvent{{Kind: runtime.EventKindMessageCompleted, Payload: json.RawMessage(`{"text":"needs reply"}`)}}

		firstErr := engine.deliverCompletedTurn(context.Background(), req, events)
		if code, ok := runtime.CodeOf(firstErr); !ok || code != runtime.ErrorCodeDeliveryFailed {
			t.Fatalf("first code = %q (ok=%v), want delivery_failed", code, ok)
		}
		if got := capture.count(); got != 1 {
			t.Fatalf("first Deliver calls = %d, want 1", got)
		}
		secondErr := engine.deliverCompletedTurn(context.Background(), req, events)
		if code, ok := runtime.CodeOf(secondErr); !ok || code != runtime.ErrorCodeDeliveryFailed {
			t.Fatalf("second code = %q (ok=%v), want delivery_failed retry, never nil-success without delivery", code, ok)
		}
		if got := capture.count(); got != 2 {
			t.Fatalf("second Deliver calls = %d, want 2 (retry must re-attempt)", got)
		}
		capture.deliverErr = nil
		if err := engine.deliverCompletedTurn(context.Background(), req, events); err != nil {
			t.Fatalf("third deliver: %v", err)
		}
		if got := capture.count(); got != 3 {
			t.Fatalf("third Deliver calls = %d, want 3", got)
		}
	})
}

func TestMissingAdapterFailureIsRetryable(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		executor := runtime.NewFakeExecutor(nil)
		engine, db, _ := newTestRuntime(t, Config{MaxActiveTurns: 2, MaxPendingTurns: 4}, executor)
		mustCreateSession(t, db, "channel:333")

		req := channelTurnRequest("channel:333", "turn-no-adapter-retry")
		events := []store.RuntimeEvent{{Kind: runtime.EventKindMessageCompleted, Payload: json.RawMessage(`{"text":"hello"}`)}}
		if err := engine.deliverCompletedTurn(context.Background(), req, events); err == nil {
			t.Fatal("missing adapter must return delivery_failed, got nil")
		} else if code, ok := runtime.CodeOf(err); !ok || code != runtime.ErrorCodeDeliveryFailed {
			t.Fatalf("code = %q (ok=%v), want delivery_failed", code, ok)
		}
		capture := &capturingAdapter{}
		engine.SetChannelAdapters([]runtimechannelhost.ChannelPort{capture})
		if err := engine.deliverCompletedTurn(context.Background(), req, events); err != nil {
			t.Fatalf("retry after adapter configured: %v", err)
		}
		if got := capture.count(); got != 1 {
			t.Fatalf("Deliver calls = %d, want 1 after retry", got)
		}
	})
}

func TestDeliveredEntriesExpireAfterDedupeWindow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		executor := runtime.NewFakeExecutor(nil)
		engine, db, _ := newTestRuntime(t, Config{MaxActiveTurns: 2, MaxPendingTurns: 4}, executor)
		mustCreateSession(t, db, "channel:333")
		capture := &capturingAdapter{}
		engine.SetChannelAdapters([]runtimechannelhost.ChannelPort{capture})

		req := channelTurnRequest("channel:333", "turn-expire")
		events := []store.RuntimeEvent{{Kind: runtime.EventKindMessageCompleted, Payload: json.RawMessage(`{"text":"hello"}`)}}
		if err := engine.deliverCompletedTurn(context.Background(), req, events); err != nil {
			t.Fatalf("first deliver: %v", err)
		}
		if got := capture.count(); got != 1 {
			t.Fatalf("first Deliver calls = %d, want 1", got)
		}
		if err := engine.deliverCompletedTurn(context.Background(), req, events); err != nil {
			t.Fatalf("deduped redeliver: %v", err)
		}
		if got := capture.count(); got != 1 {
			t.Fatalf("Deliver calls after dedupe = %d, want 1", got)
		}
		engine.mu.Lock()
		engine.delivered[req.TurnID] = time.Now().UTC().Add(-dedupeWindow - time.Minute)
		engine.mu.Unlock()
		if engine.hasDelivered(req.TurnID) {
			t.Fatal("hasDelivered = true for expired entry, want false")
		}
		if err := engine.deliverCompletedTurn(context.Background(), req, events); err != nil {
			t.Fatalf("deliver after expiry: %v", err)
		}
		if got := capture.count(); got != 2 {
			t.Fatalf("Deliver calls after expiry = %d, want 2", got)
		}
	})
}
