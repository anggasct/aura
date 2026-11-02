package store

import (
	"testing"
	"time"
)

func TestPersonaEvalResultsAppendAndLatestWins(t *testing.T) {
	db := newTestDB(t)
	evals := NewPersonaEvalStore(db)
	ctx := t.Context()
	base := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	if _, found, err := evals.Latest(ctx, "lobby", "digest-a", EvalLayerPolicy); err != nil || found {
		t.Fatalf("Latest before any run = %v, %v", found, err)
	}
	for i, passed := range []bool{true, false} {
		if _, err := evals.Record(ctx, &PersonaEvalResult{AgentID: "lobby", Digest: "digest-a", Layer: EvalLayerPolicy, Passed: passed, CasesTotal: 4, CasesFailed: i, FailedCases: []string{"case-" + string(rune('a'+i))}, CreatedAt: base.Add(time.Duration(i) * time.Minute)}); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	latest, found, err := evals.Latest(ctx, "lobby", "digest-a", EvalLayerPolicy)
	if err != nil || !found || latest.Passed || latest.CasesFailed != 1 || len(latest.FailedCases) != 1 {
		t.Fatalf("Latest = %+v, %v, %v; want the failing second run", latest, found, err)
	}
	if _, found, _ := evals.Latest(ctx, "lobby", "digest-b", EvalLayerPolicy); found {
		t.Fatal("result leaked to another digest")
	}
	if _, found, _ := evals.Latest(ctx, "lobby", "digest-a", EvalLayerBehavior); found {
		t.Fatal("result leaked to another layer")
	}
	list, err := evals.List(ctx, "lobby", 10)
	if err != nil || len(list) != 2 || list[0].CreatedAt.Before(list[1].CreatedAt) {
		t.Fatalf("List = %+v, %v; want newest first", list, err)
	}
	if _, err := evals.Record(ctx, &PersonaEvalResult{AgentID: "lobby", Digest: "d", Layer: "vibes"}); err == nil {
		t.Fatal("unknown layer accepted")
	}
}
