package chatcommand_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/channels/kit/chatcommand"
	"github.com/anggasct/aura/internal/platform/capability/capabilitytest"
	"github.com/anggasct/aura/internal/platform/command"
	"github.com/anggasct/aura/internal/platform/effect"
	"github.com/anggasct/aura/internal/platform/operation"
	"github.com/anggasct/aura/internal/platform/operation/operationtest"
	"github.com/anggasct/aura/internal/platform/principal"
)

const operationChannel = "testchat"

type noSessions struct{}

func (noSessions) EventCount(context.Context, string) (int, error) { return 0, nil }

func (noSessions) Create(context.Context, *command.Caller) (command.Session, error) {
	return command.Session{}, errors.New("unused")
}

func (noSessions) Switch(context.Context, *command.Caller, string) (command.Session, error) {
	return command.Session{}, errors.New("unused")
}

var errProcessDied = errors.New("the process died after the intent started")

type dyingEffects struct {
	journal *effect.Journal
}

func (d *dyingEffects) Execute(ctx context.Context, req *effect.PrepareRequest, _ effect.Provider) (*effect.Intent, error) {
	intent, err := d.journal.Prepare(ctx, req)
	if err != nil {
		return nil, err
	}
	if _, err := d.journal.Start(ctx, intent.ID); err != nil {
		return nil, err
	}
	return nil, errProcessDied
}

type operationRig struct {
	*operationtest.Rig
	dispatcher *command.Dispatcher
}

func newOperationRig(t *testing.T) *operationRig {
	t.Helper()
	rig := operationtest.NewRig(t, nil)
	chat, err := operation.NewChat(&operation.ChatDeps{Registry: rig.Registry, Capabilities: capabilitytest.Host(t)})
	if err != nil {
		t.Fatalf("NewChat: %v", err)
	}
	groups := operation.ChatGroups([]operation.Catalog{operationtest.Catalog()})
	names := append(command.MessengerNames(), groups[0].Name)
	registry, err := command.NewRegistry(append(command.Catalog(), groups...), command.Binding{Channel: operationChannel, Style: command.StyleMessenger, Names: names})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	dispatcher, err := command.NewDispatcher(registry, command.Ports{Sessions: noSessions{}, Operations: chat}, nil)
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	return &operationRig{Rig: rig, dispatcher: dispatcher}
}

func ownerCaller() *command.Caller {
	return &command.Caller{Channel: operationChannel, PrincipalID: operationtest.OwnerID, Class: principal.ClassOwner, ConversationKey: "conv", SessionID: operationtest.SessionID, AgentID: "main", Direct: true}
}

type sentMessage struct {
	caller    *command.Caller
	text, key string
}

func (r *operationRig) execute(t *testing.T, effects chatcommand.Effects, message *sentMessage) (chatcommand.Outcome, error) {
	t.Helper()
	inv, ok := command.Parse(message.text)
	if !ok {
		t.Fatalf("Parse(%q) is not a command", message.text)
	}
	return chatcommand.Execute(t.Context(), &chatcommand.Run{
		Effects: effects, Dispatcher: r.dispatcher, Logger: slog.New(slog.DiscardHandler),
		Caller: message.caller, Invocation: &inv, IdempotencyKey: message.key, EffectSession: operationtest.SessionID,
	})
}

func (r *operationRig) run(t *testing.T, text, key string) chatcommand.Outcome {
	t.Helper()
	outcome, err := r.execute(t, r.Executor, &sentMessage{caller: ownerCaller(), text: text, key: key})
	if err != nil {
		t.Fatalf("Execute(%q): %v", text, err)
	}
	return outcome
}

func (r *operationRig) dieAfterStart(t *testing.T, text, key string) {
	t.Helper()
	if _, err := r.execute(t, &dyingEffects{journal: r.Executor.Journal()}, &sentMessage{caller: ownerCaller(), text: text, key: key}); !errors.Is(err, errProcessDied) {
		t.Fatalf("the dying run = %v, want the simulated death", err)
	}
}

type journaledIntent struct {
	operation, provider, state, request string
}

func intentOf(t *testing.T, db *sql.DB, key string) journaledIntent {
	t.Helper()
	var row journaledIntent
	err := db.QueryRowContext(t.Context(), `SELECT operation, provider, state, request_json FROM effect_intent WHERE idempotency_key = ?`, key).Scan(&row.operation, &row.provider, &row.state, &row.request)
	if err != nil {
		t.Fatalf("read the intent of %s: %v", key, err)
	}
	return row
}

