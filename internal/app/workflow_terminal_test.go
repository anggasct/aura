package app

import (
	"strings"
	"testing"
	"time"

	toolsbuiltin "github.com/anggasct/aura/internal/modules/builtin"
	"github.com/anggasct/aura/internal/modules/workflow"
	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/store"
)

func TestProposalOutputsAreTerminalSafe(t *testing.T) {
	gf := writeWorkflowFixtures(t, validWorkflowYAML)
	db := openAccessDB(t, gf.configPath)
	runs := workflow.NewStore(db)
	sessionID := "ops/term\x1b[2Jinal"
	if err := store.NewSessionService(db).Create(t.Context(), &store.Session{ID: sessionID, OwnerID: principal.OwnerID, Metadata: []byte(`{}`), CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	propose := func(id, goal string) *workflow.Proposal {
		spec := &workflow.Spec{
			ID: "gen_" + id, Goal: goal, Version: 1, Source: workflow.SourceGenerated,
			Steps: []workflow.StepSpec{{ID: "approve", Executor: workflow.ExecutorSpec{Kind: workflow.KindApproval}, Timeout: time.Minute}},
		}
		digest, err := workflow.Digest(spec)
		if err != nil {
			t.Fatal(err)
		}
		proposal := &workflow.Proposal{ID: id, SessionID: sessionID, AgentID: "ops\x1b[2J", PrincipalID: principal.OwnerID, Spec: spec, Digest: digest, CreatedAt: time.Now().UTC()}
		if err := runs.CreateProposal(t.Context(), proposal); err != nil {
			t.Fatal(err)
		}
		return proposal
	}
	shown := propose("prop-shown", "Show the plan")
	listed := propose("prop-listed", "Ship\x1b[2Jit")

	interpreter := workflow.NewInterpreter(runs, queuedRuntime{durable.NewFake()}, nil)
	spec := &workflow.Spec{ID: "terminal_demo", Goal: "Run the demo", Version: 1, Source: workflow.SourceDefined, Steps: []workflow.StepSpec{{ID: "approve", Executor: workflow.ExecutorSpec{Kind: workflow.KindApproval}, Timeout: time.Minute}}}
	if err := interpreter.Install(t.Context(), []*workflow.Spec{spec}, &workflow.ValidationDeps{KnownTools: toolsbuiltin.DefinitionNames(), Agents: builtinRegistry(t), CheckToolCall: acceptToolCall, StepAllowed: acceptStepAllowed}); err != nil {
		t.Fatal(err)
	}
	run, err := interpreter.StartConfigured(t.Context(), spec.ID, nil, principal.OwnerID, principal.ClassOwner, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `UPDATE workflow_run SET goal = 'Ship' || char(27) || '[2Jit' WHERE id = ?`, run.ID); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		args []string
		want []string
	}{
		{name: "proposal show", args: []string{"proposal", "show", shown.ID}, want: []string{`session: ops/term\u{001b}[2Jinal`}},
		{name: "proposals", args: []string{"proposals"}, want: []string{`ops\u{001b}[2J`, `Ship\u{001b}[2Jit`}},
		{name: "runs", args: []string{"runs"}, want: []string{`Ship\u{001b}[2Jit`}},
		{name: "inspect", args: []string{"inspect", run.ID}, want: []string{`goal: Ship\u{001b}[2Jit`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := runWorkflowCommand(t, gf, tc.args...)
			if err != nil {
				t.Fatal(err)
			}
			if strings.ContainsRune(out, 0x1b) {
				t.Fatalf("output holds a raw escape character: %q", out)
			}
			for _, want := range tc.want {
				if !strings.Contains(out, want) {
					t.Fatalf("output lacks %q:\n%s", want, out)
				}
			}
		})
	}
	t.Run("proposal show of a goal that is out of rule prints only its stored plan", func(t *testing.T) {
		out, err := runWorkflowCommand(t, gf, "proposal", "show", listed.ID)
		if err != nil || strings.ContainsRune(out, 0x1b) || !strings.Contains(out, planNotStoredLine) {
			t.Fatalf("proposal show = %q, %v, want the escaped headers and the not-stored line", out, err)
		}
	})
}
