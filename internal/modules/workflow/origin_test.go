package workflow

import (
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/durable"
)

func ownerOrigin() *RunOrigin {
	return &RunOrigin{PrincipalID: "local-owner", PrincipalClass: "owner", AgentID: "main"}
}

func TestRunsRecordWhoStartedThemFromWhere(t *testing.T) {
	disk := newTestStore(t)
	event := "done"
	spec := &Spec{ID: "book_visit", Goal: "Book a visit", Version: 1, Source: SourceDefined, Steps: []StepSpec{{ID: "hold", Executor: ExecutorSpec{Kind: KindWait, Event: &event}, Timeout: time.Minute}}}
	if err := disk.SaveDefinition(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	cli, err := disk.CreateRun(t.Context(), spec, &RunInput{}, ownerOrigin())
	if err != nil {
		t.Fatal(err)
	}
	origin := RunOrigin{PrincipalID: "discord:111", PrincipalClass: "public", AgentID: "lobby", SessionID: "lobby/discord:111/dm", Tainted: true}
	tool, err := disk.CreateRun(t.Context(), spec, &RunInput{Objective: "Saturday"}, &origin)
	if err != nil {
		t.Fatal(err)
	}
	if run, err := disk.Run(t.Context(), cli.ID); err != nil || run.Origin != *ownerOrigin() {
		t.Fatalf("CLI run origin = %+v, %v", run, err)
	}
	if run, err := disk.Run(t.Context(), tool.ID); err != nil || run.Origin != origin {
		t.Fatalf("tool run origin = %+v, %v", run, err)
	}
	if _, err := disk.db.ExecContext(t.Context(), `UPDATE workflow_run SET principal_class = 'stranger' WHERE id = ?`, tool.ID); err == nil {
		t.Fatal("principal_class accepted a value outside owner, trusted and public")
	}
}

func TestCompletionOutboxClaimsOnceAndRetriesWithinTheCap(t *testing.T) {
	disk := newTestStore(t)
	event := "done"
	spec := &Spec{ID: "book_visit", Goal: "Book a visit", Version: 1, Source: SourceDefined, Steps: []StepSpec{{ID: "hold", Executor: ExecutorSpec{Kind: KindWait, Event: &event}, Timeout: time.Minute}}}
	if err := disk.SaveDefinition(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	origin := RunOrigin{PrincipalID: "discord:111", PrincipalClass: "public", AgentID: "lobby", SessionID: "lobby/discord:111/dm", ReplyAccount: "front", ReplyChannelID: "333"}
	tool, err := disk.CreateRun(t.Context(), spec, &RunInput{}, &origin)
	if err != nil {
		t.Fatal(err)
	}
	running, err := disk.CreateRun(t.Context(), spec, &RunInput{}, &origin)
	if err != nil {
		t.Fatal(err)
	}
	cli, err := disk.CreateRun(t.Context(), spec, &RunInput{}, ownerOrigin())
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{tool.ID, cli.ID} {
		if err := disk.SetRunStatus(t.Context(), id, RunSucceeded); err != nil {
			t.Fatal(err)
		}
	}
	if err := disk.SetRunStatus(t.Context(), running.ID, RunRunning); err != nil {
		t.Fatal(err)
	}
	pending, err := disk.PendingCompletions(t.Context(), 10)
	if err != nil || len(pending) != 1 || pending[0].ID != tool.ID || pending[0].Origin != origin {
		t.Fatalf("pending = %+v, %v; want only the finished tool-started run", pending, err)
	}
	now := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	if claimed, err := disk.ClaimCompletion(t.Context(), tool.ID, now); err != nil || !claimed {
		t.Fatalf("first claim = %v, %v", claimed, err)
	}
	if claimed, _ := disk.ClaimCompletion(t.Context(), tool.ID, now); claimed {
		t.Fatal("a claimed completion was claimed twice")
	}
	for attempt := 1; attempt <= MaxCompletionAttempts; attempt++ {
		exhausted, err := disk.ReleaseCompletion(t.Context(), tool.ID)
		if err != nil || exhausted != (attempt == MaxCompletionAttempts) {
			t.Fatalf("release %d = %v, %v", attempt, exhausted, err)
		}
		pending, _ := disk.PendingCompletions(t.Context(), 10)
		if (len(pending) == 1) != (attempt < MaxCompletionAttempts) {
			t.Fatalf("after release %d pending = %d", attempt, len(pending))
		}
		if attempt < MaxCompletionAttempts {
			if claimed, _ := disk.ClaimCompletion(t.Context(), tool.ID, now); !claimed {
				t.Fatalf("released completion %d could not be reclaimed", attempt)
			}
		}
	}
	if attempts, err := disk.CompletionAttempts(t.Context(), tool.ID); err != nil || attempts != MaxCompletionAttempts {
		t.Fatalf("attempts = %d, %v", attempts, err)
	}
}

var originDrivers = []struct {
	name   string
	driver Driver
}{
	{name: "serial", driver: DriverSerial},
	{name: "concurrent", driver: DriverConcurrent},
}

func toolStartedOrigin() *RunOrigin {
	return &RunOrigin{PrincipalID: "discord:111", PrincipalClass: "public", AgentID: "lobby", SessionID: "lobby/discord:111/dm", ReplyAccount: "front", ReplyChannelID: "333", Tainted: true}
}

func countRuns(t *testing.T, disk *Store) int {
	t.Helper()
	var count int
	if err := disk.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM workflow_run`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestStartRefusesAnIncompleteOrigin(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(origin *RunOrigin) *RunOrigin
	}{
		{name: "no origin", mutate: func(*RunOrigin) *RunOrigin { return nil }},
		{name: "empty principal", mutate: func(origin *RunOrigin) *RunOrigin { origin.PrincipalID = ""; return origin }},
		{name: "empty class", mutate: func(origin *RunOrigin) *RunOrigin { origin.PrincipalClass = ""; return origin }},
		{name: "unknown class", mutate: func(origin *RunOrigin) *RunOrigin { origin.PrincipalClass = "stranger"; return origin }},
		{name: "empty agent", mutate: func(origin *RunOrigin) *RunOrigin { origin.AgentID = ""; return origin }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			interpreter, _, disk := driverTestSetup(t, &fakeToolRunner{}, DriverConcurrent)
			installAll(t, interpreter, toolSpec("leaf"))
			origin := tc.mutate(ownerOrigin())
			if _, err := interpreter.Start(t.Context(), "leaf", nil, origin); !hasCode(err, ErrorCodeOriginIncomplete) {
				t.Fatalf("Start = %v, want code %s", err, ErrorCodeOriginIncomplete)
			}
			if _, err := interpreter.StartKeyed(t.Context(), "leaf", nil, origin, "sched-key"); !hasCode(err, ErrorCodeOriginIncomplete) {
				t.Fatalf("StartKeyed = %v, want code %s", err, ErrorCodeOriginIncomplete)
			}
			if _, err := disk.CreateRun(t.Context(), toolSpec("leaf"), nil, origin); !hasCode(err, ErrorCodeOriginIncomplete) {
				t.Fatalf("CreateRun = %v, want code %s", err, ErrorCodeOriginIncomplete)
			}
			if got := countRuns(t, disk); got != 0 {
				t.Fatalf("%d workflow_run rows written for an incomplete origin, want none", got)
			}
		})
	}
}

func TestToolStepCarriesOriginAndDigest(t *testing.T) {
	for _, tc := range originDrivers {
		driver := tc.driver
		t.Run(tc.name, func(t *testing.T) {
			tools := &stepCallTools{}
			interpreter, fake, disk := driverTestSetup(t, tools, driver)
			spec := toolSpec("leaf")
			installAll(t, interpreter, spec)
			digest, err := Digest(spec)
			if err != nil {
				t.Fatal(err)
			}
			origin := toolStartedOrigin()
			var runID string
			if driver == DriverSerial {
				run, err := disk.CreateRun(t.Context(), spec, &RunInput{}, origin)
				if err != nil {
					t.Fatal(err)
				}
				runID = run.ID
				inv, _ := holdInvocation(t, fake, "holder-origin")
				if terminal, err := interpreter.DriveSerial(t.Context(), inv, runID); err != nil || terminal != RunSucceeded {
					t.Fatalf("DriveSerial = %s, %v", terminal, err)
				}
			} else {
				run, err := interpreter.Start(t.Context(), "leaf", &RunInput{}, origin)
				if err != nil {
					t.Fatal(err)
				}
				runID = run.ID
				waitForRunStatus(t, disk, runID, RunSucceeded, 5*time.Second)
			}
			calls := tools.recorded()
			if len(calls) != 1 {
				t.Fatalf("step calls = %+v, want one", calls)
			}
			if calls[0].RunID != runID || calls[0].Origin != *origin || calls[0].DefinitionDigest != digest {
				t.Fatalf("step call = %+v, want origin %+v and digest %s", calls[0], *origin, digest)
			}
		})
	}
}

func TestToolStepWithoutCompleteOriginNeverReachesTheRunner(t *testing.T) {
	for _, tc := range originDrivers {
		driver := tc.driver
		t.Run(tc.name, func(t *testing.T) {
			tools := &stepCallTools{}
			interpreter, fake, disk := driverTestSetup(t, tools, driver)
			spec := toolSpec("leaf")
			installAll(t, interpreter, spec)
			run, err := disk.CreateRun(t.Context(), spec, &RunInput{}, ownerOrigin())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := disk.db.ExecContext(t.Context(), `UPDATE workflow_run SET agent_id = NULL WHERE id = ?`, run.ID); err != nil {
				t.Fatal(err)
			}
			if driver == DriverSerial {
				inv, _ := holdInvocation(t, fake, "holder-incomplete")
				if terminal, err := interpreter.DriveSerial(t.Context(), inv, run.ID); err != nil || terminal != RunFailed {
					t.Fatalf("DriveSerial = %s, %v; want a failed run", terminal, err)
				}
			} else {
				if _, err := fake.Start(t.Context(), durable.StartRequest{
					Handler: HandlerName, Key: "workflow/" + run.ID, Payload: mustEncode(tickPayload{RunID: run.ID, DefinitionID: spec.ID, Version: spec.Version}),
				}); err != nil {
					t.Fatal(err)
				}
				waitForRunStatus(t, disk, run.ID, RunFailed, 5*time.Second)
			}
			steps, err := disk.Steps(t.Context(), run.ID)
			if err != nil || len(steps) != 1 {
				t.Fatalf("Steps = %d, %v; want one", len(steps), err)
			}
			if steps[0].Status != StepFailed || steps[0].ErrorCode != string(ErrorCodeOriginIncomplete) {
				t.Fatalf("step = %s/%s, want failed/%s", steps[0].Status, steps[0].ErrorCode, ErrorCodeOriginIncomplete)
			}
			if calls := tools.recorded(); len(calls) != 0 {
				t.Fatalf("the runner was called %d times for a run without a complete origin", len(calls))
			}
		})
	}
}

func TestChildRunInheritsOriginAndStaysQuiet(t *testing.T) {
	for _, tc := range originDrivers {
		driver := tc.driver
		t.Run(tc.name, func(t *testing.T) {
			tools := &stepCallTools{}
			interpreter, fake, disk := driverTestSetup(t, tools, driver)
			installAll(t, interpreter, toolSpec("leaf"), composingSpec("top", "leaf"))
			origin := toolStartedOrigin()
			var parentID string
			if driver == DriverSerial {
				run, err := disk.CreateRun(t.Context(), specOf(t, interpreter, "top"), &RunInput{}, origin)
				if err != nil {
					t.Fatal(err)
				}
				parentID = run.ID
				inv, _ := holdInvocation(t, fake, "holder-child-origin")
				if terminal, err := interpreter.DriveSerial(t.Context(), inv, parentID); err != nil || terminal != RunSucceeded {
					t.Fatalf("DriveSerial = %s, %v", terminal, err)
				}
			} else {
				run, err := interpreter.Start(t.Context(), "top", &RunInput{}, origin)
				if err != nil {
					t.Fatal(err)
				}
				parentID = run.ID
				waitForRunStatus(t, disk, parentID, RunSucceeded, 5*time.Second)
			}
			listed := childRuns(t, disk, "leaf")
			if len(listed) != 1 {
				t.Fatalf("child runs = %d, want one", len(listed))
			}
			child, err := disk.Run(t.Context(), listed[0].ID)
			if err != nil {
				t.Fatal(err)
			}
			if child.Origin != *origin || !child.Origin.Tainted || child.ParentRunID != parentID {
				t.Fatalf("child = origin %+v parent %q, want origin %+v parent %q", child.Origin, child.ParentRunID, *origin, parentID)
			}
			if calls := tools.recorded(); len(calls) != 1 || calls[0].RunID != child.ID || calls[0].Origin != *origin {
				t.Fatalf("child tool step calls = %+v, want one with origin %+v", calls, *origin)
			}
			pending, err := disk.PendingCompletions(t.Context(), 10)
			if err != nil {
				t.Fatal(err)
			}
			if len(pending) != 1 || pending[0].ID != parentID {
				t.Fatalf("pending completions = %+v, want only the parent %s", pending, parentID)
			}
		})
	}
}

func TestProposalKeepsItsTaint(t *testing.T) {
	disk := newTestStore(t)
	proposalSession(t, disk, "s1")
	created := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		id      string
		tainted bool
	}{{id: "prop_clean"}, {id: "prop_tainted", tainted: true}} {
		spec := NormalizeProposal(toolSpec("x"), tc.id)
		digest, err := Digest(spec)
		if err != nil {
			t.Fatal(err)
		}
		if err := disk.CreateProposal(t.Context(), &Proposal{ID: tc.id, SessionID: "s1", AgentID: "main", PrincipalID: "owner", Tainted: tc.tainted, Spec: spec, Digest: digest, CreatedAt: created}); err != nil {
			t.Fatal(err)
		}
		got, err := disk.Proposal(t.Context(), tc.id, created.Add(time.Hour))
		if err != nil || got.Tainted != tc.tainted {
			t.Fatalf("%s tainted = %v, %v; want %v", tc.id, got, err, tc.tainted)
		}
	}
}
