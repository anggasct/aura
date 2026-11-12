//go:build durable

package app

import (
	"encoding/json"
	"os"
	"testing"

	gatewaywebhook "github.com/anggasct/aura/internal/channels/webhook"
	"github.com/anggasct/aura/internal/modules/child"
	githubadapter "github.com/anggasct/aura/internal/modules/github"
	"github.com/anggasct/aura/internal/modules/workflow"
	"github.com/anggasct/aura/internal/platform/effect"
	"github.com/anggasct/aura/internal/store"
)

func TestDurableRunFirstLife(t *testing.T) {
	raw := os.Getenv(runFirstLifeEnv)
	if raw == "" {
		t.Skip("runs only as the first life of a killed-process suite")
	}
	var plan runPlan
	if err := json.Unmarshal([]byte(raw), &plan); err != nil {
		t.Fatalf("decode the first life plan: %v", err)
	}
	plan.Life = 1
	life := openRunLife(t, plan)
	life.scenario.firstLife(t, life)
	<-t.Context().Done()
}

func TestDurableWorkflowSkipsCompletedStepsAfterAKilledProcess(t *testing.T) {
	suite := startRunSuite(t, runStart{scenario: "defined"})
	suite.waitSuspendedAfterOneReadFile(t)
	suite.child.kill(t)
	suite.requireResumedAtTheHold(t)
}

func TestDurableWorkflowGeneratedRunResumesFromTheStoreAfterAKilledProcess(t *testing.T) {
	suite := startRunSuite(t, runStart{scenario: "generated"})
	suite.waitSuspendedAfterOneReadFile(t)
	suite.child.kill(t)
	suite.requireResumedAtTheHold(t)
}

func (s *runSuite) waitSuspendedAfterOneReadFile(t *testing.T) {
	t.Helper()
	disk := workflow.NewStore(s.db)
	s.child.waitKillPoint(t, "the run is suspended after one read_file", func() bool {
		return workflowStatus(t, disk, s.plan.Run) == workflow.RunSuspended && len(s.ledger.records(t, executions(readFileTool))) == 1
	})
}

func (s *runSuite) requireResumedAtTheHold(t *testing.T) {
	t.Helper()
	disk := workflow.NewStore(s.db)
	life := s.openSecondLife(t)
	life.signal(t, holdSignal, []byte(`{"ci":"green"}`))

	s.requireRunSucceeded(t)
	if status := workflowStatus(t, disk, s.plan.Run); status != workflow.RunSucceeded {
		t.Errorf("workflow run status = %q, want %q", status, workflow.RunSucceeded)
	}
	requireStepsSucceeded(t, disk, s.plan.Run)
	s.ledger.requireCount(t, executions(readFileTool), 1)
	s.ledger.requireCount(t, executions(listDirTool), 1)
}

func TestDurableWorkflowWithGitHubToolsSurvivesAKilledProcess(t *testing.T) {
	t.Setenv(proofTokenEnv, proofToken)
	api := startGitHubStub(t)
	suite := startRunSuite(t, runStart{scenario: "github", github: api.server.URL})
	disk := workflow.NewStore(suite.db)
	suite.child.waitKillPoint(t, "the run is suspended at the CI wait with its pull request created and bound", func() bool {
		return api.creates.Load() == 1 && workflowStatus(t, disk, suite.plan.Run) == workflow.RunSuspended && stepSucceeded(t, disk, suite.plan.Run, "open_pr") && len(correlations(t, disk, suite.plan.Run)) == 1
	})
	if rows := correlations(t, disk, suite.plan.Run); rows[0].ExternalID != proofPullID {
		t.Fatalf("bindings = %+v, want the create_pr external identity %s", rows, proofPullID)
	}

	suite.child.kill(t)
	life := suite.openSecondLife(t)
	deliverCICompletion(t, life)
	life.signal(t, mergeSignal, []byte(approveOutcome))

	suite.requireRunSucceeded(t)
	if status := workflowStatus(t, disk, suite.plan.Run); status != workflow.RunSucceeded {
		t.Errorf("workflow run status = %q, want %q", status, workflow.RunSucceeded)
	}
	requireStepsSucceeded(t, disk, suite.plan.Run)
	for name, calls := range map[string]int64{"create pull request": api.creates.Load(), "comment": api.notes.Load(), "merge": api.merges.Load()} {
		if calls != 1 {
			t.Errorf("%s calls = %d, want exactly one across the killed process", name, calls)
		}
	}
	for _, tool := range githubadapter.ToolNames() {
		suite.ledger.requireCount(t, executions(tool), 1)
	}
	requireCIFamily(t, correlations(t, disk, suite.plan.Run))
	requireOneEffectPerOperation(t, life.effects)
}

