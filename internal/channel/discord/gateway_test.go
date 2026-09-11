package discord

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/websocket"

	"github.com/anggasct/aura/internal/health"
	runtimechannelhost "github.com/anggasct/aura/internal/runtime/channelhost"
)

func TestAdapter_AdmitsAllowlistedMessage(t *testing.T) {
	gateway := newFakeGateway(t, true)
	sink := newMemorySink()
	resumes := &memoryResumeStore{}
	logs := &logCapture{}
	adapter, cancel, done := startTestAdapter(t, gateway, sink, resumes, logs)
	defer cancel()

	ws := gateway.nextConn(5 * time.Second)
	gateway.send(ws, opHello, helloData(30), nil, "")
	identify := gateway.nextFrame(5 * time.Second)
	if frameOp(t, identify) != opIdentify {
		t.Fatalf("first client frame op = %v, want identify", identify["op"])
	}
	payload, _ := identify["d"].(map[string]any)
	if payload["token"] != testToken {
		t.Error("identify carries wrong token")
	}
	if intents, _ := payload["intents"].(float64); int(intents) != requiredIntents {
		t.Errorf("identify intents = %v, want %d", intents, requiredIntents)
	}

	gateway.send(ws, opDispatch, readyData("session-a", "999"), seqPtr(0), eventReady)
	gateway.send(ws, opDispatch, messageData("1001", "333", "222", "111", "hello aura", []string{"999"}), seqPtr(1), eventMessageCreate)

	waitFor(t, 5*time.Second, func() bool {
		calls, _ := sink.stats("message:1001")
		return calls == 1
	}, "admitted message")

	if got := adapter.Health(t.Context()); got.Status != runtimechannelhost.ChannelHealthy {
		t.Errorf("health = %+v, want healthy", got)
	}
	cursor, found, err := resumes.Load(t.Context())
	if err != nil || !found {
		t.Fatalf("cursor Load(): %v, found = %v", err, found)
	}
	if cursor.SessionID != "session-a" || cursor.Sequence != 1 {
		t.Errorf("cursor = %+v, want session-a seq 1", cursor)
	}
	if cursor.ConfigDigest == "" {
		t.Error("cursor carries no config digest")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start(): %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start() did not return after cancel")
	}
	if strings.Contains(logs.String(), testToken) {
		t.Error("bot token leaked into logs")
	}
}

func TestAdapter_ReplayAcrossRestartAdmitsOneTurn(t *testing.T) {
	gateway := newFakeGateway(t, true)
	sink := newMemorySink()
	resumes := &memoryResumeStore{}
	_, cancel, done := startTestAdapter(t, gateway, sink, resumes, &logCapture{})

	first := gateway.nextConn(5 * time.Second)
	gateway.send(first, opHello, helloData(30), nil, "")
	gateway.nextFrame(5 * time.Second)
	gateway.send(first, opDispatch, readyData("session-a", "999"), seqPtr(0), eventReady)
	gateway.send(first, opDispatch, messageData("1001", "333", "222", "111", "hello", []string{"999"}), seqPtr(1), eventMessageCreate)
	gateway.send(first, opDispatch, messageData("1002", "333", "222", "111", "again", []string{"999"}), seqPtr(2), eventMessageCreate)
	waitFor(t, 5*time.Second, func() bool {
		calls, _ := sink.stats("message:1002")
		return calls == 1
	}, "second message")
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("first Start() did not return after cancel")
	}

	_, secondCancel, secondDone := startTestAdapter(t, gateway, sink, resumes, &logCapture{})
	defer secondCancel()
	second := gateway.nextConn(5 * time.Second)
	gateway.send(second, opHello, helloData(30), nil, "")
	resumeFrame := gateway.nextFrame(5 * time.Second)
	if frameOp(t, resumeFrame) != opResume {
		t.Fatalf("reconnect frame op = %v, want resume", resumeFrame["op"])
	}
	payload, _ := resumeFrame["d"].(map[string]any)
	if payload["session_id"] != "session-a" {
		t.Errorf("resume session = %v, want session-a", payload["session_id"])
	}
	if seq, _ := payload["seq"].(float64); int64(seq) != 2 {
		t.Errorf("resume seq = %v, want 2", seq)
	}
	gateway.send(second, opDispatch, messageData("1002", "333", "222", "111", "again", []string{"999"}), seqPtr(2), eventMessageCreate)
	waitFor(t, 5*time.Second, func() bool {
		calls, _ := sink.stats("message:1002")
		return calls == 2
	}, "redelivered message")
	secondCancel()
	select {
	case <-secondDone:
	case <-time.After(5 * time.Second):
		t.Fatal("second Start() did not return after cancel")
	}
	if calls, turns := sink.stats("message:1002"); calls != 2 || turns != 1 {
		t.Errorf("calls = %d, turns = %d; want 2 calls and 1 turn", calls, turns)
	}
}

