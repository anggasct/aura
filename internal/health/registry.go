package health

import (
	"context"
	"slices"
	"sync"
	"time"

	"errors"
	"fmt"

	"golang.org/x/sync/errgroup"
)

// Severity is an operator-facing triage level. It is derived from Status by
// default and carried separately so a degraded condition an operator must
// act on can be escalated without a new Status value.
type Severity string

const (
	SeverityInfo     Severity = "info"
	SeverityWarning  Severity = "warning"
	SeverityCritical Severity = "critical"
)

// SeverityFor maps a status to its default severity.
func SeverityFor(s Status) Severity {
	switch s {
	case StatusDown:
		return SeverityCritical
	case StatusUnknown, StatusDegraded:
		return SeverityWarning
	default:
		return SeverityInfo
	}
}

// Remediation IDs. Stable identifiers an operator can look up; the detail
// string is human text, the ID is the contract.
const (
	RemediationNone            = ""
	RemediationRunMigrations   = "run-migrations"
	RemediationRestoreBackup   = "restore-backup"
	RemediationConfigureModels = "configure-models"
	RemediationReviewSandbox   = "review-sandbox-host"
	RemediationReconcileState  = "reconcile-durable-state"
	RemediationRepairStorage   = "repair-storage"
)

// Scope labels where a finding applies. Local findings are safe to print in
// full; the distinction exists so future remote consumers can filter.
const (
	ScopeLocal = "local"
)

// RegisteredCheck binds a checker to its execution budget. Timeout bounds one
// evaluation; Freshness bounds how long the previous observation may be
// served before it is reported stale.
type RegisteredCheck struct {
	ID          string
	Checker     Checker
	Timeout     time.Duration
	Freshness   time.Duration
	Scope       string
	Remediation string
}

// Registry evaluates a fixed set of checks with a bounded concurrency budget
// and remembers the first and last time each finding was observed. It never
// mutates observed state: checkers receive the evaluation context only.
type Registry struct {
	checks     []RegisteredCheck
	maxRunning int
	now        func() time.Time

	mu   sync.Mutex
	seen map[string]Finding
}

// NewRegistry builds a registry over the given checks. Duplicate check IDs
// are rejected so a finding always maps to exactly one checker.
func NewRegistry(checks ...RegisteredCheck) (*Registry, error) {
	ids := make(map[string]struct{}, len(checks))
	normalized := make([]RegisteredCheck, 0, len(checks))
	for _, check := range checks {
		if check.ID == "" {
			return nil, &Error{Code: ErrorCodeInvalidCheck, Detail: "check id must not be empty"}
		}
		if check.Checker == nil {
			return nil, &Error{Code: ErrorCodeInvalidCheck, Detail: fmt.Sprintf("check %q has no checker", check.ID)}
		}
		if _, ok := ids[check.ID]; ok {
			return nil, &Error{Code: ErrorCodeInvalidCheck, Detail: fmt.Sprintf("duplicate check id %q", check.ID)}
		}
		ids[check.ID] = struct{}{}
		if check.Timeout <= 0 {
			check.Timeout = defaultCheckTimeout
		}
		if check.Freshness <= 0 {
			check.Freshness = defaultCheckFreshness
		}
		if check.Scope == "" {
			check.Scope = ScopeLocal
		}
		normalized = append(normalized, check)
	}
	return &Registry{
		checks:     normalized,
		maxRunning: maxCheckConcurrency,
		now:        func() time.Time { return time.Now().UTC() },
		seen:       make(map[string]Finding, len(normalized)),
	}, nil
}

const (
	defaultCheckTimeout   = 5 * time.Second
	defaultCheckFreshness = 2 * time.Minute
	maxCheckConcurrency   = 8
	staleCode             = "check_stale"
	timeoutCode           = "check_timeout"
	ErrorCodeInvalidCheck = "invalid_check"
)

// Evaluate runs every check within its budget and returns the observed
// findings in a stable order (check id, component, code). A check that
// overruns its deadline yields a stale unknown finding carrying the last
// observation's summary instead of blocking the evaluation.
func (r *Registry) Evaluate(ctx context.Context) []Finding {
	type result struct {
		check    *RegisteredCheck
		findings []Finding
	}
	results := make([]result, len(r.checks))
	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(min(len(r.checks), r.maxRunning))
	for i := range r.checks {
		group.Go(func() error {
			findings := r.runCheck(groupCtx, &r.checks[i])
			results[i] = result{check: &r.checks[i], findings: findings}
			return nil
		})
	}
	// The only error groupCtx carries is the parent's cancellation, which
	// the caller already observes; per-check failures are findings.
	_ = group.Wait()

	all := make([]Finding, 0, len(r.checks))
	for _, res := range results {
		for j := range res.findings {
			all = append(all, r.stamp(res.check, &res.findings[j]))
		}
	}
	slices.SortFunc(all, func(a, b Finding) int {
		if c := compareStrings(a.Component, b.Component); c != 0 {
			return c
		}
		if c := compareStrings(a.Code, b.Code); c != 0 {
			return c
		}
		return compareStrings(a.Detail, b.Detail)
	})
	return all
}

