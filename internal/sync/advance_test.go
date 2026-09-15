package sync

import (
	"context"
	"errors"
	"testing"
)

type stubGraph struct {
	ancestor bool
	err      error
}

func (s *stubGraph) IsAncestor(_ context.Context, _, _ string) (bool, error) {
	return s.ancestor, s.err
}

func TestDecideAdvance(t *testing.T) {
	t.Parallel()
	if decision, err := DecideAdvance(t.Context(), "aaa", "aaa"); err != nil || decision != AdvanceUpToDate {
		t.Fatalf("decision = %v,%v want up_to_date", decision, err)
	}
	if decision, err := DecideAdvance(t.Context(), "aaa", "bbb"); err != nil || decision != AdvanceFastForward {
		t.Fatalf("decision = %v,%v want fast_forward", decision, err)
	}
	if _, err := DecideAdvance(t.Context(), "", "bbb"); err == nil {
		t.Fatal("empty ref must fail")
	} else if code, ok := CodeOf(err); !ok || code != ErrorCodeConflict {
		t.Fatalf("code = %v,%v want sync_conflict", code, ok)
	}
}

func TestCheckFastForward(t *testing.T) {
	t.Parallel()
	if decision, err := CheckFastForward(t.Context(), &stubGraph{ancestor: true}, "aaa", "bbb"); err != nil || decision != AdvanceFastForward {
		t.Fatalf("decision = %v,%v want fast_forward", decision, err)
	}
	if _, err := CheckFastForward(t.Context(), &stubGraph{ancestor: false}, "aaa", "bbb"); err == nil {
		t.Fatal("divergence must fail")
	} else if code, ok := CodeOf(err); !ok || code != ErrorCodeConflict {
		t.Fatalf("code = %v,%v want sync_conflict", code, ok)
	}
	if _, err := CheckFastForward(t.Context(), &stubGraph{err: errors.New("boom")}, "aaa", "bbb"); err == nil {
		t.Fatal("graph error must fail")
	}
	if _, err := CheckFastForward(t.Context(), nil, "aaa", "bbb"); err == nil {
		t.Fatal("nil graph must fail")
	}
}