func stepSucceeded(t *testing.T, disk *workflow.Store, runID, stepID string) bool {
	t.Helper()
	steps, err := disk.Steps(t.Context(), runID)
	if err != nil {
		t.Fatalf("Steps: %v", err)
	}
	for _, step := range steps {
		if step.StepID == stepID {
			return step.Status == workflow.StepSucceeded
		}
	}
	return false
}

func correlations(t *testing.T, disk *workflow.Store, runID string) []workflow.Correlation {
	t.Helper()
	rows, err := disk.ListCorrelationsByRun(t.Context(), runID)
	if err != nil {
		t.Fatalf("ListCorrelationsByRun: %v", err)
	}
	return rows
}

func deliverCICompletion(t *testing.T, life *runLife) {
	t.Helper()
	adapter, err := githubadapter.NewAdapter(life.workflows, life.runtime, nil)
	if err != nil {
		t.Fatalf("github adapter: %v", err)
	}
	body := []byte(`{"action":"completed","repository":{"full_name":"org/repo"},` +
		`"check_suite":{"status":"completed","conclusion":"success","head_sha":"abc123",` +
		`"html_url":"https://github.com/org/repo/suites/1","pull_requests":[{"number":42}]}}`)
	event := &gatewaywebhook.AcceptedEvent{KeyID: "github", Nonce: "nonce-abcdefghijklmnop", BodyDigest: "digest-ci-proof-1", Body: body}
	handled, ref, err := adapter.Handle(t.Context(), event)
	if err != nil || !handled || ref.ExecutionID != life.plan.Run {
		t.Fatalf("Handle CI event = %v, %+v, %v; want run %s", handled, ref, err, life.plan.Run)
	}
	if _, _, err := adapter.Handle(t.Context(), event); err != nil {
		t.Fatalf("duplicate Handle: %v", err)
	}
}

func requireCIFamily(t *testing.T, rows []workflow.Correlation) {
	t.Helper()
	var bindings, deliveries int
	for _, row := range rows {
		if row.SignalName != "wait.hold_ci" || row.ExternalID != proofPullID {
			t.Errorf("correlation row = %+v, want the CI binding family", row)
		}
		if row.DedupeKey == "" {
			bindings++
		} else {
			deliveries++
		}
	}
	if bindings != 1 || deliveries != 1 {
		t.Errorf("bindings = %d, deliveries = %d; want one of each", bindings, deliveries)
	}
}

func requireOneEffectPerOperation(t *testing.T, effects *effect.Executor) {
	t.Helper()
	intents, err := effects.Journal().ListByState(t.Context(), effect.StateSucceeded, 0)
	if err != nil {
		t.Fatalf("list the succeeded effect intents: %v", err)
	}
	seen := map[string]int{}
	for i := range intents {
		if intents[i].Classification == effect.ClassificationEffectful {
			seen[intents[i].Operation]++
		}
	}
	for _, operation := range githubadapter.ToolNames() {
		if seen[operation] != 1 {
			t.Errorf("succeeded effectful intents for %s = %d, want 1", operation, seen[operation])
		}
	}
}

func TestDurableChildResumesAfterAKilledProcess(t *testing.T) {
	suite := startRunSuite(t, runStart{scenario: "child"})
	suite.child.waitKillPoint(t, "tool-b is entered", suite.ledger.exists(t, "tool-b.entered"))
	if n := len(suite.ledger.records(t, modelCalls)); n != 2 {
		t.Fatalf("model calls at the kill = %d, want 2", n)
	}
	if n := len(suite.ledger.records(t, executions("tool-a"))); n != 1 {
		t.Fatalf("tool-a executions at the kill = %d, want 1", n)
	}

	suite.child.kill(t)
	suite.openSecondLife(t)
	suite.release(t, "tool-b")

	suite.requireRunSucceeded(t)
	run, found, err := store.NewChildStore(suite.db).GetRun(t.Context(), suite.plan.Run)
	if err != nil || !found {
		t.Fatalf("GetRun = %+v, %v, %v", run, found, err)
	}
	if run.State != child.StatusSucceeded || run.ResultOutput != scriptedAnswerText {
		t.Errorf("child run state = %q with the output %q, want %q with the model's final text %q", run.State, run.ResultOutput, child.StatusSucceeded, scriptedAnswerText)
	}
	suite.ledger.requireCount(t, modelCalls, 3)
	suite.ledger.requireCount(t, executions("tool-a"), 1)
	suite.ledger.requireCount(t, executions("tool-b"), 1)
}