func TestChatOperationCommands(t *testing.T) {
	rig := newOperationRig(t)

	outcome := rig.run(t, `/stub change "two words"`, "testchat:1")

	if outcome.Receipt == nil || outcome.Receipt.Lines[1] != "operation: "+operationtest.Change {
		t.Fatalf("outcome = %+v, want the rendered operation output", outcome)
	}
	row := intentOf(t, rig.DB, "testchat:1")
	var request chatcommand.Request
	if err := json.Unmarshal([]byte(row.request), &request); err != nil {
		t.Fatalf("request: %v", err)
	}
	if row.provider != chatcommand.Provider || row.operation != operationtest.Change || row.state != string(effect.StateSucceeded) {
		t.Errorf("intent = %+v, want the command provider under the operation id, succeeded", row)
	}
	if request.Command != "stub" || request.Conversation != "conv" || request.Operation != operationtest.Change || string(request.Input) != `{"subject":"two words"}` {
		t.Errorf("request = %+v, want the group, the operation and its canonical input", request)
	}
	var receipts int
	if err := rig.DB.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM operation_receipt WHERE idempotency_key = ? AND surface = 'chat' AND request_digest = ?`, "testchat:1", operation.Digest(request.Input)).Scan(&receipts); err != nil || receipts != 1 {
		t.Errorf("receipts under the message key with the input digest = %d, %v; want 1", receipts, err)
	}
}

func TestChatOperationRedeliveredMessageRunsOnce(t *testing.T) {
	rig := newOperationRig(t)
	first := rig.run(t, "/stub change notes", "testchat:1")
	second := rig.run(t, "/stub change notes", "testchat:1")
	if !slices.Equal(first.Receipt.Lines, second.Receipt.Lines) {
		t.Errorf("second outcome = %+v, want the journaled reply %+v", second.Receipt, first.Receipt)
	}
	if rig.Stubs.Handled(operationtest.Change) != 1 {
		t.Errorf("the handler ran %d times for one message", rig.Stubs.Handled(operationtest.Change))
	}
}

func TestChatOperationResumesStartedIntent(t *testing.T) {
	t.Run("a live lease is reported replayed and runs nothing", func(t *testing.T) {
		rig := newOperationRig(t)
		rig.dieAfterStart(t, "/stub change notes", "testchat:1")

		outcome := rig.run(t, "/stub change notes", "testchat:1")

		if outcome.Receipt != nil || rig.Stubs.Handled(operationtest.Change) != 0 {
			t.Fatalf("outcome = %+v handled %d, want a silent replay that ran nothing", outcome, rig.Stubs.Handled(operationtest.Change))
		}
		if row := intentOf(t, rig.DB, "testchat:1"); row.state != string(effect.StateStarted) {
			t.Errorf("state = %s, want the holder's intent left started", row.state)
		}
	})
	t.Run("an expired lease without a receipt runs the handler once and settles", func(t *testing.T) {
		rig := newOperationRig(t)
		rig.dieAfterStart(t, "/stub change notes", "testchat:1")
		rig.Clock.Advance(61 * time.Second)

		outcome := rig.run(t, "/stub change notes", "testchat:1")

		if outcome.Receipt == nil || outcome.Receipt.Lines[1] != "operation: "+operationtest.Change || rig.Stubs.Handled(operationtest.Change) != 1 {
			t.Fatalf("outcome = %+v handled %d, want one run and its reply", outcome, rig.Stubs.Handled(operationtest.Change))
		}
		if ledger, _ := rig.Stubs.Ledger(t.Context(), operationtest.Change); ledger != 1 {
			t.Errorf("ledger rows = %d, want one", ledger)
		}
		if row := intentOf(t, rig.DB, "testchat:1"); row.state != string(effect.StateSucceeded) {
			t.Errorf("state = %s, want succeeded", row.state)
		}
		again := rig.run(t, "/stub change notes", "testchat:1")
		if again.Receipt == nil || !slices.Equal(again.Receipt.Lines, outcome.Receipt.Lines) || rig.Stubs.Handled(operationtest.Change) != 1 {
			t.Errorf("a later redelivery = %+v handled %d, want the journaled reply without a run", again, rig.Stubs.Handled(operationtest.Change))
		}
	})
	t.Run("an expired lease with the receipt of a committed call settles without running the handler", func(t *testing.T) {
		rig := newOperationRig(t)
		rig.dieAfterStart(t, "/stub change notes", "testchat:1")
		committed := &operation.Call{
			Caller:         operation.Caller{Surface: operation.SurfaceChat, PrincipalID: operationtest.OwnerID, Class: principal.ClassOwner, SessionID: operationtest.SessionID, Direct: true},
			Input:          json.RawMessage(`{"subject":"notes"}`),
			IdempotencyKey: "testchat:1",
			RequestDigest:  operation.Digest(json.RawMessage(`{"subject":"notes"}`)),
		}
		if _, err := rig.Registry.Invoke(t.Context(), operationtest.Change, committed); err != nil {
			t.Fatalf("the call that committed before the death: %v", err)
		}
		rig.Clock.Advance(61 * time.Second)

		outcome := rig.run(t, "/stub change notes", "testchat:1")

		if outcome.Receipt == nil || outcome.Receipt.Lines[1] != "operation: "+operationtest.Change {
			t.Fatalf("outcome = %+v, want the reply rendered from the receipt", outcome)
		}
		if rig.Stubs.Handled(operationtest.Change) != 1 {
			t.Errorf("the handler ran %d times, want only the call that committed", rig.Stubs.Handled(operationtest.Change))
		}
		if ledger, _ := rig.Stubs.Ledger(t.Context(), operationtest.Change); ledger != 1 {
			t.Errorf("ledger rows = %d, want one", ledger)
		}
	})
	t.Run("an external operation is left unknown for the owner", func(t *testing.T) {
		rig := newOperationRig(t)
		rig.dieAfterStart(t, "/stub external notes", "testchat:1")
		rig.Clock.Advance(61 * time.Second)

		outcome := rig.run(t, "/stub external notes", "testchat:1")

		if outcome.Receipt == nil || outcome.Receipt.Lines[0] != "operation_failed: the earlier attempt is unresolved; see aura effects list" || rig.Stubs.Handled(operationtest.External) != 0 {
			t.Fatalf("outcome = %+v handled %d, want the unresolved reply and no run", outcome, rig.Stubs.Handled(operationtest.External))
		}
		if row := intentOf(t, rig.DB, "testchat:1"); row.state != string(effect.StateUnknown) {
			t.Errorf("state = %s, want unknown", row.state)
		}
	})
	t.Run("an unknown transactional intent without a receipt resolves failed and is never re-run", func(t *testing.T) {
		rig := newOperationRig(t)
		rig.dieAfterStart(t, "/stub change notes", "testchat:1")
		if _, err := rig.DB.ExecContext(t.Context(), `UPDATE effect_intent SET state = 'unknown' WHERE idempotency_key = 'testchat:1'`); err != nil {
			t.Fatalf("mark the intent unknown: %v", err)
		}

		outcome := rig.run(t, "/stub change notes", "testchat:1")

		if outcome.Receipt == nil || outcome.Receipt.Lines[0] != "operation_not_committed" || rig.Stubs.Handled(operationtest.Change) != 0 {
			t.Fatalf("outcome = %+v handled %d, want operation_not_committed and no run", outcome, rig.Stubs.Handled(operationtest.Change))
		}
		if row := intentOf(t, rig.DB, "testchat:1"); row.state != string(effect.StateFailed) {
			t.Errorf("state = %s, want failed", row.state)
		}
	})
}

func TestChatOperationRefusalsAreJournaledWithoutTheOperation(t *testing.T) {
	tests := []struct {
		name   string
		caller func() *command.Caller
		line   string
	}{
		{"a group chat", func() *command.Caller { c := ownerCaller(); c.Direct = false; return c }, command.ReplyDirectOnly},
		{"a trusted principal", func() *command.Caller { c := ownerCaller(); c.Class = principal.ClassTrusted; return c }, command.ReplyDenied},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rig := newOperationRig(t)

			outcome, err := rig.execute(t, rig.Executor, &sentMessage{caller: tt.caller(), text: "/stub change secret-subject", key: "testchat:1"})

			if err != nil || outcome.Receipt == nil || !slices.Equal(outcome.Receipt.Lines, []string{tt.line}) {
				t.Fatalf("outcome = %+v, %v; want only %q", outcome, err, tt.line)
			}
			row := intentOf(t, rig.DB, "testchat:1")
			if row.operation != "stub" || strings.Contains(row.request, "operation") || strings.Contains(row.request, "secret-subject") {
				t.Errorf("intent = %+v, want a record with no operation and no input", row)
			}
			if rig.Stubs.Handled(operationtest.Change) != 0 {
				t.Error("a handler ran for a refused command")
			}
		})
	}
}
