package health

import (
	"context"
	"fmt"
	"time"
)

type Status string

const (
	StatusUp       Status = "up"
	StatusDegraded Status = "degraded"
	StatusUnknown  Status = "unknown"
	StatusDown     Status = "down"
)

const (
	ComponentMigration  = "migration"
	ComponentCapability = "capability"
	ComponentSandbox    = "sandbox"
	ComponentBackup     = "backup"
	ComponentProvider   = "provider"
	ComponentEffectJob  = "effect_job"
	ComponentDurable    = "durable"
)

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

type Checker interface {
	Check(ctx context.Context) []Finding
}

type Evaluator struct {
	checkers []Checker
}

func NewEvaluator(checkers ...Checker) *Evaluator {
	return &Evaluator{checkers: checkers}
}

func (e *Evaluator) Evaluate(ctx context.Context) []Finding {
	findings := make([]Finding, 0, len(e.checkers))
	for _, c := range e.checkers {
		findings = append(findings, c.Check(ctx)...)
	}
	return findings
}

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

type CapabilityStatus struct {
	Name              string
	Compiled          bool
	Available         bool
	Enabled           bool
	MissingDependency string
	UnavailableReason string
}

type CapabilityChecker struct {
	Statuses func(ctx context.Context) []CapabilityStatus
}

func (c CapabilityChecker) Check(ctx context.Context) []Finding {
	now := time.Now().UTC()
	statuses := c.Statuses(ctx)
	findings := make([]Finding, 0, len(statuses))
	for _, s := range statuses {
		switch {
		case !s.Compiled && s.Enabled:
			findings = append(findings, Finding{
				Component: ComponentCapability + "/" + s.Name,
				Code:      "capability_not_compiled",
				Status:    StatusDown,
				Detail:    "enabled in configuration but not compiled into this binary",
				CheckedAt: now,
			})
		case s.Enabled && !s.Available:
			detail := "capability unavailable"
			switch {
			case s.MissingDependency != "":
				detail = "missing dependency: " + s.MissingDependency
			case s.UnavailableReason != "":
				detail = s.UnavailableReason
			}
			findings = append(findings, Finding{
				Component: ComponentCapability + "/" + s.Name,
				Code:      "capability_unavailable",
				Status:    StatusDown,
				Detail:    detail,
				CheckedAt: now,
			})
		}
	}
	if len(findings) == 0 {
		return []Finding{{Component: ComponentCapability, Code: "ok", Status: StatusUp, Detail: "all capabilities consistent with this profile", CheckedAt: now}}
	}
	return findings
}

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

type StorageIntakeState string

const (
	StorageIntakeOK          StorageIntakeState = "ok"
	StorageIntakeUnknown     StorageIntakeState = "unknown"
	StorageIntakeUnreachable StorageIntakeState = "unreachable"
	StorageIntakeReadOnly    StorageIntakeState = "read_only"
	StorageIntakeFull        StorageIntakeState = "full"
)

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

type DurableState string

const (
	DurableUp          DurableState = "up"
	DurableDisabled    DurableState = "disabled"
	DurableUnreachable DurableState = "unreachable"
	DurableUnknown     DurableState = "unknown"
)

type DurableChecker struct {
	State func(ctx context.Context) (DurableState, string)
}

func (c DurableChecker) Check(ctx context.Context) []Finding {
	now := time.Now().UTC()
	if c.State == nil {
		return []Finding{{Component: ComponentDurable, Code: "durable_unknown", Status: StatusUnknown, Detail: "durable state is not wired", CheckedAt: now}}
	}
	state, detail := c.State(ctx)
	switch state {
	case DurableUp:
		return []Finding{{Component: ComponentDurable, Code: "ok", Status: StatusUp, Detail: detail, CheckedAt: now}}
	case DurableDisabled:
		return []Finding{{Component: ComponentDurable, Code: "disabled", Status: StatusUp, Detail: detail, CheckedAt: now}}
	case DurableUnreachable:
		return []Finding{{Component: ComponentDurable, Code: "durable_unreachable", Status: StatusDown, Detail: detail, CheckedAt: now}}
	default:
		return []Finding{{Component: ComponentDurable, Code: "durable_unknown", Status: StatusUnknown, Detail: detail, CheckedAt: now}}
	}
}
