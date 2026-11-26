package sqlite

import (
	"context"
	"testing"
	"time"
)

type stubPruner struct {
	calls int
	got   []PruneRequest
}

func (p *stubPruner) PruneEnded(_ context.Context, req PruneRequest) (int, error) {
	p.calls++
	p.got = append(p.got, req)
	if p.calls == 1 {
		return req.Limit, nil
	}
	return 1, nil
}

func TestRetentionSweeperPassesConfiguredHandlers(t *testing.T) {
	pruner := &stubPruner{}
	sweeper, err := NewRetentionSweeper(&RetentionSweeperOptions{
		Pruner:    pruner,
		Handlers:  []string{"broadcast", "turn"},
		Orphans:   func(context.Context) (int, error) { return 2, nil },
		Retention: 24 * time.Hour,
		Batch:     3,
	})
	if err != nil {
		t.Fatalf("NewRetentionSweeper: %v", err)
	}
	counts, err := sweeper.DeleteOrphansThenPrune(t.Context())
	if err != nil {
		t.Fatalf("DeleteOrphansThenPrune: %v", err)
	}
	if counts.Orphaned != 2 || counts.Deleted != 4 {
		t.Fatalf("counts = %+v, want {Orphaned:2 Deleted:4}", counts)
	}
	if len(pruner.got) != 2 || len(pruner.got[0].Handlers) != 2 {
		t.Fatalf("requests = %+v", pruner.got)
	}
}
