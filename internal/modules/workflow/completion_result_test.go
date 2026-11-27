package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/anggasct/aura/internal/platform/durable/durabletest"
)

type replyRecorder struct {
	mu      sync.Mutex
	targets []ReplyTarget
	texts   []string
}

func (r *replyRecorder) DeliverReply(_ context.Context, target *ReplyTarget, text string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.targets = append(r.targets, *target)
	r.texts = append(r.texts, text)
	return nil
}

func (r *replyRecorder) delivered() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.texts...)
}

type silentAppender struct{}

func (silentAppender) AppendSessionText(context.Context, string, string, string, string, time.Time) error {
	return nil
}

var resultReplyTarget = ReplyTarget{Channel: "discord", Account: "front", ChannelID: "333"}

const resultGoal = "Ship it"

func resultOrigin(class string) *RunOrigin {
	return &RunOrigin{PrincipalID: "discord:111", PrincipalClass: class, AgentID: "lobby", SessionID: "ops/session", Reply: resultReplyTarget}
}

func sweepCompletions(t *testing.T, store *Store) []string {
	t.Helper()
	registry, err := buildTestRegistry()
	if err != nil {
		t.Fatal(err)
	}
	recorder := &replyRecorder{}
	notifier, err := NewCompletionNotifier(&CompletionNotifierDeps{
		Runs: store, Registry: registry, Appender: silentAppender{}, Deliver: recorder, Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	notifier.Sweep(t.Context())
	return recorder.delivered()
}

func agentEnvelope(text string) string {
	encoded, err := json.Marshal(agentOutput{Text: text})
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

func agentJSONEnvelope(object string) string {
	encoded, err := json.Marshal(agentOutput{Text: object, JSON: json.RawMessage(object)})
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

type resultRow struct {
	status  string
	endedAt int
	output  string
	tainted bool
	private bool
}

var resultEpoch = time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC)

func seedResultRun(t *testing.T, steps []StepSpec, rows map[string]resultRow, class, runStatus string) (*Store, *RunSummary) {
	t.Helper()
	store := mustNewStore(t, newTestDB(t))
	spec := &Spec{ID: "report", Goal: resultGoal, Version: 1, Source: SourceDefined, Steps: steps}
	if err := store.SaveDefinition(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	seedMigrationSession(t, store.db, "ops/session", "discord:111")
	run, err := store.newRun(t.Context(), spec, &RunInput{}, resultOrigin(class))
	if err != nil {
		t.Fatal(err)
	}
	for stepID, row := range rows {
		ended := resultEpoch.Add(time.Duration(row.endedAt) * time.Second)
		update := &stepUpdate{Status: row.status, EndedAt: &ended, Output: []byte(row.output), Tainted: row.tainted, Private: row.private}
		if err := store.updateStep(t.Context(), run.ID, stepID, update); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.SetRunStatus(t.Context(), run.ID, runStatus); err != nil {
		t.Fatal(err)
	}
	return store, run
}

func producingAgent(id string) StepSpec { return labelStep(id, agentExecutor("engineer")) }

func producingTool(id string) StepSpec { return labelStep(id, toolExecutor("read_file")) }

func childWorkflowStep(id string) StepSpec {
	return labelStep(id, ExecutorSpec{Kind: KindWorkflow, WorkflowID: ptr("leaf")})
}

func waitingStep(id string) StepSpec {
	return labelStep(id, ExecutorSpec{Kind: KindWait, Event: ptr("done")})
}

func TestCompletionCarriesTheLastStepResult(t *testing.T) {
	const plain = "Done: " + resultGoal
	cases := []struct {
		name      string
		steps     []StepSpec
		rows      map[string]resultRow
		runStatus string
		want      string
	}{
		{
			name:  "an agent step contributes its text",
			steps: []StepSpec{producingAgent("write")},
			rows:  map[string]resultRow{"write": {status: StepSucceeded, endedAt: 1, output: agentEnvelope("  five points  ")}},
			want:  plain + "\n\nfive points",
		},
		{
			name:  "an agent step in json mode contributes its json value compact",
			steps: []StepSpec{producingAgent("write")},
			rows:  map[string]resultRow{"write": {status: StepSucceeded, endedAt: 1, output: agentJSONEnvelope(`{"a": 1,  "b": [2, 3]}`)}},
			want:  plain + "\n\n" + `{"a":1,"b":[2,3]}`,
		},
		{
			name:  "a tool step contributes its output as compact json",
			steps: []StepSpec{producingTool("read")},
			rows:  map[string]resultRow{"read": {status: StepSucceeded, endedAt: 1, output: `{"rows": 2,  "ok": true}`}},
			want:  plain + "\n\n" + `{"rows":2,"ok":true}`,
		},
		{
			name:  "a workflow step last falls back to the previous producing step",
			steps: []StepSpec{producingTool("read"), childWorkflowStep("child")},
			rows: map[string]resultRow{
				"read":  {status: StepSucceeded, endedAt: 1, output: `{"rows":2}`},
				"child": {status: StepSucceeded, endedAt: 2, output: `{"status":"succeeded"}`},
			},
			want: plain + "\n\n" + `{"rows":2}`,
		},
		{
			name:  "a wait step payload is not a result",
			steps: []StepSpec{producingAgent("write"), waitingStep("hold")},
			rows: map[string]resultRow{
				"write": {status: StepSucceeded, endedAt: 1, output: agentEnvelope("the summary")},
				"hold":  {status: StepSucceeded, endedAt: 2, output: `{"decision":"go"}`},
			},
			want: plain + "\n\nthe summary",
		},
		{
			name:  "a skipped last step is passed over",
			steps: []StepSpec{producingAgent("write"), producingTool("notify")},
			rows: map[string]resultRow{
				"write":  {status: StepSucceeded, endedAt: 1, output: agentEnvelope("the summary")},
				"notify": {status: StepSkipped, endedAt: 2},
			},
			want: plain + "\n\nthe summary",
		},
		{
			name:  "a failed step does not contribute its output",
			steps: []StepSpec{producingAgent("write"), producingTool("notify")},
			rows: map[string]resultRow{
				"write":  {status: StepSucceeded, endedAt: 1, output: agentEnvelope("the summary")},
				"notify": {status: StepFailed, endedAt: 2, output: `{"error":"late"}`},
			},
			want: plain + "\n\nthe summary",
		},
		{
			name:  "a step with an empty text does not produce",
			steps: []StepSpec{producingAgent("write"), producingAgent("blank")},
			rows: map[string]resultRow{
				"write": {status: StepSucceeded, endedAt: 1, output: agentEnvelope("the summary")},
				"blank": {status: StepSucceeded, endedAt: 2, output: agentEnvelope("   ")},
			},
			want: plain + "\n\nthe summary",
		},
		{
			name:  "of parallel branches the latest ended step wins whatever its id",
			steps: []StepSpec{producingAgent("alpha"), producingAgent("bravo")},
			rows: map[string]resultRow{
				"alpha": {status: StepSucceeded, endedAt: 9, output: agentEnvelope("slow branch")},
				"bravo": {status: StepSucceeded, endedAt: 3, output: agentEnvelope("fast branch")},
			},
			want: plain + "\n\nslow branch",
		},
		{
			name:  "steps that ended together tie to the later one in the definition",
			steps: []StepSpec{producingAgent("zulu"), producingAgent("alpha")},
			rows: map[string]resultRow{
				"zulu":  {status: StepSucceeded, endedAt: 4, output: agentEnvelope("first in the definition")},
				"alpha": {status: StepSucceeded, endedAt: 4, output: agentEnvelope("later in the definition")},
			},
			want: plain + "\n\nlater in the definition",
		},
		{
			name:  "a run with no producing step posts the plain message",
			steps: []StepSpec{childWorkflowStep("child")},
			rows:  map[string]resultRow{"child": {status: StepSucceeded, endedAt: 1, output: `{"status":"succeeded"}`}},
			want:  plain,
		},
		{
			name:      "a failed run posts the failed message without a result",
			steps:     []StepSpec{producingAgent("write")},
			rows:      map[string]resultRow{"write": {status: StepSucceeded, endedAt: 1, output: agentEnvelope("the summary")}},
			runStatus: RunFailed,
			want:      "Sorry, " + resultGoal + " could not be completed.",
		},
		{
			name:      "a cancelled run posts the cancelled message without a result",
			steps:     []StepSpec{producingAgent("write")},
			rows:      map[string]resultRow{"write": {status: StepSucceeded, endedAt: 1, output: agentEnvelope("the summary")}},
			runStatus: RunCancelled,
			want:      resultGoal + " was cancelled.",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runStatus := tc.runStatus
			if runStatus == "" {
				runStatus = RunSucceeded
			}
			store, _ := seedResultRun(t, tc.steps, tc.rows, "owner", runStatus)
			got := sweepCompletions(t, store)
			if len(got) != 1 || got[0] != tc.want {
				t.Fatalf("completion posts = %q, want exactly %q", got, tc.want)
			}
		})
	}
}

func TestChildRunsPostNoCompletionOfTheirOwn(t *testing.T) {
	steps := []StepSpec{producingAgent("write")}
	rows := map[string]resultRow{"write": {status: StepSucceeded, endedAt: 1, output: agentEnvelope("the summary")}}
	store, parent := seedResultRun(t, steps, rows, "owner", RunSucceeded)
	spec, err := store.Definition(t.Context(), parent.DefinitionID, parent.DefinitionVersion)
	if err != nil {
		t.Fatal(err)
	}
	child, err := store.createRun(t.Context(), &runCreate{Spec: spec, Input: &RunInput{}, Origin: resultOrigin("owner"), DurableKey: newID("dur"), ParentRunID: parent.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.updateStep(t.Context(), child.ID, "write", &stepUpdate{Status: StepSucceeded, EndedAt: &resultEpoch, Output: []byte(agentEnvelope("child result"))}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetRunStatus(t.Context(), child.ID, RunSucceeded); err != nil {
		t.Fatal(err)
	}
	got := sweepCompletions(t, store)
	want := "Done: " + resultGoal + "\n\nthe summary"
	if len(got) != 1 || got[0] != want {
		t.Fatalf("completion posts = %q, want only the root run's %q", got, want)
	}
	if claimed, err := store.ClaimCompletion(t.Context(), child.ID, time.Now()); err != nil || !claimed {
		t.Fatalf("the child run's completion was already claimed: claimed = %v, err = %v", claimed, err)
	}
}

func TestCompletionResultIsBoundedAndMarked(t *testing.T) {
	cases := []struct {
		name        string
		text        string
		tainted     bool
		wantMarked  bool
		wantHint    bool
		wantPayload func(text string) string
	}{
		{name: "text at the bound is posted whole", text: strings.Repeat("a", maxCompletionResultRunes), wantPayload: func(text string) string { return text }},
		{
			name: "text over the bound is cut with the inspect hint", text: strings.Repeat("a", maxCompletionResultRunes+1), wantHint: true,
			wantPayload: func(text string) string { return text[:maxCompletionResultRunes] },
		},
		{
			name: "text is cut at a rune boundary", text: strings.Repeat("日", maxCompletionResultRunes+10), wantHint: true,
			wantPayload: func(string) string { return strings.Repeat("日", maxCompletionResultRunes) },
		},
		{name: "tainted output is preceded by the marker line", text: "from the web", tainted: true, wantMarked: true, wantPayload: func(text string) string { return text }},
		{name: "untainted output carries no marker", text: "from the owner", wantPayload: func(text string) string { return text }},
		{
			name: "tainted long output carries the marker and the hint", text: strings.Repeat("b", maxCompletionResultRunes+5), tainted: true, wantMarked: true, wantHint: true,
			wantPayload: func(text string) string { return text[:maxCompletionResultRunes] },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rows := map[string]resultRow{"write": {status: StepSucceeded, endedAt: 1, output: agentEnvelope(tc.text), tainted: tc.tainted}}
			store, run := seedResultRun(t, []StepSpec{producingAgent("write")}, rows, "owner", RunSucceeded)
			got := sweepCompletions(t, store)
			if len(got) != 1 {
				t.Fatalf("completion posts = %q, want one", got)
			}
			want := "Done: " + resultGoal + "\n\n"
			if tc.wantMarked {
				want += taintedResultMarker + "\n"
			}
			want += tc.wantPayload(tc.text)
			if tc.wantHint {
				want += fmt.Sprintf("… (truncated; aura workflow inspect %s)", run.ID)
			}
			if got[0] != want {
				t.Fatalf("completion = %q, want %q", got[0], want)
			}
			if !utf8.ValidString(got[0]) {
				t.Fatal("the completion is not valid UTF-8")
			}
			if strings.Contains(got[0], taintedResultMarker) != tc.wantMarked {
				t.Fatalf("marker present = %v, want %v", !tc.wantMarked, tc.wantMarked)
			}
		})
	}
}

type resultProbe struct {
	agents map[string]string
	tools  map[string]string
	taints map[string]bool
}

func (p *resultProbe) RunStep(_ context.Context, call *AgentStepCall) (AgentStepResult, error) {
	return AgentStepResult{Output: json.RawMessage(p.agents[call.StepID]), Tainted: p.tainted(call.StepID)}, nil
}

func (p *resultProbe) Invoke(ctx context.Context, _ string, _ json.RawMessage) (ToolStepResult, error) {
	call, _ := StepCallFrom(ctx)
	return ToolStepResult{Output: json.RawMessage(p.tools[call.StepID]), Tainted: p.tainted(call.StepID)}, nil
}

func (p *resultProbe) tainted(stepID string) bool { return p.taints[stepID] }

func runResultWorkflow(t *testing.T, steps []StepSpec, probe *resultProbe, class string) (*Store, *RunSummary) {
	t.Helper()
	fake := durabletest.NewFake()
	store := newTestStore(t)
	interpreter := mustNewInterpreter(t, store, fake, &Options{MaxConcurrentSteps: 2, AgentSteps: probe, Tools: probe, Driver: DriverSerial})
	spec := &Spec{ID: "report", Goal: resultGoal, Version: 1, Source: SourceDefined, Steps: steps}
	if err := interpreter.Install(t.Context(), []*Spec{spec}, taintTestDeps()); err != nil {
		t.Fatalf("Install: %v", err)
	}
	seedMigrationSession(t, store.db, "ops/session", "discord:111")
	run, err := store.newRun(t.Context(), spec, &RunInput{}, resultOrigin(class))
	if err != nil {
		t.Fatalf("newRun: %v", err)
	}
	inv, _ := holdInvocation(t, fake, "holder-result")
	if terminal, err := interpreter.DriveSerial(t.Context(), inv, run.ID); err != nil || terminal != RunSucceeded {
		t.Fatalf("DriveSerial = %s, %v; want %s", terminal, err, RunSucceeded)
	}
	return store, run
}

func TestCompletionNeverShowsPrivateOutputToANonOwner(t *testing.T) {
	const plain = "Done: " + resultGoal
	const marker = taintedResultMarker + "\n"
	probe := func() *resultProbe {
		return &resultProbe{
			agents: map[string]string{
				"read":      agentEnvelope("workspace notes"),
				"summarize": agentEnvelope("notes summary"),
				"research":  agentEnvelope("web findings"),
			},
			tools:  map[string]string{"record": `{"saved":true}`, "fetch": `{"page":"news"}`},
			taints: map[string]bool{"research": true, "fetch": true},
		}
	}
	cases := []struct {
		name  string
		steps []StepSpec
		class string
		want  string
	}{
		{name: "an owner run shows a private tool output", steps: []StepSpec{producingTool("record")}, class: "owner", want: plain + "\n\n" + `{"saved":true}`},
		{name: "an owner run shows a workspace agent output", steps: []StepSpec{labelStep("read", workspaceAgent())}, class: "owner", want: plain + "\n\nworkspace notes"},
		{name: "a trusted run hides a non-web tool output", steps: []StepSpec{producingTool("record")}, class: "trusted", want: plain},
		{name: "a public run hides a non-web tool output", steps: []StepSpec{producingTool("record")}, class: "public", want: plain},
		{name: "a public run hides a workspace agent output", steps: []StepSpec{labelStep("read", workspaceAgent())}, class: "public", want: plain},
		{
			name:  "a trusted run hides an agent output downstream of a workspace agent",
			steps: []StepSpec{labelStep("read", workspaceAgent()), labelStep("summarize", agentExecutor("engineer"), "read")}, class: "trusted", want: plain,
		},
		{name: "a public run shows a web tool output", steps: []StepSpec{labelStep("fetch", toolExecutor("web_fetch"))}, class: "public", want: plain + "\n\n" + marker + `{"page":"news"}`},
		{name: "a trusted run shows a web agent output", steps: []StepSpec{labelStep("research", webAgent())}, class: "trusted", want: plain + "\n\n" + marker + "web findings"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, _ := runResultWorkflow(t, tc.steps, probe(), tc.class)
			got := sweepCompletions(t, store)
			if len(got) != 1 || got[0] != tc.want {
				t.Fatalf("completion posts = %q, want exactly %q", got, tc.want)
			}
		})
	}

	defaultLabels := []struct {
		name  string
		class string
		want  string
	}{
		{name: "a row with the default labels counts as private for a public run", class: "public", want: plain},
		{name: "a row with the default labels shows to the owner as tainted", class: "owner", want: plain + "\n\n" + marker + "the summary"},
	}
	for _, tc := range defaultLabels {
		t.Run(tc.name, func(t *testing.T) {
			store, run := seedResultRun(t, []StepSpec{producingAgent("write")}, nil, tc.class, RunSucceeded)
			if _, err := store.db.ExecContext(t.Context(),
				`UPDATE workflow_step_run SET status = 'succeeded', ended_at = ?, output_json = ? WHERE run_id = ?`,
				resultEpoch.Format(time.RFC3339Nano), agentEnvelope("the summary"), run.ID); err != nil {
				t.Fatal(err)
			}
			got := sweepCompletions(t, store)
			if len(got) != 1 || got[0] != tc.want {
				t.Fatalf("completion posts = %q, want exactly %q", got, tc.want)
			}
		})
	}
}

func TestStepLabelsArePersistedAndSurviveARestart(t *testing.T) {
	steps := []StepSpec{
		labelStep("fetch", toolExecutor("web_fetch")),
		labelStep("record", toolExecutor("read_file")),
		labelStep("research", webAgent()),
		labelStep("read", workspaceAgent()),
	}
	probe := &resultProbe{
		agents: map[string]string{"research": agentEnvelope("web findings"), "read": agentEnvelope("workspace notes")},
		tools:  map[string]string{"fetch": `{"page":"news"}`, "record": `{"saved":true}`},
		taints: map[string]bool{"fetch": true, "research": true},
	}
	store, run := runResultWorkflow(t, steps, probe, "public")

	restarted := mustNewStore(t, store.db)
	got, err := restarted.Steps(t.Context(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]struct{ tainted, private bool }{
		"fetch":    {tainted: true, private: false},
		"record":   {tainted: false, private: true},
		"research": {tainted: true, private: false},
		"read":     {tainted: false, private: true},
	}
	if len(got) != len(want) {
		t.Fatalf("steps = %d, want %d", len(got), len(want))
	}
	for _, step := range got {
		if labels := want[step.StepID]; step.Tainted != labels.tainted || step.Private != labels.private {
			t.Errorf("step %s labels after a restart = tainted %v private %v, want tainted %v private %v", step.StepID, step.Tainted, step.Private, labels.tainted, labels.private)
		}
	}
	posts := sweepCompletions(t, restarted)
	if len(posts) != 1 || !strings.HasPrefix(posts[0], "Done: "+resultGoal) {
		t.Fatalf("completion posts after a restart = %q, want one Done message", posts)
	}
}
