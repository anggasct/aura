//go:build durable

package restate

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"github.com/anggasct/aura/internal/durable"
	runtimesessions "github.com/anggasct/aura/internal/runtime/sessions"
	"github.com/anggasct/aura/internal/store"
)

func callSession(t *testing.T, adapter *Adapter, sessionID, handler string, payload any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal %s request: %v", handler, err)
	}
	out, err := adapter.Call(context.Background(), durable.CallRequest{
		Service: SessionServiceName,
		Key:     sessionID,
		Handler: handler,
		Payload: raw,
	})
	if err != nil {
		t.Fatalf("call %s: %v", handler, err)
	}
	return json.RawMessage(out)
}

func TestLiveSessionTurnsSurviveCrash(t *testing.T) {
	binary := os.Getenv("AURA_RESTATE_BINARY")
	if binary == "" {
		t.Skip("AURA_RESTATE_BINARY is not set")
	}
	if _, err := os.Stat(binary); err != nil {
		t.Skipf("restate-server binary is not available: %v", err)
	}
	ctx := context.Background()
	db := liveTestDB(t)
	if err := store.NewSessionService(db).Create(ctx, &store.Session{ID: "session-1", OwnerID: "user-1"}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	events := store.NewEventStore(db)
	dedupe := store.NewDedupeStore(db)

	dir := t.TempDir()
	ingressPort, adminPort := freePort(t), freePort(t)
	handlerPort := freePort(t)
	handlerAddr := fmt.Sprintf("127.0.0.1:%d", handlerPort)
	ingressURL := fmt.Sprintf("http://127.0.0.1:%d", ingressPort)

	server := startLiveServer(t, binary, dir, ingressPort, adminPort)
	t.Cleanup(func() { server.stop() })

	var endpoint *Endpoint
	var stopEndpoint context.CancelFunc
	startEndpoint := func() {
		var err error
		endpoint, err = NewEndpoint(EndpointConfig{HandlerAddr: handlerAddr}, nil)
		if err != nil {
			t.Fatalf("NewEndpoint: %v", err)
		}
		endpoint.RegisterSessionTurns(SessionStores{Events: events, Dedupe: dedupe})
		endpointCtx, stop := context.WithCancel(context.Background())
		stopEndpoint = stop
		done := make(chan error, 1)
		go func() { done <- endpoint.Start(endpointCtx) }()
		t.Cleanup(func() { stop(); <-done })
		waitEndpointReady(t, endpoint)
		server.registerDeployment(t, handlerAddr)
	}
	startEndpoint()

	adapter, err := NewAdapter(Config{IngressURL: ingressURL}, nil)
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}

	admit := func(turnID, key string) runtimesessions.AdmitResult {
		t.Helper()
		raw := callSession(t, adapter, "session-1", SessionAdmitHandler, runtimesessions.AdmitRequest{
			Turn: runtimesessions.Descriptor{
				TurnID:         turnID,
				SessionID:      "session-1",
				PrincipalID:    "user-1",
				Origin:         "terminal",
				Parts:          []runtimesessions.Part{{Text: "hello"}},
				IdempotencyKey: key,
			},
			AcceptedEventID: "event-" + turnID,
			MaxPending:      16,
		})
		var result runtimesessions.AdmitResult
		if err := json.Unmarshal(raw, &result); err != nil {
			t.Fatalf("decode admit result: %v", err)
		}
		return result
	}
	release := func(turnID string) runtimesessions.ReleaseResult {
		t.Helper()
		raw := callSession(t, adapter, "session-1", SessionReleaseHandler, runtimesessions.ReleaseRequest{TurnID: turnID})
		var result runtimesessions.ReleaseResult
		if err := json.Unmarshal(raw, &result); err != nil {
			t.Fatalf("decode release result: %v", err)
		}
		return result
	}
	callStatus := func() runtimesessions.StatusResult {
		t.Helper()
		raw := callSession(t, adapter, "session-1", SessionStatusHandler, json.RawMessage(`{}`))
		var result runtimesessions.StatusResult
		if err := json.Unmarshal(raw, &result); err != nil {
			t.Fatalf("decode status result: %v", err)
		}
		return result
	}

	first := admit("turn-1", "key-1")
	if first.Replayed || first.ToStart == nil || first.ToStart.Descriptor.TurnID != "turn-1" {
		t.Fatalf("first admit must grant turn-1, got %+v", first)
	}
	if second := admit("turn-2", "key-2"); second.Replayed || second.ToStart != nil {
		t.Fatalf("second admit must queue, got %+v", second)
	}
	if third := admit("turn-3", "key-3"); third.Replayed || third.ToStart != nil {
		t.Fatalf("third admit must queue, got %+v", third)
	}
	if dup := admit("turn-1-again", "key-1"); !dup.Replayed || dup.OriginalTurnID != "turn-1" {
		t.Fatalf("duplicate key must replay turn-1, got %+v", dup)
	}
	if next := release("turn-1"); next.ToStart == nil || next.ToStart.Descriptor.TurnID != "turn-2" {
		t.Fatalf("release must hand turn-2, got %+v", next)
	}

	server.stop()
	stopEndpoint()
	t.Logf("phase=crashed")

	server = startLiveServer(t, binary, dir, ingressPort, adminPort)
	startEndpoint()
	t.Logf("phase=restarted")

	status := callStatus()
	if status.ActiveTurnID != "turn-2" || status.QueueDepth != 1 {
		t.Fatalf("status after restart = %+v, want active turn-2 with depth 1", status)
	}
	if next := release("turn-2"); next.ToStart == nil || next.ToStart.Descriptor.TurnID != "turn-3" {
		t.Fatalf("release after restart must hand turn-3, got %+v", next)
	}
	if dup := admit("turn-9", "key-1"); !dup.Replayed || dup.OriginalTurnID != "turn-1" {
		t.Fatalf("dedupe must survive the crash, got %+v", dup)
	}
	if next := release("turn-3"); next.ToStart != nil {
		t.Fatalf("final release must hand nothing, got %+v", next)
	}

	rows, err := db.QueryContext(ctx, `SELECT sequence FROM runtime_event WHERE session_id = ? ORDER BY sequence`, "session-1")
	if err != nil {
		t.Fatalf("list sequences: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var last uint64
	var count int
	for rows.Next() {
		var sequence uint64
		if err := rows.Scan(&sequence); err != nil {
			t.Fatalf("scan sequence: %v", err)
		}
		if sequence <= last {
			t.Fatalf("sequences not ordered: %d after %d", sequence, last)
		}
		last = sequence
		count++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("list sequences: %v", err)
	}
	if count != 3 {
		t.Fatalf("accepted events = %d, want 3 (duplicate/redelivered admits persist nothing extra)", count)
	}
}
