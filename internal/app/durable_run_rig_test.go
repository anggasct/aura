//go:build durable

package app

import (
	"database/sql"
	"testing"

	"github.com/anggasct/aura/internal/modules/workflow"
	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/durable/sqlite"
	"github.com/anggasct/aura/internal/store"
)

const runFirstLifeEnv = "AURA_DURABLE_RUN_FIRST_LIFE"

var runEntry = lifeEntry{test: "TestDurableRunFirstLife", env: runFirstLifeEnv}

type runPlan struct {
	lifePlan
	Run    string `json:"run"`
	Key    string `json:"key"`
	GitHub string `json:"github"`
}

type runLife struct {
	plan        runPlan
	scenario    runScenario
	db          *sql.DB
	runtime     *sqlite.Runtime
	ledger      *turnLedger
	workflows   *workflow.Store
	interpreter *workflow.Interpreter
}

func openRunLife(t *testing.T, plan runPlan) *runLife {
	t.Helper()
	scenario, ok := runScenarios[plan.Scenario]
	if !ok {
		t.Fatalf("unknown scenario %q", plan.Scenario)
	}
	db, err := store.OpenDB(t.Context(), plan.Database)
	if err != nil {
		t.Fatalf("open the database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	life := &runLife{
		plan:      plan,
		scenario:  scenario,
		db:        db,
		runtime:   newSuiteRuntime(t, db),
		ledger:    &turnLedger{dir: plan.Ledger, life: plan.Life},
		workflows: workflow.NewStore(db),
	}
	scenario.host(t, life)
	serveRuntime(t, life.runtime)
	return life
}

func (l *runLife) start(t *testing.T, req durable.StartRequest) {
	t.Helper()
	if _, err := l.runtime.Start(t.Context(), req); err != nil {
		t.Fatalf("start the durable run %s: %v", req.Key, err)
	}
}

func (l *runLife) signal(t *testing.T, name string, payload []byte) {
	t.Helper()
	if err := l.interpreter.Signal(t.Context(), l.plan.Run, name, payload); err != nil {
		t.Fatalf("signal %s: %v", name, err)
	}
}

func workflowStatus(t *testing.T, disk *workflow.Store, runID string) string {
	t.Helper()
	summary, err := disk.Run(t.Context(), runID)
	if err != nil {
		t.Fatalf("read the workflow run %s: %v", runID, err)
	}
	return summary.Status
}

func requireStepsSucceeded(t *testing.T, disk *workflow.Store, runID string) {
	t.Helper()
	steps, err := disk.Steps(t.Context(), runID)
	if err != nil {
		t.Fatalf("Steps: %v", err)
	}
	if len(steps) == 0 {
		t.Fatalf("the workflow run %s has no steps", runID)
	}
	for _, step := range steps {
		if step.Status != workflow.StepSucceeded {
			t.Errorf("step %s = %s, want %s", step.StepID, step.Status, workflow.StepSucceeded)
		}
	}
}

type runStart struct {
	scenario string
	github   string
}

type runSuite struct {
	plan   runPlan
	db     *sql.DB
	ledger *turnLedger
	child  *firstLife
}

func startRunSuite(t *testing.T, start runStart) *runSuite {
	t.Helper()
	scenario, ok := runScenarios[start.scenario]
	if !ok {
		t.Fatalf("unknown scenario %q", start.scenario)
	}
	plan, db := seedSuite(t, start.scenario)
	suite := &runSuite{plan: runPlan{lifePlan: plan, GitHub: start.github}, db: db, ledger: &turnLedger{dir: plan.Ledger}}
	scenario.seed(t, suite)
	suite.child = startFirstLife(t, lifeLaunch{entry: runEntry, plan: suite.plan})
	if scenario.started != nil {
		scenario.started(t, suite)
	}
	return suite
}

func (s *runSuite) openSecondLife(t *testing.T) *runLife {
	t.Helper()
	if !s.child.ended {
		t.Fatal("the second life opens only after the first life is killed")
	}
	plan := s.plan
	plan.Life = 2
	return openRunLife(t, plan)
}

func (s *runSuite) release(t *testing.T, tool string) {
	t.Helper()
	s.ledger.mark(t, tool+".release")
}

func (s *runSuite) requireRunSucceeded(t *testing.T) {
	t.Helper()
	waitUntil(t, "the durable run "+s.plan.Key+" ends", func() bool {
		state, _ := runRow(t, s.db, s.plan.Key)
		return state != string(durable.RunRunning)
	})
	state, claim := runRow(t, s.db, s.plan.Key)
	if state != string(durable.RunSucceeded) {
		t.Errorf("durable run %s state = %q, want %q", s.plan.Key, state, durable.RunSucceeded)
	}
	if claim != 2 {
		t.Errorf("durable run %s claim = %d, want 2 (one claim by each life; a third claim means a host lost its lease under load and an action may have run twice)", s.plan.Key, claim)
	}
}

func (s *runSuite) requireWorkflowHandler(t *testing.T) {
	t.Helper()
	var handler string
	if err := s.db.QueryRowContext(t.Context(), `SELECT handler FROM durable_run WHERE key = ?`, s.plan.Key).Scan(&handler); err != nil {
		t.Fatalf("read the handler of the run %s: %v", s.plan.Key, err)
	}
	if handler != workflow.HandlerName {
		t.Errorf("durable run %s handler = %q, want %q", s.plan.Key, handler, workflow.HandlerName)
	}
}
