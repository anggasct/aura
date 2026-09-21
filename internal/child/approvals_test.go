package child

import (
	stdcontext "context"
	"errors"
	"testing"
	"time"
)

type fakeGate struct {
	deny    error
	binding *GrantBinding
}

func (f *fakeGate) Authorize(_ stdcontext.Context, request *ToolRequest, binding *GrantBinding, _ time.Time) error {
	f.binding = binding
	if f.deny != nil {
		return f.deny
	}
	return nil
}

func testSpawn() Spawn {
	return Spawn{
		ID: "ch-1", SessionID: "sess-child", Depth: 1,
		Grants:     []Grant{{Capability: "search"}},
		DurableKey: "child/ch-1", Deadline: time.Now().UTC().Add(time.Minute),
		CreatedAt:       time.Now().UTC(),
		ParentSessionID: "sess-parent", ParentTurnID: "turn-1",
		ParentInvocation: "inv-1", OwnerID: "owner-1",
	}
}

func testToolRequest() *ToolRequest {
	return &ToolRequest{
		RequestID: "req-1", TurnID: "turn-1", SessionID: "sess-child",
		PrincipalID: "owner-1", ToolName: "search", ToolVersion: "v1",
		Capabilities: []string{"search"}, Deadline: time.Now().UTC().Add(time.Minute),
	}
}

func TestCheckChildToolCallBindsLineage(t *testing.T) {
	spawn := testSpawn()
	gate := &fakeGate{}
	check := CheckChildToolCall(&spawn, gate, nil)
	if err := check(t.Context(), testToolRequest()); err != nil {
		t.Fatalf("check: %v", err)
	}
	if gate.binding == nil {
		t.Fatal("gate never saw the call")
	}
	if gate.binding.SessionID != "sess-child" || gate.binding.PrincipalID != "owner-1" {
		t.Errorf("binding = %+v", gate.binding)
	}
	if len(gate.binding.Capabilities) != 1 || gate.binding.Capabilities[0] != "search" {
		t.Errorf("capabilities = %+v", gate.binding.Capabilities)
	}
	if gate.binding.ChildID != "ch-1" || gate.binding.ParentInvocation != "inv-1" {
		t.Errorf("lineage = %+v", gate.binding)
	}
	if gate.binding.ActionDigest == "" {
		t.Error("action digest must not be empty")
	}
	if gate.binding.ExpiresAt.IsZero() {
		t.Error("binding expiry must not be zero")
	}
}

func TestCheckChildToolCallRejects(t *testing.T) {
	spawn := testSpawn()
	for name, request := range map[string]*ToolRequest{
		"cross-session": {RequestID: "r", TurnID: "t", SessionID: "sess-other", PrincipalID: "owner-1", ToolName: "search", ToolVersion: "v1"},
		"spawn-nested":  {RequestID: "r", TurnID: "t", SessionID: "sess-child", PrincipalID: "owner-1", ToolName: "spawn_child", ToolVersion: "v1"},
	} {
		check := CheckChildToolCall(&spawn, &fakeGate{}, nil)
		if err := check(t.Context(), request); err == nil {
			t.Errorf("%s: expected rejection", name)
		}
	}
	denied := CheckChildToolCall(&spawn, &fakeGate{deny: errors.New("policy denies")}, nil)
	if err := denied(t.Context(), testToolRequest()); err == nil {
		t.Error("expected gate denial")
	}
	if err := CheckChildToolCall(nil, &fakeGate{}, nil)(t.Context(), testToolRequest()); err == nil {
		t.Error("expected nil spawn rejection")
	}
	if err := CheckChildToolCall(&spawn, nil, nil)(t.Context(), testToolRequest()); err == nil {
		t.Error("expected nil gate rejection")
	}
	if err := CheckChildToolCall(&spawn, &fakeGate{}, nil)(t.Context(), nil); err == nil {
		t.Error("expected nil request rejection")
	}
}
