package principal

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/sqlite"
)

const (
	RekeyLineSessions             = "sessions"
	RekeyLineMemoryDocuments      = "memory_documents"
	RekeyLineProfileFacts         = "profile_facts"
	RekeyLineProfileFactConflicts = "profile_fact_conflicts"
)

var rekeyLines = [...]string{
	RekeyLineSessions,
	RekeyLineMemoryDocuments,
	RekeyLineProfileFacts,
	RekeyLineProfileFactConflicts,
}

var errRekeyDryRun = errors.New("rekey dry run")

func isDeclaredRekeyLine(line string) bool {
	return slices.Contains(rekeyLines[:], line)
}

type OwnerMove struct {
	From string
	To   string
}

type RekeyState struct {
	counts map[string]int64
}

func (s *RekeyState) Record(line string, count int64) error {
	if s == nil {
		return errNilArgument("state")
	}
	if !isDeclaredRekeyLine(line) {
		return fault.New(ErrorCodeInvalidArgument, fmt.Sprintf("undeclared rekey line %q", line))
	}
	if s.counts == nil {
		s.counts = make(map[string]int64, len(rekeyLines))
	}
	s.counts[line] += count
	return nil
}

func (s *RekeyState) Count(line string) int64 {
	if s == nil {
		return 0
	}
	return s.counts[line]
}

type Rekeyer interface {
	RekeyOwner(ctx context.Context, state *RekeyState, move OwnerMove) error
}

type RekeyReport struct {
	counts map[string]int64
}

func (r RekeyReport) Count(line string) int64 {
	return r.counts[line]
}

func (r RekeyReport) Moved() int64 {
	return r.counts[RekeyLineSessions] + r.counts[RekeyLineMemoryDocuments] + r.counts[RekeyLineProfileFacts]
}

func Rekey(ctx context.Context, db *sql.DB, moves []OwnerMove, apply bool, rekeyers []Rekeyer) (RekeyReport, error) {
	if db == nil {
		return RekeyReport{}, fault.New(ErrorCodeInvalidArgument, "database must not be nil")
	}
	if err := validateOwnerMoves(moves); err != nil {
		return RekeyReport{}, err
	}
	state := &RekeyState{counts: make(map[string]int64, len(rekeyLines))}
	err := sqlite.RunInTx(ctx, db, func(ctx context.Context) error {
		for _, move := range moves {
			for _, rekeyer := range rekeyers {
				if rekeyer == nil {
					return errNilArgument("rekeyer")
				}
				if err := rekeyer.RekeyOwner(ctx, state, move); err != nil {
					return err
				}
			}
		}
		if !apply {
			return errRekeyDryRun
		}
		return nil
	})
	if errors.Is(err, errRekeyDryRun) {
		return RekeyReport{counts: maps.Clone(state.counts)}, nil
	}
	if err != nil {
		return RekeyReport{}, fmt.Errorf("rekey owners: %w", classify(err))
	}
	return RekeyReport{counts: maps.Clone(state.counts)}, nil
}

func validateOwnerMoves(moves []OwnerMove) error {
	targets := make(map[string]struct{}, len(moves))
	sources := make(map[string]struct{}, len(moves))
	for _, move := range moves {
		if strings.TrimSpace(move.From) == "" || strings.TrimSpace(move.To) == "" || move.From == move.To {
			return fault.New(ErrorCodeInvalidArgument, "owner move needs distinct non-empty source and target")
		}
		if _, dup := sources[move.From]; dup {
			return fault.New(ErrorCodeInvalidArgument, "owner move source is listed more than once")
		}
		sources[move.From] = struct{}{}
		targets[move.To] = struct{}{}
	}
	for source := range sources {
		if _, chained := targets[source]; chained {
			return fault.New(ErrorCodeInvalidArgument, "owner move source is also a target")
		}
	}
	return nil
}
