package workflow

import (
	"testing"
	"time"
)

func TestRunsRecordWhoStartedThemFromWhere(t *testing.T) {
	disk := newTestStore(t)
	event := "done"
	spec := &Spec{ID: "book_visit", Goal: "Book a visit", Version: 1, Source: SourceDefined, Steps: []StepSpec{{ID: "hold", Executor: ExecutorSpec{Kind: KindWait, Event: &event}, Timeout: time.Minute}}}
	if err := disk.SaveDefinition(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	cli, err := disk.CreateRun(t.Context(), spec, &RunInput{})
	if err != nil {
		t.Fatal(err)
	}
	origin := RunOrigin{PrincipalID: "discord:111", PrincipalClass: "public", AgentID: "lobby", SessionID: "lobby/discord:111/dm"}
	tool, err := disk.CreateRunFor(t.Context(), spec, &RunInput{Objective: "Saturday"}, &origin)
	if err != nil {
		t.Fatal(err)
	}
	if run, err := disk.Run(t.Context(), cli.ID); err != nil || run.Origin != (RunOrigin{}) {
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
	tool, err := disk.CreateRunFor(t.Context(), spec, &RunInput{}, &origin)
	if err != nil {
		t.Fatal(err)
	}
	running, err := disk.CreateRunFor(t.Context(), spec, &RunInput{}, &origin)
	if err != nil {
		t.Fatal(err)
	}
	cli, err := disk.CreateRun(t.Context(), spec, &RunInput{})
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
