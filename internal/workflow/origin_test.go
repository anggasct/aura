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
