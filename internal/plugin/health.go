package plugin

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/health"
	"github.com/anggasct/aura/internal/tools"
)

const (
	HealthCheckID         = "plugin"
	RemediationReview     = "review-plugin"
	RemediationLogin      = "login-plugin"
	maxDriftNamesReported = 5
)

type RuntimeStatus struct {
	ID      string
	Running bool
	Drift   []string
	Auth    ErrorCode
	Backend ErrorCode
}

type DriftItem struct {
	ID     string
	Reason string
}

func Drift(desired *config.Plugins, installed []Record) []DriftItem {
	if desired == nil {
		return nil
	}
	byID := make(map[string]*Record, len(installed))
	for i := range installed {
		byID[installed[i].ID] = &installed[i]
	}
	var items []DriftItem
	declared := make(map[string]bool, len(desired.Desired))
	for i := range desired.Desired {
		want := &desired.Desired[i]
		declared[want.ID] = true
		have, present := byID[want.ID]
		switch {
		case !present:
			items = append(items, DriftItem{ID: want.ID, Reason: "declared but not installed"})
		case have.Version != want.Version:
			items = append(items, DriftItem{ID: want.ID, Reason: "installed version " + have.Version + " differs from declared " + want.Version})
		case want.Enabled && (have.State != StateActive || have.ApprovedVersion != have.Version):
			items = append(items, DriftItem{ID: want.ID, Reason: "declared enabled but not approved and active"})
		case !want.Enabled && have.State == StateActive:
			items = append(items, DriftItem{ID: want.ID, Reason: "declared disabled but active"})
		}
	}
	for _, record := range installed {
		if !declared[record.ID] {
			items = append(items, DriftItem{ID: record.ID, Reason: "installed but not declared"})
		}
	}
	slices.SortFunc(items, func(a, b DriftItem) int { return strings.Compare(a.ID, b.ID) })
	return items
}

type CheckerOptions struct {
	Registry Registry
	Desired  *config.Plugins
	Held     []Held
	Runtime  func(ctx context.Context) []RuntimeStatus
	Now      func() time.Time
}

type Checker struct {
	registry Registry
	desired  *config.Plugins
	held     []Held
	runtime  func(ctx context.Context) []RuntimeStatus
	now      func() time.Time
}

func NewChecker(opts *CheckerOptions) (*Checker, error) {
	if opts == nil || opts.Registry == nil {
		return nil, Errorf(ErrInvalidArgument, "a registry is required")
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Checker{registry: opts.Registry, desired: opts.Desired, held: slices.Clone(opts.Held), runtime: opts.Runtime, now: now}, nil
}

func remediationFor(code ErrorCode) string {
	if code == ErrAuthRequired || code == ErrAuthFailed {
		return RemediationLogin
	}
	return RemediationReview
}

func (c *Checker) finding(code ErrorCode, scope string, status health.Status, detail string, at time.Time) health.Finding {
	return health.Finding{
		ID:          string(code) + ":" + scope,
		Component:   HealthCheckID,
		Scope:       scope,
		Code:        string(code),
		Status:      status,
		Severity:    health.SeverityFor(status),
		Detail:      detail,
		Remediation: remediationFor(code),
		CheckedAt:   at,
		FirstSeen:   at,
		LastSeen:    at,
	}
}

func (c *Checker) Check(ctx context.Context) []health.Finding {
	if ctx == nil {
		return nil
	}
	at := c.now().UTC()
	var findings []health.Finding
	for _, held := range c.held {
		status := health.StatusDegraded
		if held.Code == ErrTampered {
			status = health.StatusDown
		}
		findings = append(findings, c.finding(held.Code, held.ID, status, "plugin "+held.ID+" is kept off: "+held.Detail, at))
	}
	if c.runtime != nil {
		for _, status := range c.runtime(ctx) {
			if status.Auth != "" {
				findings = append(findings, c.finding(status.Auth, status.ID, health.StatusDegraded,
					"plugin "+status.ID+" has no usable authorization; run aura plugin login and restart", at))
			}
			if status.Backend != "" {
				findings = append(findings, c.finding(status.Backend, status.ID, health.StatusDegraded,
					"plugin "+status.ID+" is kept off: its backend cannot run on this host", at))
			}
			if len(status.Drift) == 0 {
				continue
			}
			findings = append(findings, c.finding(ErrToolDrift, status.ID, health.StatusDegraded,
				"plugin "+status.ID+" offers tools it was not approved for: "+reportedToolNames(status.ID, status.Drift), at))
		}
	}
	if c.desired != nil {
		records, err := c.registry.List(ctx)
		if err != nil {
			findings = append(findings, c.finding(ErrStorage, "registry", health.StatusUnknown, "the plugin registry cannot be read", at))
		} else {
			for _, item := range Drift(c.desired, records) {
				findings = append(findings, c.finding(ErrDrift, item.ID, health.StatusDegraded, "plugin "+item.ID+": "+item.Reason, at))
			}
		}
	}
	if len(findings) == 0 {
		findings = append(findings, health.Finding{
			Component: HealthCheckID,
			Code:      "ok",
			Status:    health.StatusUp,
			Detail:    "all plugins match their approvals",
			CheckedAt: at,
		})
	}
	return findings
}

func reportedToolNames(id string, names []string) string {
	shown := make([]string, 0, maxDriftNamesReported)
	for _, name := range names {
		if len(shown) == maxDriftNamesReported {
			shown = append(shown, "...")
			break
		}
		if toolNamePattern.MatchString(name) {
			shown = append(shown, tools.PluginToolID(id, name))
		} else {
			shown = append(shown, "(unrepresentable name)")
		}
	}
	return strings.Join(shown, ", ")
}
