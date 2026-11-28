//go:build durable

package app

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/effect"
	"github.com/anggasct/aura/internal/platform/operation"
	"github.com/anggasct/aura/internal/platform/operation/operationtest"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	runtimeadk "github.com/anggasct/aura/internal/platform/turn/adk"
)

const (
	handlerEntries   = "handler"
	previewCalls     = "preview"
	beforeCommitGate = "operation-before-commit"
	afterCommitGate  = "operation-after-commit"
)

func registerLedgerOperations(t *testing.T, db *sql.DB, broker *toolbroker.Broker, effects *effect.Executor, parts *ledgerTurn) []runtimeadk.BuiltinToolDefinition {
	t.Helper()
	ledger := parts.ledger
	stubs := operationtest.NewStubs(db)
	for _, planned := range parts.scenario.operations {
		stubs.BeforeCommit(planned.id, func(ctx context.Context, _ *operation.Call) error {
			if _, err := ledger.record(handlerEntries+"."+planned.id, ""); err != nil {
				return err
			}
			if planned.gateBefore {
				return ledger.gate(ctx, beforeCommitGate)
			}
			return nil
		})
		if planned.gateAfter {
			stubs.AfterCommit(planned.id, func(ctx context.Context, _ *operation.Call) error {
				return ledger.gate(ctx, afterCommitGate)
			})
		}
	}
	bindings := stubs.Bindings()
	for i := range bindings {
		if bindings[i].Preview == nil {
			continue
		}
		preview := bindings[i].Preview
		bindings[i].Preview = func(ctx context.Context, call *operation.Call) (operation.Preview, error) {
			if _, err := ledger.record(previewCalls, ""); err != nil {
				return operation.Preview{}, err
			}
			return preview(ctx, call)
		}
	}
	registry, err := operation.NewRegistry([]operation.Catalog{operationtest.Catalog()}, bindings, &operation.Deps{DB: db, Effects: effects, Tools: broker})
	if err != nil {
		t.Fatalf("operation.NewRegistry: %v", err)
	}
	if err := broker.RegisterProvider(registry); err != nil {
		t.Fatalf("register the operation tools: %v", err)
	}
	var defs []runtimeadk.BuiltinToolDefinition
	for _, registration := range registry.Tools() {
		defs = append(defs, runtimeadk.BuiltinToolDefinition{
			Name: registration.Definition.Name, Version: registration.Definition.Version, Schema: registration.Definition.Schema,
		})
	}
	return defs
}

func expireEffectLeases(t *testing.T, db *sql.DB) {
	t.Helper()
	past := effect.FormatFixedWidthTime(time.Now().Add(-time.Hour))
	if _, err := db.ExecContext(t.Context(), `UPDATE effect_intent SET lease_expires_at = ? WHERE state = ?`, past, string(effect.StateStarted)); err != nil {
		t.Fatalf("expire the effect leases: %v", err)
	}
}

func requireOneOperationEffect(t *testing.T, db *sql.DB, id string) {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), `SELECT state FROM effect_intent WHERE provider = ? AND operation = ?`, "operation", operationtest.ToolOf(id))
	if err != nil {
		t.Fatalf("read the effect intents of %s: %v", id, err)
	}
	defer func() { _ = rows.Close() }()
	var states []string
	for rows.Next() {
		var state string
		if err := rows.Scan(&state); err != nil {
			t.Fatalf("scan an effect intent: %v", err)
		}
		states = append(states, state)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the effect intents of %s: %v", id, err)
	}
	if len(states) != 1 || states[0] != string(effect.StateSucceeded) {
		t.Fatalf("effect intents of %s = %v, want exactly one succeeded", id, states)
	}
}

func requireOneCommit(t *testing.T, db *sql.DB, id string) {
	t.Helper()
	if n := countRows(t, db, `SELECT COUNT(*) FROM operation_stub_ledger WHERE operation = ?`, id); n != 1 {
		t.Errorf("changes applied by %s = %d, want 1", id, n)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM operation_receipt WHERE operation_id = ?`, id); n != 1 {
		t.Errorf("receipts of %s = %d, want 1", id, n)
	}
}

func requireOneAppliedChange(t *testing.T, suite *killedSuite, id string) {
	t.Helper()
	requireOneCommit(t, suite.db, id)
	requireOneOperationEffect(t, suite.db, id)
	if questions := suite.ledger.records(t, approvalCalls); len(questions) != 1 || questions[0].Life != 1 {
		t.Errorf("approval questions = %+v, want one, asked by the first life", questions)
	}
}

func TestDurableTurnOperationCallIsReplayedOnceAfterAKilledProcess(t *testing.T) {
	suite := startKilledSuite(t, "operation-replay")
	suite.child.waitKillPoint(t, "the handler is entered before its commit", suite.ledger.exists(t, beforeCommitGate+".entered"))
	if n := countRows(t, suite.db, `SELECT COUNT(*) FROM operation_stub_ledger`); n != 0 {
		t.Fatalf("changes applied at the kill = %d, want none", n)
	}

	expireEffectLeases(t, suite.db)
	life := suite.resume(t)
	suite.release(t, beforeCommitGate)

	suite.requireResumed(t, life)
	suite.ledger.requireCount(t, handlerEntries+"."+operationtest.Change, 2)
	requireOneAppliedChange(t, suite, operationtest.Change)
}

func TestDurableTurnOperationKilledBetweenCommitAndSuccessIsSettledFromItsReceipt(t *testing.T) {
	suite := startKilledSuite(t, "operation-committed")
	suite.child.waitKillPoint(t, "the handler committed", suite.ledger.exists(t, afterCommitGate+".entered"))
	requireOneCommit(t, suite.db, operationtest.Change)

	expireEffectLeases(t, suite.db)
	life := suite.resume(t)
	suite.release(t, afterCommitGate)

	suite.requireResumed(t, life)
	suite.ledger.requireCount(t, handlerEntries+"."+operationtest.Change, 1)
	requireOneAppliedChange(t, suite, operationtest.Change)
}

func TestDurableTurnApprovedDestructiveCallSurvivesReplay(t *testing.T) {
	suite := startKilledSuite(t, "operation-destructive")
	suite.child.waitKillPoint(t, "the approved deletion is entered before its commit", suite.ledger.exists(t, beforeCommitGate+".entered"))
	suite.ledger.requireCount(t, approvalCalls, 1)
	suite.ledger.requireCount(t, previewCalls, 1)

	expireEffectLeases(t, suite.db)
	life := suite.resume(t)
	suite.release(t, beforeCommitGate)

	suite.requireResumed(t, life)
	suite.ledger.requireCount(t, previewCalls, 1)
	suite.ledger.requireCount(t, handlerEntries+"."+operationtest.Delete, 2)
	requireOneAppliedChange(t, suite, operationtest.Delete)
}
