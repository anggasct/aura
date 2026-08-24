// Package health defines Aura's diagnostics contract: typed findings, a
// bounded evaluator, and checkers for the conditions an operator must detect
// (migration state, capability availability, sandbox support, backup age,
// provider configuration, stuck effects/jobs). Checkers take narrow probes so
// the package stays decoupled from the subsystems it observes; the composition
// root wires real probes and presents the findings (aura status, readiness
// probes).
package health

import (
	"context"
	"fmt"
	"time"
)

// Status is a component's coarse health state, ordered from healthy to failed.
type Status string

const (
	StatusUp       Status = "up"
	StatusDegraded Status = "degraded"
	StatusUnknown  Status = "unknown"
	StatusDown     Status = "down"
)

// Component names. These are stable labels; renaming one is a breaking change
// for diagnostics consumers.
const (
	ComponentMigration  = "migration"
	ComponentCapability = "capability"
	ComponentSandbox    = "sandbox"
	ComponentBackup     = "backup"
	ComponentProvider   = "provider"
	ComponentEffectJob  = "effect_job"
)

// Finding is one typed diagnostic. Detail is operator-facing and redacted: no
// secrets, tokens, absolute paths, or content. ID, Severity, Scope, and
// Remediation are stable contract fields for diagnostics consumers; the
// registry fills ID/Severity/Scope/Remediation/FirstSeen/LastSeen when a
// checker leaves them zero.
type Finding struct {
	ID          string    `json:"id"`
	Component   string    `json:"component"`
	Scope       string    `json:"scope"`
	Code        string    `json:"code"`
	Status      Status    `json:"status"`
	Severity    Severity  `json:"severity"`
	Detail      string    `json:"detail"`
	Remediation string    `json:"remediation,omitempty"`
	Stale       bool      `json:"stale,omitempty"`
	FirstSeen   time.Time `json:"first_seen"`
	LastSeen    time.Time `json:"last_seen"`
	CheckedAt   time.Time `json:"checked_at"`
}

// Checker reports the health of one concern. A checker always returns at least
// one finding describing its component's current state.
type Checker interface {
	Check(ctx context.Context) []Finding
}

// Evaluator runs a fixed set of checkers and aggregates their findings.
type Evaluator struct {
	checkers []Checker
}

// NewEvaluator builds an evaluator over the given checkers.
func NewEvaluator(checkers ...Checker) *Evaluator {
	return &Evaluator{checkers: checkers}
}

// Evaluate runs every checker and returns all findings.
func (e *Evaluator) Evaluate(ctx context.Context) []Finding {
	findings := make([]Finding, 0, len(e.checkers))
	for _, c := range e.checkers {
		findings = append(findings, c.Check(ctx)...)
	}
	return findings
}

// Status returns the worst status across all findings, or StatusUp when there
// are none.
func (e *Evaluator) Status(ctx context.Context) Status {
	findings := e.Evaluate(ctx)
	worst := StatusUp
	for i := range findings {
		if severity(findings[i].Status) > severity(worst) {
			worst = findings[i].Status
		}
	}
	return worst
}

func severity(s Status) int {
	switch s {
	case StatusDown:
		return 3
	case StatusUnknown:
		return 2
	case StatusDegraded:
		return 1
	default:
		return 0
	}
}

// MigrationChecker reports whether the schema is current. Versions probes the
// applied and latest migration versions.
type MigrationChecker struct {
	Versions func(ctx context.Context) (applied, latest int, err error)
}

func (c MigrationChecker) Check(ctx context.Context) []Finding {
	now := time.Now().UTC()
	applied, latest, err := c.Versions(ctx)
	if err != nil {
		return []Finding{{Component: ComponentMigration, Code: "migration_unknown", Status: StatusUnknown, Detail: "migration state unavailable", CheckedAt: now}}
	}
	switch {
	case applied > latest:
		return []Finding{{Component: ComponentMigration, Code: "migration_downgrade", Status: StatusDown, Detail: fmt.Sprintf("schema version %d is newer than this binary supports (%d)", applied, latest), CheckedAt: now}}
	case applied < latest:
		return []Finding{{Component: ComponentMigration, Code: "migration_pending", Status: StatusDegraded, Detail: fmt.Sprintf("schema version %d is behind the latest (%d)", applied, latest), CheckedAt: now}}
	default:
		return []Finding{{Component: ComponentMigration, Code: "ok", Status: StatusUp, Detail: fmt.Sprintf("schema version %d", applied), CheckedAt: now}}
	}
}

// CapabilityStatus is the health-owned view of one capability's availability.
// The composition root maps the capability registry onto this narrow shape so
// the health package need not import the capability domain.
type CapabilityStatus struct {
	Name      string
	Available bool
}

// CapabilityChecker reports capabilities that are unavailable. Statuses probes
// the current capability availability.
type CapabilityChecker struct {
	Statuses func(ctx context.Context) []CapabilityStatus
}

func (c CapabilityChecker) Check(ctx context.Context) []Finding {
	now := time.Now().UTC()
	statuses := c.Statuses(ctx)
	findings := make([]Finding, 0, len(statuses))
	for _, s := range statuses {
		if !s.Available {
			findings = append(findings, Finding{
				Component: ComponentCapability + "/" + s.Name,
				Code:      "capability_unavailable",
				Status:    StatusDown,
				Detail:    "capability unavailable",
				CheckedAt: now,
			})
		}
	}
	if len(findings) == 0 {
		return []Finding{{Component: ComponentCapability, Code: "ok", Status: StatusUp, Detail: "all capabilities available", CheckedAt: now}}
	}
	return findings
}

