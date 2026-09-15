package sync

import (
	"context"
	"strings"
)

type AdvanceDecision string

const (
	AdvanceFastForward AdvanceDecision = "fast_forward"
	AdvanceUpToDate    AdvanceDecision = "up_to_date"
	AdvanceConflict    AdvanceDecision = "conflict"
)

type RefGraph interface {
	IsAncestor(ctx context.Context, ancestor, descendant string) (bool, error)
}

func DecideAdvance(_ context.Context, localRef, remoteRef string) (AdvanceDecision, error) {
	if strings.TrimSpace(localRef) == "" || strings.TrimSpace(remoteRef) == "" {
		return AdvanceConflict, Errorf(ErrorCodeConflict, "sync refs are not established")
	}
	if localRef == remoteRef {
		return AdvanceUpToDate, nil
	}
	return AdvanceFastForward, nil
}

func CheckFastForward(ctx context.Context, graph RefGraph, localRef, remoteRef string) (AdvanceDecision, error) {
	if ctx == nil {
		return AdvanceConflict, errNilArgument("ctx")
	}
	if graph == nil {
		return AdvanceConflict, errNilArgument("graph")
	}
	decision, err := DecideAdvance(ctx, localRef, remoteRef)
	if err != nil {
		return AdvanceConflict, err
	}
	if decision != AdvanceFastForward {
		return decision, nil
	}
	ancestor, err := graph.IsAncestor(ctx, localRef, remoteRef)
	if err != nil {
		return AdvanceConflict, err
	}
	if !ancestor {
		return AdvanceConflict, Errorf(ErrorCodeConflict, "remote history is not a descendant of local")
	}
	return AdvanceFastForward, nil
}
