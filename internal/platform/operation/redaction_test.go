package operation_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/platform/operation"
	"github.com/anggasct/aura/internal/platform/operation/operationtest"
	"github.com/anggasct/aura/internal/platform/toolbroker"
)

func TestOperationResultsAreRedacted(t *testing.T) {
	const secret = "sk-live-0123456789abcdef"
	leak := json.RawMessage(`{"done":true,"operation":"leaks ` + secret + `"}`)
	cases := []struct {
		name string
		id   string
	}{
		{name: "a read", id: operationtest.Read},
		{name: "a transactional change", id: operationtest.Change},
		{name: "an external idempotent change", id: operationtest.Idempotent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := operationtest.NewRig(t, &operationtest.Options{Bindings: func(stubs *operationtest.Stubs) []operation.Binding {
				bindings := stubs.Bindings()
				for i := range bindings {
					if bindings[i].ID != tc.id {
						continue
					}
					bindings[i].Handle = func(ctx context.Context, call *operation.Call) (operation.Result, error) {
						if tc.id != operationtest.Change {
							return operation.Result{Data: leak}, nil
						}
						return call.Commit(ctx, func(context.Context) (operation.Result, error) {
							return operation.Result{Data: leak}, nil
						})
					}
				}
				return bindings
			}})
			rig.Broker.RegisterSecret(secret)

			outcome := rig.Execute(t.Context(), operationtest.Request(t, operationtest.ToolOf(tc.id), "a"))

			if outcome.Class != toolbroker.ResultOK {
				t.Fatalf("outcome = %+v, want ok", outcome)
			}
			if strings.Contains(outcome.Output, secret) {
				t.Fatalf("the agent result carries the secret: %s", outcome.Output)
			}
			var recorded []string
			rows, err := rig.DB.QueryContext(t.Context(), `SELECT COALESCE(provider_receipt_json, '') FROM effect_intent`)
			if err != nil {
				t.Fatalf("read the effect journal: %v", err)
			}
			defer func() { _ = rows.Close() }()
			for rows.Next() {
				var receipt string
				if err := rows.Scan(&receipt); err != nil {
					t.Fatalf("scan the effect journal: %v", err)
				}
				recorded = append(recorded, receipt)
			}
			for _, receipt := range recorded {
				if strings.Contains(receipt, secret) {
					t.Errorf("the effect journal carries the secret: %s", receipt)
				}
			}
		})
	}
}

func TestToolObservationNamesTheOperation(t *testing.T) {
	var observed []toolbroker.Observation
	rig := operationtest.NewRig(t, &operationtest.Options{ToolObserver: func(_ context.Context, observation toolbroker.Observation) {
		observed = append(observed, observation)
	}})

	outcome := rig.Execute(t.Context(), operationtest.Request(t, operationtest.ToolOf(operationtest.Read), "a"))

	if outcome.Class != toolbroker.ResultOK {
		t.Fatalf("outcome = %+v, want ok", outcome)
	}
	if len(observed) != 1 || observed[0].OperationID != operationtest.Read {
		t.Fatalf("observations = %+v, want one naming %s", observed, operationtest.Read)
	}
}
