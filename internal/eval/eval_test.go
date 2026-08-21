package eval

import (
	"context"
	"testing"

	"github.com/anggasct/aura/internal/approval"
	"github.com/anggasct/aura/internal/store"
)

func TestGoldenTrajectories(t *testing.T) {
	trajectories, err := LoadTrajectories("testdata/trajectories")
	if err != nil {
		t.Fatalf("LoadTrajectories: %v", err)
	}
	if len(trajectories) == 0 {
		t.Fatal("no golden trajectories loaded")
	}
	for _, traj := range trajectories {
		t.Run(traj.Name, func(t *testing.T) {
			ctx := context.Background()
			rt, clean, err := ScriptedRuntime(ctx, t.TempDir(), traj.Script)
			if err != nil {
				t.Fatalf("ScriptedRuntime: %v", err)
			}
			t.Cleanup(clean)

			events, err := RunTrajectory(ctx, rt, "turn-"+traj.Name)
			if err != nil {
				t.Fatalf("RunTrajectory: %v", err)
			}
			if err := CheckTrajectory(events, traj.WantKinds); err != nil {
				t.Error(err)
			}
		})
	}
}

func TestAdversarialCorpusIsDenied(t *testing.T) {
	cases, err := LoadAbuseCases("testdata/abuse")
	if err != nil {
		t.Fatalf("LoadAbuseCases: %v", err)
	}
	if len(cases) == 0 {
		t.Fatal("no adversarial cases loaded")
	}

	policy := approval.Policy{
		Version: "eval-1",
		Rules: map[string]approval.Rule{
			"shell.exec": {ToolName: "shell.exec", AllowedTrust: []approval.TrustLabel{approval.TrustOwnerInput}},
			"fs.write":   {ToolName: "fs.write", AllowedTrust: []approval.TrustLabel{approval.TrustOwnerInput}, RequiredCapabilities: []string{"filesystem"}},
		},
	}
	handler := func(context.Context, approval.ToolRequest, approval.Constraints) (approval.ToolResult, error) {
		return approval.ToolResult{}, nil
	}
	broker, err := approval.NewEngine(policy, handler)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}

	for _, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) {
			req := tc.Request
			if err := CheckDenied(context.Background(), broker, &req); err != nil {
				t.Error(err)
			}
		})
	}
}

// A trusted, authorized request must still be allowed, so the adversarial
// suite proves discrimination rather than a blanket deny.
func TestAuthorizedRequestIsAllowed(t *testing.T) {
	policy := approval.Policy{
		Version: "eval-1",
		Rules: map[string]approval.Rule{
			"shell.exec": {ToolName: "shell.exec", AllowedTrust: []approval.TrustLabel{approval.TrustOwnerInput}},
		},
	}
	handler := func(context.Context, approval.ToolRequest, approval.Constraints) (approval.ToolResult, error) {
		return approval.ToolResult{}, nil
	}
	broker, err := approval.NewEngine(policy, handler)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	req := &approval.ToolRequest{ToolName: "shell.exec", Trust: approval.TrustOwnerInput, PrincipalID: "owner"}
	decision, err := broker.Evaluate(context.Background(), req)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if decision.Outcome != approval.OutcomeAllow {
		t.Errorf("authorized request outcome = %q, want allow", decision.Outcome)
	}
}

func TestCheckTrajectoryRejectsMissingAndExtraSteps(t *testing.T) {
	events := []store.RuntimeEvent{
		{Sequence: 1, Kind: "turn.accepted"},
		{Sequence: 2, Kind: "tool.requested"},
		{Sequence: 3, Kind: "tool.completed"},
		{Sequence: 4, Kind: "turn.completed"},
	}
	if err := CheckTrajectory(events, []string{"turn.accepted", "tool.requested", "tool.completed", "turn.completed"}); err != nil {
		t.Fatalf("CheckTrajectory(valid): %v", err)
	}
	if err := CheckTrajectory(events[:3], []string{"turn.accepted", "tool.requested", "tool.completed", "turn.completed"}); err == nil {
		t.Fatal("CheckTrajectory accepted a missing terminal step")
	}
	withExtra := append(append([]store.RuntimeEvent{}, events...), store.RuntimeEvent{Sequence: 5, Kind: "policy.bypass"})
	if err := CheckTrajectory(withExtra, []string{"turn.accepted", "tool.requested", "tool.completed", "turn.completed"}); err == nil {
		t.Fatal("CheckTrajectory accepted an extra step")
	}
}

func TestGradeTraceScoresToolApprovalHandoffAndContinuationSignals(t *testing.T) {
	trace := NewTrace("core", "completed", []store.RuntimeEvent{
		{Sequence: 1, Kind: "tool.requested"},
		{Sequence: 2, Kind: "approval.required"},
		{Sequence: 3, Kind: "tool.completed"},
		{Sequence: 4, Kind: "handoff.completed"},
		{Sequence: 5, Kind: "continuation.started"},
		{Sequence: 6, Kind: "turn.completed"},
	})
	grade := GradeTrace(trace, &TraceExpectation{
		Profile: "core", Outcome: "completed", MinScore: 100,
		ExpectedKinds:  []string{"tool.requested", "approval.required", "tool.completed", "handoff.completed", "continuation.started", "turn.completed"},
		RequiredKinds:  []string{"tool.requested", "approval.required", "tool.completed", "handoff.completed", "continuation.started", "turn.completed"},
		ForbiddenKinds: []string{"policy.bypass"},
	})
	if !grade.Passed || grade.Version != "trace-grader.v1" {
		t.Fatalf("grade = %+v, want passing semantic signal grade", grade)
	}
}
