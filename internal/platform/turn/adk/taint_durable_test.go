package runtimeadk

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"google.golang.org/genai"

	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/capability/capabilitytest"
	"github.com/anggasct/aura/internal/platform/durable"
	auramodel "github.com/anggasct/aura/internal/platform/model"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	auratools "github.com/anggasct/aura/internal/platform/tools"
	"github.com/anggasct/aura/internal/platform/turn"
	runtimeingress "github.com/anggasct/aura/internal/platform/turn/ingress"
)

func TestAdmissionTaintIsJournaled(t *testing.T) {
	db, sessions, events := newSessionTestDB(t)
	mustCreateSession(t, db, "session-adm")

	broker, err := toolbroker.New(&toolbroker.Options{HostCapabilities: capabilitytest.Host(t)})
	if err != nil {
		t.Fatalf("toolbroker.New: %v", err)
	}
	probeDef := auratools.Definition{
		Name: "probe_tool", Version: auratools.Version,
		Schema:    json.RawMessage(`{"type":"object"}`),
		Validator: func(raw json.RawMessage) (json.RawMessage, error) { return raw, nil },
	}
	var seenRequests []toolbroker.ToolRequest
	probeRule := toolbroker.DefaultRule(&probeDef)
	if err := broker.RegisterTool(&probeDef, func(_ context.Context, req *toolbroker.ToolRequest, _ approval.Constraints) (toolbroker.ToolResult, error) {
		seenRequests = append(seenRequests, *req)
		return toolbroker.ToolResult{Output: json.RawMessage(`{"ok":true}`)}, nil
	}, &probeRule); err != nil {
		t.Fatalf("RegisterTool: %v", err)
	}
	builtins, err := NewBrokerExecutor(broker, capabilitytest.Host(t))
	if err != nil {
		t.Fatalf("NewBrokerExecutor: %v", err)
	}

	inv := &plantedTaintInvocation{actions: map[string][]byte{}}
	runOnce := func() {
		model := &scriptedStepsModel{
			steps: [][]*genai.Part{
				{callPart("c1", "probe_tool", map[string]any{})},
				{{Text: "done"}},
			},
		}
		executor, err := NewADKExecutor("aura", model.Name(), auramodel.Set{model.Name(): model}, sessions, builtins, nil, slog.New(slog.DiscardHandler), WithBuiltinToolExecutor(builtins))
		if err != nil {
			t.Fatalf("NewADKExecutor: %v", err)
		}
		ctx := durable.WithTurnScope(t.Context(), durable.NewTurnScope(inv))
		for _, err := range executor.Execute(ctx, &turn.TurnRequest{
			TurnID: "turn-adm", SessionID: "session-adm", PrincipalID: principal.OwnerID,
			PrincipalClass: principal.ClassOwner, Origin: turn.OriginTerminal,
			Parts: []runtimeingress.InputPart{{Text: "hi"}},
		}) {
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
		}
	}

	runOnce()
	raw, ok := inv.actions[admissionTaintAction]
	if !ok {
		t.Fatalf("durable actions missing %q: %v", admissionTaintAction, inv.actions)
	}
	rec, valid := decodeAdmissionTaintRecord(raw)
	if !valid || rec.Tainted {
		t.Fatalf("journaled %q = %+v (valid=%v), want clean admission", admissionTaintAction, rec, valid)
	}

	markPayload, _ := turn.EncodeTaintMark(turn.TaintMark{Source: turn.TaintSourceToolResult, Detail: "web_fetch"})
	if _, err := events.AppendSequenced(t.Context(), "session-adm", &turn.Event{
		ID: "late-mark", SessionID: "session-adm", TurnID: "turn-other", Author: "system",
		Kind: turn.EventKindSessionTainted, SchemaVersion: 1, Payload: markPayload,
	}); err != nil {
		t.Fatalf("AppendSequenced: %v", err)
	}

	delete(inv.actions, "call-1-0/taint")
	delete(inv.actions, "call-1-0/execution")
	delete(inv.actions, "call-1-0/policy")
	delete(inv.actions, "call-1-0/authorization")
	delete(inv.actions, "call-1-0/budget")
	runOnce()
	if len(seenRequests) < 2 {
		t.Fatalf("probe_tool calls = %d, want 2", len(seenRequests))
	}
	if seenRequests[1].Tainted {
		t.Fatal("replayed turn ignored its journaled clean taint-admission record")
	}
}

