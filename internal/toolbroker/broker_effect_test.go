package toolbroker

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/approval"
	"github.com/anggasct/aura/internal/capability/capabilitytest"
	"github.com/anggasct/aura/internal/effect"
	"github.com/anggasct/aura/internal/store"
	"github.com/anggasct/aura/internal/store/storetest"
)

func newBrokerEffectExecutor(t *testing.T) *effect.Executor {
	t.Helper()
	db := storetest.OpenMigrated(t)
	now := time.Now().UTC()
	if err := store.NewSessionService(db).Create(context.Background(), &store.Session{
		ID: "session-1", OwnerID: "owner-1", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	journal, err := effect.NewJournal(db, effect.Options{})
	if err != nil {
		t.Fatalf("new journal: %v", err)
	}
	executor, err := effect.NewExecutor(journal)
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	return executor
}

func TestBrokerEffectfulToolUsesDurableLifecycle(t *testing.T) {
	executor := newBrokerEffectExecutor(t)
	calls := 0
	broker, err := New(&Options{
		HostCapabilities: capabilitytest.Host(t), Effects: executor,
		Adapters: map[string]Adapter{
			"exec@v1": func(context.Context, *ToolRequest, approval.Constraints) (ToolResult, error) {
				calls++
				return ToolResult{Output: json.RawMessage(`{"ok":true}`)}, nil
			},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	request := brokerRequest("exec", `{"command":["true"]}`, "exec-linux")
	request.EventSequence = 1
	grant, err := broker.Grant(context.Background(), request, time.Minute)
	if err != nil {
		t.Fatalf("Grant: %v", err)
	}
	request.Approval = &grant
	result, err := broker.Execute(context.Background(), request)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Class != ResultOK || calls != 1 {
		t.Fatalf("result = %+v, adapter calls = %d", result, calls)
	}
	intents, err := executor.Journal().ListByState(context.Background(), effect.StateSucceeded, 0)
	if err != nil {
		t.Fatalf("list succeeded intents: %v", err)
	}
	if len(intents) != 1 || intents[0].Operation != "exec" || intents[0].Classification != effect.ClassificationEffectful {
		t.Fatalf("succeeded intents = %+v", intents)
	}
}

func TestBrokerEffectfulFailureSettlesByWhetherTheEffectWasPerformed(t *testing.T) {
	cases := []struct {
		name      string
		failure   error
		wantState effect.State
	}{
		{"plain adapter error settles unknown", errors.New("transport broke"), effect.StateUnknown},
		{"not performed settles failed", NotPerformed("backend_unavailable", Errorf(ResultExecutionFailed, "backend unavailable")), effect.StateFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			executor := newBrokerEffectExecutor(t)
			calls := 0
			broker, err := New(&Options{
				HostCapabilities: capabilitytest.Host(t), Effects: executor,
				Adapters: map[string]Adapter{
					"exec@v1": func(context.Context, *ToolRequest, approval.Constraints) (ToolResult, error) {
						calls++
						return ToolResult{}, tc.failure
					},
				},
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			request := brokerRequest("exec", `{"command":["true"]}`, "exec-linux")
			request.EventSequence = 1
			grant, err := broker.Grant(context.Background(), request, time.Minute)
			if err != nil {
				t.Fatalf("Grant: %v", err)
			}
			request.Approval = &grant
			_, err = broker.Execute(context.Background(), request)
			if class, ok := CodeOf(err); !ok || class != ResultExecutionFailed {
				t.Fatalf("Execute error = %v, want execution_failed", err)
			}
			if calls != 1 {
				t.Fatalf("adapter calls = %d, want exactly one attempt", calls)
			}
			intents, err := executor.Journal().ListByState(context.Background(), tc.wantState, 0)
			if err != nil {
				t.Fatalf("list intents: %v", err)
			}
			if len(intents) != 1 {
				t.Fatalf("intents in %s = %d, want 1", tc.wantState, len(intents))
			}
			if tc.wantState == effect.StateFailed && intents[0].SafeErrorCode != "backend_unavailable" {
				t.Fatalf("safe error code = %q", intents[0].SafeErrorCode)
			}
		})
	}
}

func TestBrokerEffectReceiptCarriesTheAdapterAudit(t *testing.T) {
	cases := []struct {
		name      string
		audit     json.RawMessage
		wantAudit string
	}{
		{"valid audit is embedded", json.RawMessage(`{"runtime":"python","result_class":"ok"}`), `{"runtime":"python","result_class":"ok"}`},
		{"missing audit is omitted", nil, ""},
		{"invalid audit is dropped and the effect still settles", json.RawMessage(`{"runtime":`), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			executor := newBrokerEffectExecutor(t)
			broker, err := New(&Options{
				HostCapabilities: capabilitytest.Host(t), Effects: executor,
				Adapters: map[string]Adapter{
					"exec@v1": func(context.Context, *ToolRequest, approval.Constraints) (ToolResult, error) {
						return ToolResult{Output: json.RawMessage(`{"ok":true}`), Audit: tc.audit}, nil
					},
				},
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			request := brokerRequest("exec", `{"command":["true"]}`, "exec-linux")
			request.EventSequence = 1
			grant, err := broker.Grant(context.Background(), request, time.Minute)
			if err != nil {
				t.Fatalf("Grant: %v", err)
			}
			request.Approval = &grant
			result, err := broker.Execute(context.Background(), request)
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if encoded, err := json.Marshal(result); err != nil || containsKey(encoded, "audit") {
				t.Fatalf("the audit must stay out of the tool result: %s %v", encoded, err)
			}
			intents, err := executor.Journal().ListByState(context.Background(), effect.StateSucceeded, 0)
			if err != nil || len(intents) != 1 {
				t.Fatalf("succeeded intents = %d, err = %v", len(intents), err)
			}
			var receipt struct {
				Audit json.RawMessage `json:"audit"`
			}
			if err := json.Unmarshal(intents[0].ProviderReceipt, &receipt); err != nil {
				t.Fatalf("receipt %s: %v", intents[0].ProviderReceipt, err)
			}
			if string(receipt.Audit) != tc.wantAudit {
				t.Fatalf("receipt audit = %q, want %q", receipt.Audit, tc.wantAudit)
			}
		})
	}
}

func containsKey(encoded []byte, key string) bool {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		return false
	}
	_, present := fields[key]
	return present
}