func (r *Registry) runCheck(ctx context.Context, check *RegisteredCheck) []Finding {
	runCtx, cancel := context.WithTimeout(ctx, check.Timeout)
	defer cancel()

	type outcome struct {
		findings []Finding
		err      error
	}
	done := make(chan outcome, 1)
	go func() {
		findings, err := safeCheck(runCtx, check)
		done <- outcome{findings: findings, err: err}
	}()
	select {
	case out := <-done:
		if out.err != nil {
			return []Finding{{
				Component: check.ID,
				Code:      timeoutCode,
				Status:    StatusUnknown,
				Detail:    "check failed: " + redactedCheckError(out.err),
			}}
		}
		return out.findings
	case <-runCtx.Done():
		last, hasLast := r.lastFinding(check)
		detail := "check overran its deadline"
		if hasLast {
			detail = fmt.Sprintf("check overran its deadline; last observation: %s %s", last.Status, last.Code)
		}
		return []Finding{{
			Component: check.ID,
			Code:      timeoutCode,
			Status:    StatusUnknown,
			Detail:    detail,
			Stale:     true,
		}}
	}
}

// safeCheck converts a checker panic into a finding; a diagnostics sweep must
// never take the process down because an observed subsystem misbehaved.
func safeCheck(ctx context.Context, check *RegisteredCheck) (findings []Finding, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			findings = []Finding{{
				Component: check.ID,
				Code:      "check_panicked",
				Status:    StatusUnknown,
				Detail:    "check panicked",
			}}
		}
	}()
	return check.Checker.Check(ctx), nil
}

func (r *Registry) lastFinding(check *RegisteredCheck) (Finding, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	finding, ok := r.seen[check.ID]
	return finding, ok
}

// stamp fills the registry-owned fields: severity, scope, remediation, stable
// finding ID, and first/last-seen timestamps from the observation history.
// The observed finding is copied first — checker-owned slices must never be
// mutated through the registry.
func (r *Registry) stamp(check *RegisteredCheck, observed *Finding) Finding {
	finding := *observed
	now := r.now()
	if finding.CheckedAt.IsZero() {
		finding.CheckedAt = now
	}
	if finding.Severity == "" {
		finding.Severity = SeverityFor(finding.Status)
	}
	if finding.Scope == "" {
		finding.Scope = check.Scope
	}
	if finding.Remediation == "" {
		finding.Remediation = check.Remediation
	}
	if finding.Component == "" {
		finding.Component = check.ID
	}
	if finding.ID == "" {
		finding.ID = finding.Component + "/" + finding.Code
	}
	finding.LastSeen = finding.CheckedAt

	r.mu.Lock()
	defer r.mu.Unlock()
	key := check.ID + "\x00" + finding.ID
	previous, ok := r.seen[key]
	if ok {
		finding.FirstSeen = previous.FirstSeen
	} else {
		finding.FirstSeen = finding.CheckedAt
	}
	r.seen[key] = finding
	return finding
}

// WorstStatus returns the worst status across findings.
func WorstStatus(findings []Finding) Status {
	worst := StatusUp
	for i := range findings {
		if severity(findings[i].Status) > severity(worst) {
			worst = findings[i].Status
		}
	}
	return worst
}

// WorstSeverity returns the worst severity across findings.
func WorstSeverity(findings []Finding) Severity {
	worst := SeverityInfo
	for i := range findings {
		if severityRank(findings[i].Severity) > severityRank(worst) {
			worst = findings[i].Severity
		}
	}
	return worst
}

func severityRank(s Severity) int {
	switch s {
	case SeverityCritical:
		return 2
	case SeverityWarning:
		return 1
	default:
		return 0
	}
}

func compareStrings(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

// Error is the health package's typed error.
type Error struct {
	Code   ErrorCode
	Detail string
}

type ErrorCode string

func (e *Error) Error() string {
	return string(e.Code) + ": " + e.Detail
}

// CodeOf returns the typed code of a health error.
func CodeOf(err error) (ErrorCode, bool) {
	var target *Error
	if errors.As(err, &target) {
		return target.Code, true
	}
	return "", false
}

// redactedCheckError strips anything a subsystem error might carry that does
// not belong in a finding: only the error class is operator-relevant here.
func redactedCheckError(err error) string {
	if code, ok := CodeOf(err); ok {
		return string(code)
	}
	return "unknown"
}