// SandboxChecker reports whether host containment primitives are available.
// Support returns whether any primitive is usable plus an operator detail.
// An unsupported host is down, not degraded: containment is mandatory for
// effectful execution, so its absence must block intake.
type SandboxChecker struct {
	Support func() (supported bool, detail string)
}

func (c SandboxChecker) Check(_ context.Context) []Finding {
	now := time.Now().UTC()
	supported, detail := c.Support()
	if supported {
		return []Finding{{Component: ComponentSandbox, Code: "ok", Status: StatusUp, Detail: detail, CheckedAt: now}}
	}
	return []Finding{{Component: ComponentSandbox, Code: "sandbox_unavailable", Status: StatusDown, Detail: detail, CheckedAt: now}}
}

// StorageIntakeState classifies whether the storage surface can durably
// accept new writes.
type StorageIntakeState string

const (
	StorageIntakeOK          StorageIntakeState = "ok"
	StorageIntakeUnknown     StorageIntakeState = "unknown"
	StorageIntakeUnreachable StorageIntakeState = "unreachable"
	StorageIntakeReadOnly    StorageIntakeState = "read_only"
	StorageIntakeFull        StorageIntakeState = "full"
)

// StorageChecker reports whether the storage surface accepts durable intake.
// Intake probes writability and free space without mutating state.
type StorageChecker struct {
	Intake func(ctx context.Context) (StorageIntakeState, string)
}

func (c StorageChecker) Check(ctx context.Context) []Finding {
	now := time.Now().UTC()
	state, detail := c.Intake(ctx)
	switch state {
	case StorageIntakeOK:
		return []Finding{{Component: ComponentStorage, Code: "ok", Status: StatusUp, Detail: detail, CheckedAt: now}}
	case StorageIntakeReadOnly:
		return []Finding{{Component: ComponentStorage, Code: "storage_read_only", Status: StatusDown, Detail: detail, CheckedAt: now}}
	case StorageIntakeFull:
		return []Finding{{Component: ComponentStorage, Code: "storage_full", Status: StatusDown, Detail: detail, CheckedAt: now}}
	case StorageIntakeUnreachable:
		return []Finding{{Component: ComponentStorage, Code: "storage_unreachable", Status: StatusDown, Detail: detail, CheckedAt: now}}
	default:
		return []Finding{{Component: ComponentStorage, Code: "storage_unknown", Status: StatusUnknown, Detail: detail, CheckedAt: now}}
	}
}

// BackupChecker reports whether a recent backup exists. LastBackup returns the
// most recent backup time, or an error when none exists.
type BackupChecker struct {
	LastBackup func(ctx context.Context) (time.Time, error)
	MaxAge     time.Duration
	Now        func() time.Time
}

func (c BackupChecker) Check(ctx context.Context) []Finding {
	nowFunc := c.Now
	if nowFunc == nil {
		nowFunc = func() time.Time { return time.Now().UTC() }
	}
	now := nowFunc()
	at, err := c.LastBackup(ctx)
	if err != nil {
		return []Finding{{Component: ComponentBackup, Code: "backup_missing", Status: StatusDown, Detail: "no backup found", CheckedAt: now}}
	}
	if now.Sub(at) > c.MaxAge {
		return []Finding{{Component: ComponentBackup, Code: "backup_stale", Status: StatusDegraded, Detail: fmt.Sprintf("backup older than %s", c.MaxAge), CheckedAt: now}}
	}
	return []Finding{{Component: ComponentBackup, Code: "ok", Status: StatusUp, Detail: "backup current", CheckedAt: now}}
}

// ProviderChecker reports whether a model provider is configured. Probe
// returns nil when a provider resolves for the default turn task.
type ProviderChecker struct {
	Probe func(ctx context.Context) error
}

func (c ProviderChecker) Check(ctx context.Context) []Finding {
	now := time.Now().UTC()
	if err := c.Probe(ctx); err != nil {
		return []Finding{{Component: ComponentProvider, Code: "provider_unavailable", Status: StatusDown, Detail: "no provider configured", CheckedAt: now}}
	}
	return []Finding{{Component: ComponentProvider, Code: "ok", Status: StatusUp, Detail: "provider configured", CheckedAt: now}}
}

// EffectJobChecker reports stuck effects and jobs. Stuck probes the count of
// effect/job records that have not reached a terminal state; the composition
// root wires it to the effect journal once that lands.
type EffectJobChecker struct {
	Stuck func(ctx context.Context) (count int, err error)
}

func (c EffectJobChecker) Check(ctx context.Context) []Finding {
	now := time.Now().UTC()
	count, err := c.Stuck(ctx)
	if err != nil {
		return []Finding{{Component: ComponentEffectJob, Code: "effect_job_unknown", Status: StatusUnknown, Detail: "effect/job state unavailable", CheckedAt: now}}
	}
	if count > 0 {
		return []Finding{{Component: ComponentEffectJob, Code: "effect_job_stuck", Status: StatusDegraded, Detail: fmt.Sprintf("%d effect/job records stuck", count), CheckedAt: now}}
	}
	return []Finding{{Component: ComponentEffectJob, Code: "ok", Status: StatusUp, Detail: "no stuck effects or jobs", CheckedAt: now}}
}
