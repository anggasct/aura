//go:build linux

package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/anggasct/aura/internal/modules/workflow"
	"github.com/anggasct/aura/internal/platform/toolbroker"
)

type effectIntentIDs struct {
	sessionID      string
	turnID         string
	requestID      string
	idempotencyKey string
}

func writeFileArgs(t *testing.T, name, content string) json.RawMessage {
	t.Helper()
	args, err := json.Marshal(map[string]string{"path": name, "content": content})
	if err != nil {
		t.Fatalf("encode arguments: %v", err)
	}
	return args
}

func effectIntentsOf(t *testing.T, runner *workflowToolRunner, key string) []effectIntentIDs {
	t.Helper()
	rows, err := runner.db.QueryContext(t.Context(), `SELECT session_id, turn_id, tool_call_id, idempotency_key FROM effect_intent WHERE idempotency_key = ?`, key)
	if err != nil {
		t.Fatalf("query effect intents: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var intents []effectIntentIDs
	for rows.Next() {
		var ids effectIntentIDs
		if err := rows.Scan(&ids.sessionID, &ids.turnID, &ids.requestID, &ids.idempotencyKey); err != nil {
			t.Fatalf("scan effect intent: %v", err)
		}
		intents = append(intents, ids)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read effect intents: %v", err)
	}
	return intents
}

func digestOf(runID, stepID string) string {
	sum := sha256.Sum256([]byte(runID + "/" + stepID))
	return hex.EncodeToString(sum[:])[:24]
}

func stepCallAt(t *testing.T, runID, stepID string, attempt int) context.Context {
	t.Helper()
	return workflow.WithStepCall(t.Context(), &workflow.StepCall{RunID: runID, StepID: stepID, Attempt: attempt})
}

func TestWorkflowToolRunnerDerivesItsIdsFromTheStepCall(t *testing.T) {
	runner, _, workspace := newWorkflowToolRunnerFixture(t)
	toolRunner, ok := runner.(*workflowToolRunner)
	if !ok {
		t.Fatalf("runner is %T, want *workflowToolRunner", runner)
	}

	t.Run("the same run and step send the same ids whatever the attempt, and the effect is performed once", func(t *testing.T) {
		first, err := runner.Invoke(stepCallAt(t, "run-1", "step-a", 0), "write_file", writeFileArgs(t, "a.txt", "one"))
		if err != nil {
			t.Fatalf("first Invoke: %v", err)
		}
		retried, err := runner.Invoke(stepCallAt(t, "run-1", "step-a", 1), "write_file", writeFileArgs(t, "a.txt", "one"))
		if err != nil {
			t.Fatalf("retry Invoke with the same arguments: %v", err)
		}
		if !bytes.Equal(retried, first) {
			t.Fatalf("retry Invoke output = %s, want the first output %s", retried, first)
		}
		_, err = runner.Invoke(stepCallAt(t, "run-1", "step-a", 2), "write_file", writeFileArgs(t, "a.txt", "two"))
		if code, ok := toolbroker.CodeOf(err); !ok || code != toolbroker.ResultInvalidArgument {
			t.Fatalf("retry Invoke with other arguments error = %v, want %s", err, toolbroker.ResultInvalidArgument)
		}
		content, err := os.ReadFile(filepath.Join(workspace, "a.txt"))
		if err != nil {
			t.Fatalf("read the written file: %v", err)
		}
		if string(content) != "one" {
			t.Fatalf("file content = %q, want the first attempt's %q", content, "one")
		}
		digest := digestOf("run-1", "step-a")
		want := []effectIntentIDs{{sessionID: "wf-" + digest, turnID: "turn-" + digest, requestID: "req-" + digest, idempotencyKey: "wf/run-1/step-a"}}
		got := effectIntentsOf(t, toolRunner, "wf/run-1/step-a")
		if len(got) != 1 || got[0] != want[0] {
			t.Fatalf("effect intents = %+v, want %+v", got, want)
		}
	})

	t.Run("another step sends other ids", func(t *testing.T) {
		if _, err := runner.Invoke(stepCallAt(t, "run-1", "step-b", 0), "write_file", writeFileArgs(t, "b.txt", "one")); err != nil {
			t.Fatalf("Invoke step-b: %v", err)
		}
		a := effectIntentsOf(t, toolRunner, "wf/run-1/step-a")[0]
		b := effectIntentsOf(t, toolRunner, "wf/run-1/step-b")
		if len(b) != 1 {
			t.Fatalf("step-b effect intents = %+v, want one", b)
		}
		if a.sessionID == b[0].sessionID || a.turnID == b[0].turnID || a.requestID == b[0].requestID || a.idempotencyKey == b[0].idempotencyKey {
			t.Fatalf("step-a %+v and step-b %+v share an id", a, b[0])
		}
	})

	t.Run("a call without a step call is refused", func(t *testing.T) {
		_, err := runner.Invoke(t.Context(), "write_file", writeFileArgs(t, "c.txt", "one"))
		if code, ok := workflow.CodeOf(err); !ok || code != workflow.ErrorCodeExecutorInvalid {
			t.Fatalf("Invoke error = %v, want %s", err, workflow.ErrorCodeExecutorInvalid)
		}
		if _, err := os.Stat(filepath.Join(workspace, "c.txt")); !os.IsNotExist(err) {
			t.Fatalf("the refused call left c.txt behind (err=%v)", err)
		}
	})

	t.Run("empty arguments stay a step failure", func(t *testing.T) {
		contexts := map[string]context.Context{
			"with a step call":    stepCallAt(t, "run-1", "step-a", 0),
			"without a step call": t.Context(),
		}
		for name, ctx := range contexts {
			_, err := runner.Invoke(ctx, "write_file", json.RawMessage(`{}`))
			if code, ok := workflow.CodeOf(err); !ok || code != workflow.ErrorCodeStepFailed {
				t.Fatalf("%s: Invoke error = %v, want %s", name, err, workflow.ErrorCodeStepFailed)
			}
		}
	})
}
