package plugin

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/health"
)

func TestDriftMatrix(t *testing.T) {
	installed := []Record{
		{ID: "weather", State: StateActive, Version: "1.0.0", ApprovedVersion: "1.0.0"},
		{ID: "mail", State: StateQuarantined, Version: "2.0.0"},
		{ID: "old", State: StateActive, Version: "1.0.0", ApprovedVersion: "1.0.0"},
		{ID: "pending", State: StateActive, Version: "1.1.0", ApprovedVersion: "1.0.0"},
	}
	desired := &config.Plugins{Desired: []config.PluginDesired{
		{ID: "weather", Version: "1.0.0", Enabled: true},
		{ID: "mail", Version: "2.0.0", Enabled: true},
		{ID: "gone", Version: "1.0.0", Enabled: true},
		{ID: "pending", Version: "1.1.0", Enabled: true},
		{ID: "old", Version: "1.0.0", Enabled: false},
	}}
	got := Drift(desired, installed)
	want := []DriftItem{
		{ID: "gone", Reason: "declared but not installed"},
		{ID: "mail", Reason: "declared enabled but not approved and active"},
		{ID: "old", Reason: "declared disabled but active"},
		{ID: "pending", Reason: "declared enabled but not approved and active"},
	}
	if len(got) != len(want) {
		t.Fatalf("drift = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("drift[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestDriftVersionAndUndeclared(t *testing.T) {
	installed := []Record{{ID: "weather", State: StateActive, Version: "1.0.0", ApprovedVersion: "1.0.0"}, {ID: "extra", State: StateDisabled, Version: "1.0.0"}}
	desired := &config.Plugins{Desired: []config.PluginDesired{{ID: "weather", Version: "1.2.0", Enabled: true}}}
	got := Drift(desired, installed)
	if len(got) != 2 || got[0].ID != "extra" || got[0].Reason != "installed but not declared" || got[1].ID != "weather" || !strings.Contains(got[1].Reason, "differs") {
		t.Fatalf("drift = %v", got)
	}
	if Drift(nil, installed) != nil {
		t.Fatal("drift reported without a declared list")
	}
	if got := Drift(&config.Plugins{Desired: []config.PluginDesired{{ID: "weather", Version: "1.0.0", Enabled: true}}}, installed[:1]); len(got) != 0 {
		t.Fatalf("matching state drifts: %v", got)
	}
}

func newCheckerForTest(t *testing.T, env *testEnv, opts *CheckerOptions) *Checker {
	t.Helper()
	opts.Registry = env.registry
	opts.Now = func() time.Time { return env.clock }
	checker, err := NewChecker(opts)
	if err != nil {
		t.Fatalf("NewChecker: %v", err)
	}
	return checker
}

func TestCheckerReportsHeldToolDriftAndDesiredDrift(t *testing.T) {
	env := newTestEnv(t)
	env.approve("weather", env.install(baseManifest, serverFiles()))
	checker := newCheckerForTest(t, env, &CheckerOptions{
		Held:    []Held{{ID: "mail", Code: ErrTampered, Detail: "bundle changed"}, {ID: "calendar", Code: ErrSurfacePending, Detail: "capability network is waiting for review"}},
		Desired: &config.Plugins{Desired: []config.PluginDesired{{ID: "weather", Version: "1.0.0", Enabled: false}}},
		Runtime: func(context.Context) []RuntimeStatus {
			return []RuntimeStatus{
				{ID: "weather", Running: true, Drift: []string{"export_all", "bad name!"}},
				{ID: "quiet", Running: true},
			}
		},
	})
	findings := checker.Check(t.Context())
	byID := map[string]health.Finding{}
	for _, finding := range findings {
		byID[finding.ID] = finding
	}
	tampered := byID["plugin_tampered:mail"]
	if tampered.Status != health.StatusDown || tampered.Severity != health.SeverityCritical || tampered.Component != HealthCheckID {
		t.Fatalf("tampered finding = %+v", tampered)
	}
	if pending := byID["plugin_surface_pending:calendar"]; pending.Status != health.StatusDegraded {
		t.Fatalf("pending finding = %+v", pending)
	}
	drift := byID["plugin_tool_drift:weather"]
	if drift.Code != string(ErrToolDrift) || !strings.Contains(drift.Detail, "weather.export_all") || strings.Contains(drift.Detail, "bad name!") {
		t.Fatalf("tool drift finding = %+v", drift)
	}
	if desired := byID["plugin_drift:weather"]; desired.Code != string(ErrDrift) || !strings.Contains(desired.Detail, "declared disabled but active") {
		t.Fatalf("desired drift finding = %+v", desired)
	}
	if _, quiet := byID["plugin_tool_drift:quiet"]; quiet || len(findings) != 4 {
		t.Fatalf("findings = %+v", findings)
	}
}

func TestCheckerReportsUpWhenEverythingMatches(t *testing.T) {
	env := newTestEnv(t)
	env.approve("weather", env.install(baseManifest, serverFiles()))
	checker := newCheckerForTest(t, env, &CheckerOptions{
		Desired: &config.Plugins{Desired: []config.PluginDesired{{ID: "weather", Version: "1.0.0", Enabled: true}}},
		Runtime: func(context.Context) []RuntimeStatus { return []RuntimeStatus{{ID: "weather", Running: true}} },
	})
	findings := checker.Check(t.Context())
	if len(findings) != 1 || findings[0].Status != health.StatusUp || findings[0].Code != "ok" {
		t.Fatalf("findings = %+v", findings)
	}
}

func TestNewCheckerRequiresARegistry(t *testing.T) {
	if _, err := NewChecker(&CheckerOptions{}); !hasCode(err, ErrInvalidArgument) {
		t.Fatalf("NewChecker error = %v", err)
	}
	if _, err := NewChecker(nil); !hasCode(err, ErrInvalidArgument) {
		t.Fatalf("NewChecker(nil) error = %v", err)
	}
}

func TestCheckerReportsABackendThatCannotRun(t *testing.T) {
	env := newTestEnv(t)
	checker := newCheckerForTest(t, env, &CheckerOptions{Runtime: func(context.Context) []RuntimeStatus {
		return []RuntimeStatus{{ID: "mailer", Backend: ErrBackendUnavailable}, {ID: "weather", Running: true}}
	}})
	findings := checker.Check(t.Context())
	if len(findings) != 1 || findings[0].Code != string(ErrBackendUnavailable) || findings[0].Scope != "mailer" || findings[0].Status != health.StatusDegraded || findings[0].Remediation != RemediationReview {
		t.Fatalf("findings = %+v", findings)
	}
}
