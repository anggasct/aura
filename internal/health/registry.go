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

type Severity string

const (
	SeverityInfo     Severity = "info"
	SeverityWarning  Severity = "warning"
	SeverityCritical Severity = "critical"
)

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

const (
	RemediationNone            = ""
	RemediationRunMigrations   = "run-migrations"
	RemediationRestoreBackup   = "restore-backup"
	RemediationConfigureModels = "configure-models"
	RemediationReviewSandbox   = "review-sandbox-host"
	RemediationReconcileState  = "reconcile-durable-state"
	RemediationRepairStorage   = "repair-storage"
	RemediationReviewProfile   = "review-capability-profile"
	RemediationRelieveLimits   = "relieve-resource-limits"
)

const (
	ScopeLocal = "local"
)

type RegisteredCheck struct {
	ID          string
	Checker     Checker
	Timeout     time.Duration
	Freshness   time.Duration
	Scope       string
	Remediation string
}

type Registry struct {
	checks     []RegisteredCheck
	maxRunning int
	now        func() time.Time

	mu          sync.Mutex
	seen        map[string]Finding
	lastByCheck map[string]Finding
}

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
		checks:      normalized,
		maxRunning:  maxCheckConcurrency,
		now:         func() time.Time { return time.Now().UTC() },
		seen:        make(map[string]Finding, len(normalized)),
		lastByCheck: make(map[string]Finding, len(normalized)),
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
	finding, ok := r.lastByCheck[check.ID]
	return finding, ok
}

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
	r.lastByCheck[check.ID] = finding
	return finding
}

func WorstStatus(findings []Finding) Status {
	worst := StatusUp
	for i := range findings {
		if severity(findings[i].Status) > severity(worst) {
			worst = findings[i].Status
		}
	}
	return worst
}

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

type Error struct {
	Code   ErrorCode
	Detail string
}

type ErrorCode string

func (e *Error) Error() string {
	return string(e.Code) + ": " + e.Detail
}

func CodeOf(err error) (ErrorCode, bool) {
	var target *Error
	if errors.As(err, &target) {
		return target.Code, true
	}
	return "", false
}

func redactedCheckError(err error) string {
	if code, ok := CodeOf(err); ok {
		return string(code)
	}
	return "unknown"
}