func TestUnreadableAdmissionRecordFailsClosed(t *testing.T) {
	cases := map[string][]byte{
		"empty object":          []byte(`{}`),
		"null":                  []byte(`null`),
		"empty bytes":           []byte(``),
		"null tainted field":    []byte(`{"tainted":null}`),
		"corrupt json":          []byte(`{broken`),
		"unknown field":         []byte(`{"tainted":false,"extra":1}`),
		"invalid source":        []byte(`{"tainted":true,"source":"made_up"}`),
		"clean with non-source": []byte(`{"tainted":false,"source":"web_only"}`),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			db, sessions, _ := newSessionTestDB(t)
			mustCreateSession(t, db, "session-corrupt-adm")

			broker, err := toolbroker.New(&toolbroker.Options{HostCapabilities: capabilitytest.Host(t)})
			if err != nil {
				t.Fatalf("toolbroker.New: %v", err)
			}
			probeDef := auratools.Definition{
				Name: "probe_tool", Version: auratools.Version,
				Schema:    json.RawMessage(`{"type":"object"}`),
				Validator: func(raw json.RawMessage) (json.RawMessage, error) { return raw, nil },
			}
			var seen *toolbroker.ToolRequest
			probeRule := toolbroker.DefaultRule(&probeDef)
			if err := broker.RegisterTool(&probeDef, func(_ context.Context, req *toolbroker.ToolRequest, _ approval.Constraints) (toolbroker.ToolResult, error) {
				copied := *req
				seen = &copied
				return toolbroker.ToolResult{Output: json.RawMessage(`{"ok":true}`)}, nil
			}, &probeRule); err != nil {
				t.Fatalf("RegisterTool: %v", err)
			}
			builtins, err := NewBrokerExecutor(broker, capabilitytest.Host(t))
			if err != nil {
				t.Fatalf("NewBrokerExecutor: %v", err)
			}
			model := &scriptedStepsModel{
				steps: [][]*genai.Part{
					{callPart("c1", "probe_tool", map[string]any{})},
					{{Text: "done"}},
				},
			}
			executor, err := NewADKExecutor("aura", model.Name(), auramodel.Set{model.Name(): model}, sessions, builtins, nil, slog.New(slog.DiscardHandler), WithBuiltinToolExecutor(builtins))
			if err != nil {
				t.Fatalf("NewADKExecutor: %v", err)
			}
			inv := &plantedTaintInvocation{actions: map[string][]byte{
				admissionTaintAction: raw,
			}}
			ctx := durable.WithTurnScope(t.Context(), durable.NewTurnScope(inv))
			for _, err := range executor.Execute(ctx, &turn.TurnRequest{
				TurnID: "turn-corrupt-adm", SessionID: "session-corrupt-adm", PrincipalID: principal.OwnerID,
				PrincipalClass: principal.ClassOwner, Origin: turn.OriginTerminal,
				Parts: []runtimeingress.InputPart{{Text: "hi"}},
			}) {
				if err != nil {
					t.Fatalf("Execute: %v", err)
				}
			}
			if seen == nil {
				t.Fatal("probe_tool did not run")
			}
			if !seen.Tainted || seen.TaintMark.Source != turn.TaintSourceLegacy {
				t.Fatalf("taint = %v %+v, want fail-closed legacy", seen.Tainted, seen.TaintMark)
			}
		})
	}
}