func TestAdapter_InvalidSessionStartsFreshAndReportsGap(t *testing.T) {
	gateway := newFakeGateway(t, true)
	sink := newMemorySink()
	resumes := &memoryResumeStore{}
	logs := &logCapture{}
	adapter, cancel, done := startTestAdapter(t, gateway, sink, resumes, logs)
	defer cancel()

	first := gateway.nextConn(5 * time.Second)
	gateway.send(first, opHello, helloData(30), nil, "")
	gateway.nextFrame(5 * time.Second)
	gateway.send(first, opDispatch, readyData("session-a", "999"), seqPtr(0), eventReady)
	gateway.send(first, opDispatch, messageData("1001", "333", "222", "111", "hello", []string{"999"}), seqPtr(1), eventMessageCreate)
	waitFor(t, 5*time.Second, func() bool {
		calls, _ := sink.stats("message:1001")
		return calls == 1
	}, "first message")

	gateway.send(first, opInvalidSession, false, nil, "")
	second := gateway.nextConn(5 * time.Second)
	gateway.send(second, opHello, helloData(30), nil, "")
	reframe := gateway.nextFrame(5 * time.Second)
	if frameOp(t, reframe) != opIdentify {
		t.Fatalf("post-invalid frame op = %v, want fresh identify", reframe["op"])
	}
	gateway.send(second, opDispatch, readyData("session-b", "999"), seqPtr(0), eventReady)
	waitFor(t, 5*time.Second, func() bool {
		return adapter.Health(t.Context()).Status == runtimechannelhost.ChannelDegraded
	}, "degraded health")

	matched := false
	for _, finding := range adapter.Check(t.Context()) {
		if finding.Code == "possible_ingress_gap" && finding.Component == "discord" {
			matched = true
			if finding.Status != health.StatusDegraded {
				t.Errorf("finding status = %v", finding.Status)
			}
			if strings.Contains(finding.Detail, "1001") {
				t.Error("finding detail carries event content")
			}
		}
	}
	if !matched {
		t.Fatal("possible_ingress_gap finding missing")
	}
	if strings.Contains(logs.String(), testToken) {
		t.Error("bot token leaked into logs")
	}
	select {
	case err := <-done:
		t.Fatalf("Start() returned early: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestAdapter_StaleDigestStartsFresh(t *testing.T) {
	gateway := newFakeGateway(t, true)
	sink := newMemorySink()
	resumes := &memoryResumeStore{}
	resumes.cursor = ResumeCursor{SessionID: "old-session", Sequence: 9, ConfigDigest: "stale", UpdatedAt: time.Now().UTC()}
	resumes.found = true
	_, cancel, done := startTestAdapter(t, gateway, sink, resumes, &logCapture{})
	defer cancel()

	fresh := gateway.nextConn(5 * time.Second)
	gateway.send(fresh, opHello, helloData(30), nil, "")
	frame := gateway.nextFrame(5 * time.Second)
	if frameOp(t, frame) != opIdentify {
		t.Fatalf("stale-digest frame op = %v, want fresh identify", frame["op"])
	}
	select {
	case err := <-done:
		t.Fatalf("Start() returned early: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestAdapter_MissedHeartbeatReconnectsWithResume(t *testing.T) {
	gateway := newFakeGateway(t, true)
	sink := newMemorySink()
	resumes := &memoryResumeStore{}
	_, cancel, done := startTestAdapter(t, gateway, sink, resumes, &logCapture{})
	defer cancel()

	first := gateway.nextConn(5 * time.Second)
	gateway.send(first, opHello, helloData(30), nil, "")
	gateway.nextFrame(5 * time.Second)
	gateway.send(first, opDispatch, readyData("session-a", "999"), seqPtr(0), eventReady)
	gateway.send(first, opDispatch, messageData("1001", "333", "222", "111", "hello", []string{"999"}), seqPtr(1), eventMessageCreate)
	waitFor(t, 5*time.Second, func() bool {
		calls, _ := sink.stats("message:1001")
		return calls == 1
	}, "first message")
	gateway.setAutoAck(false)
	_ = first
	zombie := gateway.nextConn(5 * time.Second)
	gateway.send(zombie, opHello, helloData(30), nil, "")
	frame := gateway.nextFrame(5 * time.Second)
	if frameOp(t, frame) != opResume {
		t.Fatalf("zombie reconnect op = %v, want resume", frame["op"])
	}
	select {
	case err := <-done:
		t.Fatalf("Start() returned early: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestAdapter_SelfLookupFailureDegradesGuildMentions(t *testing.T) {
	gateway := newFakeGateway(t, true)
	gateway.selfStatus.Store(http.StatusInternalServerError)
	sink := newMemorySink()
	resumes := &memoryResumeStore{}
	resumes.cursor = ResumeCursor{SessionID: "old-session", Sequence: 4, ConfigDigest: "", UpdatedAt: time.Now().UTC()}
	resumes.found = true
	_, cancel, done := startTestAdapter(t, gateway, sink, resumes, &logCapture{})
	defer cancel()

	ws := gateway.nextConn(5 * time.Second)
	gateway.send(ws, opHello, helloData(30), nil, "")
	frame := gateway.nextFrame(5 * time.Second)
	if frameOp(t, frame) != opResume {
		t.Fatalf("reconnect frame op = %v, want resume", frame["op"])
	}
	gateway.send(ws, opDispatch, messageData("2001", "777", "", "111", "dm when blind", nil), seqPtr(5), eventMessageCreate)
	gateway.send(ws, opDispatch, messageData("2002", "333", "222", "111", "mention when blind", []string{"999"}), seqPtr(6), eventMessageCreate)
	waitFor(t, 5*time.Second, func() bool {
		calls, _ := sink.stats("message:2001")
		return calls == 1
	}, "dm admission")
	time.Sleep(200 * time.Millisecond)
	if calls, _ := sink.stats("message:2002"); calls != 0 {
		t.Errorf("guild mention admitted without self id: calls = %d", calls)
	}
	select {
	case err := <-done:
		t.Fatalf("Start() returned early: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestAdapter_TokenUnavailableNeverDials(t *testing.T) {
	gateway := newFakeGateway(t, true)
	sink := newMemorySink()
	resumes := &memoryResumeStore{}
	cfg := testAdapterConfig()
	cfg.BotTokenRef = "file:///nonexistent-token-file"
	adapter, err := New(&cfg, resumes, nil)
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	adapter.retryBase = 5 * time.Millisecond
	adapter.retryCap = 20 * time.Millisecond
	var dials atomic.Int64
	adapter.dial = func(ctx context.Context, url string) (*websocket.Conn, error) {
		dials.Add(1)
		return nil, errDialBlocked
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- adapter.Start(ctx, sink) }()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start(): %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start() did not return after cancel")
	}
	if dials.Load() != 0 {
		t.Errorf("dials = %d without a token, want 0", dials.Load())
	}
	select {
	case <-gateway.conns:
		t.Fatal("gateway dialed without a token")
	default:
	}
}
