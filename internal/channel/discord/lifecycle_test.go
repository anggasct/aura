package discord

import (
	"context"
	"testing"
	"time"
)

func TestAdapter_ShutdownPromptWithIdleConnection(t *testing.T) {
	gateway := newFakeGateway(t, true)
	sink := newMemorySink()
	resumes := &memoryResumeStore{}
	tc := startTestAdapter(t, gateway, sink, resumes, &logCapture{})
	cancel, done := tc.cancel, tc.done

	ws := gateway.nextConn(5 * time.Second)
	gateway.send(ws, opHello, helloData(3600000), nil, "")
	gateway.nextFrame(5 * time.Second)
	gateway.send(ws, opDispatch, readyData("session-idle", "999"), seqPtr(0), eventReady)
	waitFor(t, 5*time.Second, func() bool {
		cursor, found, _ := resumes.Load(t.Context())
		return found && cursor.SessionID == "session-idle"
	}, "ready cursor")

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start(): %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Start() did not return promptly after cancel on an idle connection")
	}
}

func TestAdapter_IntakeOrderPreserved(t *testing.T) {
	gateway := newFakeGateway(t, true)
	sink := newMemorySink()
	resumes := &memoryResumeStore{}
	tc := startTestAdapter(t, gateway, sink, resumes, &logCapture{})
	cancel, done := tc.cancel, tc.done

	ws := gateway.nextConn(5 * time.Second)
	gateway.send(ws, opHello, helloData(30), nil, "")
	gateway.nextFrame(5 * time.Second)
	gateway.send(ws, opDispatch, readyData("session-order", "999"), seqPtr(0), eventReady)
	gateway.send(ws, opDispatch, messageData("3001", "333", "222", "111", "first", []string{"999"}), seqPtr(1), eventMessageCreate)
	gateway.send(ws, opDispatch, messageData("3002", "333", "222", "111", "second", []string{"999"}), seqPtr(2), eventMessageCreate)
	gateway.send(ws, opDispatch, messageData("3003", "333", "222", "111", "third", []string{"999"}), seqPtr(3), eventMessageCreate)
	waitFor(t, 5*time.Second, func() bool {
		calls, _ := sink.stats("message:3003")
		return calls == 1
	}, "third message")
	sink.mu.Lock()
	order := append([]string(nil), sink.order...)
	sink.mu.Unlock()
	want := []string{"message:3001", "message:3002", "message:3003"}
	if len(order) != len(want) {
		t.Fatalf("admit order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("admit order = %v, want %v", order, want)
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Start() did not return after cancel")
	}
}

func TestAdapter_RetryDelayBounded(t *testing.T) {
	cfg := testAdapterConfig()
	adapter, err := New(&cfg, &memoryResumeStore{}, newFakeEffectRunner(), &fakeMediaStore{}, &fakeSessionEnsurer{}, nil)
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	adapter.retryBase = time.Second
	adapter.retryCap = time.Minute
	for attempt := range 12 {
		delay := adapter.retryDelay(attempt)
		if delay < 500*time.Millisecond || delay > time.Minute {
			t.Errorf("retryDelay(%d) = %v, want within [500ms, 1m]", attempt, delay)
		}
	}
}

func TestDeliver_CancelWhileSaturated(t *testing.T) {
	stub := newApprovalStub(t)
	adapter := newApprovalAdapter(t, stub, nil)
	for range maxDeliverInflight {
		adapter.deliverSem <- struct{}{}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := adapter.Deliver(ctx, deliverTestRequest("e-sat", "discord", "hello")); err == nil {
		t.Error("saturated Deliver with cancelled context accepted")
	}
	for range maxDeliverInflight {
		<-adapter.deliverSem
	}
}
