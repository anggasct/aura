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
	ErasureLineSessions           = "sessions"
	ErasureLineRunSessions        = "run_sessions"
	ErasureLineMemoryDocuments    = "memory_documents"
	ErasureLineGrants             = "grants"
	ErasureLineStandingGrants     = "standing_grants"
	ErasureLinePairingRequests    = "pairing_requests"
	ErasureLineUsageLinks         = "usage_links"
	ErasureLineEmailEffects       = "email_effects"
	ErasureLineWorkflowRuns       = "workflow_runs"
	ErasureLineWorkflowApprovals  = "workflow_approvals"
	ErasureLineLegacyStepSessions = "legacy_step_sessions"
	ErasureLineLiveWorkflowRuns   = "live_workflow_runs"
)

var erasureLineOrder = [...]string{
	ErasureLineSessions,
	ErasureLineRunSessions,
	ErasureLineMemoryDocuments,
	ErasureLineGrants,
	ErasureLineStandingGrants,
	ErasureLinePairingRequests,
	ErasureLineUsageLinks,
	ErasureLineEmailEffects,
	ErasureLineWorkflowRuns,
	ErasureLineWorkflowApprovals,
	ErasureLineLegacyStepSessions,
	ErasureLineLiveWorkflowRuns,
}

var errErasureDryRun = errors.New("erasure dry run")

func isDeclaredErasureLine(line string) bool {
	return slices.Contains(erasureLineOrder[:], line)
}

type ErasureScope struct {
	PrincipalID string
	Apply       bool
	KeepGrants  bool
}

type ErasureState struct {
	scope  ErasureScope
	counts map[string]int
}

func (s *ErasureState) Scope() ErasureScope {
	if s == nil {
		return ErasureScope{}
	}
	return s.scope
}

func (s *ErasureState) Record(line string, count int) error {
	if s == nil {
		return errNilArgument("state")
	}
	if !isDeclaredErasureLine(line) {
		return fault.New(ErrorCodeInvalidArgument, fmt.Sprintf("undeclared erasure line %q", line))
	}
	if s.counts == nil {
		s.counts = make(map[string]int, len(erasureLineOrder))
	}
	s.counts[line] = count
	return nil
}

func (s *ErasureState) Count(line string) int {
	if s == nil {
		return 0
	}
	return s.counts[line]
}

type Eraser interface {
	ErasePrincipal(ctx context.Context, state *ErasureState) error
}

type ErasureReport struct {
	counts map[string]int
}

func NewErasureReport(counts map[string]int) (ErasureReport, error) {
	copied := make(map[string]int, len(counts))
	for line, count := range counts {
		if !isDeclaredErasureLine(line) {
			return ErasureReport{}, fault.New(ErrorCodeInvalidArgument, fmt.Sprintf("undeclared erasure line %q", line))
		}
		copied[line] = count
	}
	return ErasureReport{counts: copied}, nil
}

func (r ErasureReport) Count(line string) int {
	return r.counts[line]
}

func (r ErasureReport) Lines() []string {
	lines := make([]string, len(erasureLineOrder))
	for i, line := range erasureLineOrder {
		lines[i] = fmt.Sprintf("%s: %d", line, r.counts[line])
	}
	return lines
}

func Erase(ctx context.Context, db *sql.DB, scope ErasureScope, erasers []Eraser) (ErasureReport, error) {
	if db == nil {
		return ErasureReport{}, errors.New("erase principal: database must not be nil")
	}
	if scope.PrincipalID == "" || !strings.Contains(scope.PrincipalID, ":") {
		return ErasureReport{}, fmt.Errorf("erase principal: %q is not a channel principal", scope.PrincipalID)
	}
	state := &ErasureState{
		scope:  scope,
		counts: make(map[string]int, len(erasureLineOrder)),
	}
	err := sqlite.RunInTx(ctx, db, func(ctx context.Context) error {
		for _, eraser := range erasers {
			if eraser == nil {
				return errNilArgument("eraser")
			}
			if err := eraser.ErasePrincipal(ctx, state); err != nil {
				return err
			}
		}
		if !scope.Apply {
			return errErasureDryRun
		}
		return nil
	})
	report := ErasureReport{counts: maps.Clone(state.counts)}
	if errors.Is(err, errErasureDryRun) {
		return report, nil
	}
	if err != nil {
		return report, err
	}
	return report, nil
}

type AccessEraser struct {
	db *sql.DB
}

func NewAccessEraser(db *sql.DB) *AccessEraser {
	return &AccessEraser{db: db}
}

func (e *AccessEraser) ErasePrincipal(ctx context.Context, state *ErasureState) error {
	if e == nil || e.db == nil {
		return errNilArgument("db")
	}
	if state == nil {
		return errNilArgument("state")
	}
	scope := state.Scope()
	conn := sqlite.Conn(ctx, e.db)
	targets := []struct {
		line  string
		count string
		erase string
		grant bool
	}{
		{
			line:  ErasureLineGrants,
			count: `SELECT COUNT(*) FROM principal_grant WHERE principal_id = ?`,
			erase: `DELETE FROM principal_grant WHERE principal_id = ?`,
			grant: true,
		},
		{
			line:  ErasureLineStandingGrants,
			count: `SELECT COUNT(*) FROM standing_grant WHERE principal_id = ?`,
			erase: `DELETE FROM standing_grant WHERE principal_id = ?`,
		},
		{
			line:  ErasureLinePairingRequests,
			count: `SELECT COUNT(*) FROM pairing_request WHERE principal_id = ?`,
			erase: `DELETE FROM pairing_request WHERE principal_id = ?`,
		},
	}
	for _, target := range targets {
		if target.grant && scope.KeepGrants {
			continue
		}
		var count int
		if err := conn.QueryRowContext(ctx, target.count, scope.PrincipalID).Scan(&count); err != nil {
			return classify(fmt.Errorf("count principal data: %w", err))
		}
		if err := state.Record(target.line, count); err != nil {
			return err
		}
		if !scope.Apply || count == 0 {
			continue
		}
		if _, err := conn.ExecContext(ctx, target.erase, scope.PrincipalID); err != nil {
			return classify(fmt.Errorf("erase principal data: %w", err))
		}
	}
	return nil
}
