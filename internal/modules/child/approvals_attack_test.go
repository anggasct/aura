package child

import (
	"encoding/json"
	"testing"
	"time"
)

type stubClock struct {
	now time.Time
}

func (c *stubClock) Now() time.Time { return c.now }

func (c *stubClock) advance(d time.Duration) { c.now = c.now.Add(d) }

func attackRequest() *ToolRequest {
	return &ToolRequest{
		RequestID: "req-attack", TurnID: "turn-1", SessionID: "sess-child",
		PrincipalID: "owner-1", ToolName: "search", ToolVersion: "v1",
		Arguments:      json.RawMessage(`{"query":"logs"}`),
		Capabilities:   []string{"search"},
		Deadline:       time.Now().UTC().Add(time.Minute),
		IdempotencyKey: "key-attack",
	}
}

func TestApprovalRejectsReplay(t *testing.T) {
	spawn := testSpawn()
	check := CheckChildToolCall(&spawn, &fakeGate{}, nil)
	if err := check(t.Context(), attackRequest()); err != nil {
		t.Fatalf("first use: %v", err)
	}
	if err := check(t.Context(), attackRequest()); err == nil {
		t.Fatal("replayed request identity must fail")
	} else if code, ok := CodeOf(err); !ok || code != ErrorCodeChildConflict {
		t.Fatalf("code = %v, %v (%v)", code, ok, err)
	}
}

func TestApprovalRejectsMutation(t *testing.T) {
	spawn := testSpawn()
	check := CheckChildToolCall(&spawn, &fakeGate{}, nil)
	if err := check(t.Context(), attackRequest()); err != nil {
		t.Fatalf("first use: %v", err)
	}
	mutatedCaps := attackRequest()
	mutatedCaps.Capabilities = []string{"search", "write"}
	if err := check(t.Context(), mutatedCaps); err == nil {
		t.Fatal("mutated capabilities must fail")
	} else if code, ok := CodeOf(err); !ok || code != ErrorCodeChildInvalid {
		t.Fatalf("code = %v, %v (%v)", code, ok, err)
	}
	mutatedArgs := attackRequest()
	mutatedArgs.Capabilities = []string{"search"}
	mutatedArgs.Arguments = json.RawMessage(`{"query":"other"}`)
	fresh := CheckChildToolCall(&spawn, &fakeGate{}, nil)
	if err := fresh(t.Context(), attackRequest()); err != nil {
		t.Fatalf("first use: %v", err)
	}
	if err := fresh(t.Context(), mutatedArgs); err == nil {
		t.Fatal("mutated arguments must fail")
	}
	ungranted := attackRequest()
	ungranted.RequestID = "req-other"
	ungranted.IdempotencyKey = "key-other"
	ungranted.ToolName = "write"
	if err := CheckChildToolCall(&spawn, &fakeGate{}, nil)(t.Context(), ungranted); err == nil {
		t.Fatal("tool outside the grants must fail")
	}
}

func TestApprovalRejectsExpiredDeadlines(t *testing.T) {
	spawn := testSpawn()
	clock := &stubClock{now: time.Now().UTC()}
	check := CheckChildToolCall(&spawn, &fakeGate{}, clock)
	expired := attackRequest()
	expired.RequestID = "req-expired"
	expired.IdempotencyKey = "key-expired"
	expired.Deadline = clock.now.Add(-time.Second)
	if err := check(t.Context(), expired); err == nil {
		t.Fatal("expired request deadline must fail")
	}
	pastSpawn := testSpawn()
	pastSpawn.Deadline = clock.now.Add(-time.Second)
	stale := attackRequest()
	stale.RequestID = "req-stale"
	stale.IdempotencyKey = "key-stale"
	stale.Deadline = clock.now.Add(time.Hour)
	if err := CheckChildToolCall(&pastSpawn, &fakeGate{}, clock)(t.Context(), stale); err == nil {
		t.Fatal("expired child deadline must fail")
	}
}

func TestApprovalEnforcesExpiryAtCallTime(t *testing.T) {
	spawn := testSpawn()
	clock := &stubClock{now: time.Now().UTC()}
	spawn.Deadline = clock.now.Add(30 * time.Second)
	check := CheckChildToolCall(&spawn, &fakeGate{}, clock)
	first := attackRequest()
	first.Deadline = clock.now.Add(time.Hour)
	if err := check(t.Context(), first); err != nil {
		t.Fatalf("use before deadline: %v", err)
	}
	clock.advance(time.Minute)
	second := attackRequest()
	second.RequestID = "req-late"
	second.IdempotencyKey = "key-late"
	second.Deadline = clock.now.Add(time.Hour)
	if err := check(t.Context(), second); err == nil {
		t.Fatal("call after the child deadline must fail")
	}
}

func TestApprovalRejectsSelfApproval(t *testing.T) {
	spawn := testSpawn()
	for name, principal := range map[string]string{
		"child id":      "ch-1",
		"child session": "sess-child",
		"other owner":   "owner-2",
		"empty":         "",
	} {
		request := attackRequest()
		request.RequestID = "req-" + name
		request.IdempotencyKey = "key-" + name
		request.PrincipalID = principal
		err := CheckChildToolCall(&spawn, &fakeGate{}, nil)(t.Context(), request)
		if err == nil {
			t.Errorf("%s: forbidden principal must fail", name)
			continue
		}
		if code, ok := CodeOf(err); !ok || code != ErrorCodeChildForbidden {
			t.Errorf("%s: code = %v, %v (%v)", name, code, ok, err)
		}
	}
}

func TestApprovalBindingIgnoresRequestEcho(t *testing.T) {
	spawn := testSpawn()
	gate := &fakeGate{}
	check := CheckChildToolCall(&spawn, gate, nil)
	request := attackRequest()
	request.ToolVersion = "v9"
	if err := check(t.Context(), request); err != nil {
		t.Fatalf("check: %v", err)
	}
	if gate.binding.PrincipalID != spawn.OwnerID {
		t.Errorf("principal = %q, want durable %q", gate.binding.PrincipalID, spawn.OwnerID)
	}
	if gate.binding.ChildID != spawn.ID || gate.binding.ParentInvocation != spawn.ParentInvocation {
		t.Errorf("lineage = %+v", gate.binding)
	}
	if len(gate.binding.Capabilities) != len(spawn.Grants) {
		t.Fatalf("capabilities = %+v", gate.binding.Capabilities)
	}
	for i, capability := range gate.binding.Capabilities {
		if capability != spawn.Grants[i].Capability {
			t.Fatalf("capabilities = %+v, want durable grants", gate.binding.Capabilities)
		}
	}
	want := actionDigest(request.ToolName, request.ToolVersion, request.Arguments, request.Capabilities)
	if gate.binding.ActionDigest != want {
		t.Errorf("digest = %q, want %q", gate.binding.ActionDigest, want)
	}
}
