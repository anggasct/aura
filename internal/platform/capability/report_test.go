package capability_test

import (
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/anggasct/aura/internal/platform/capability"
	"github.com/anggasct/aura/internal/platform/capability/capabilitytest"
)

func sampleReport(t *testing.T) capability.Report {
	t.Helper()
	registry, err := capability.NewRegistry(
		capability.Definition{Name: "sample-read", Profiles: []capability.Profile{capability.ProfileCore, capability.ProfileExecLinux}},
		capability.Definition{Name: "sample-search", Profiles: []capability.Profile{capability.ProfileCore, capability.ProfileExecLinux}},
		capability.Definition{Name: "sample-effect", Effectful: true, Profiles: []capability.Profile{capability.ProfileExecLinux}, Dependency: "sample-host"},
	)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	build, err := capability.NewBuild(string(capability.ProfileExecLinux), []string{"sample-read", "sample-search", "sample-effect"}, "linux")
	if err != nil {
		t.Fatalf("NewBuild: %v", err)
	}
	report, _ := registry.Resolve(build, []string{"sample-read", "sample-effect"}, nil)
	return report
}

func TestReportActiveListsAvailableAndEnabledNames(t *testing.T) {
	cases := []struct {
		name    string
		enabled []string
		want    []string
	}{
		{name: "nothing enabled", enabled: nil, want: []string{}},
		{name: "one enabled", enabled: []string{"workspace-read"}, want: []string{"workspace-read"}},
		{name: "several enabled", enabled: []string{"public-web", "exec-linux"}, want: []string{"exec-linux", "public-web"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := capabilitytest.Report(t, tc.enabled...).Active()
			if !slices.Equal(got, tc.want) {
				t.Fatalf("Active() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestReportActiveExcludesEnabledNamesWhoseDependencyIsMissing(t *testing.T) {
	report := sampleReport(t)
	if got, want := report.Active(), []string{"sample-read"}; !slices.Equal(got, want) {
		t.Fatalf("Active() = %v, want %v", got, want)
	}
}

func TestReportActiveForTheZeroReportIsEmpty(t *testing.T) {
	var report capability.Report
	if got := report.Active(); len(got) != 0 {
		t.Fatalf("Active() = %v, want none", got)
	}
}

func TestReportRequire(t *testing.T) {
	report := sampleReport(t)
	cases := []struct {
		name  string
		names []string
		want  []capability.ErrorCode
	}{
		{name: "no names", names: nil},
		{name: "available names", names: []string{"sample-read", "sample-search"}},
		{name: "name outside the registry", names: []string{"fs.read"}, want: []capability.ErrorCode{capability.ErrorCodeCapabilityUnknown}},
		{name: "name whose dependency is missing", names: []string{"sample-effect"}, want: []capability.ErrorCode{capability.ErrorCodeCapabilityUnavailable}},
		{name: "unknown and unavailable", names: []string{"fs.read", "sample-effect"}, want: []capability.ErrorCode{capability.ErrorCodeCapabilityUnknown, capability.ErrorCodeCapabilityUnavailable}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := report.Require(tc.names)
			if len(tc.want) == 0 {
				if err != nil {
					t.Fatalf("Require(%v) = %v, want nil", tc.names, err)
				}
				return
			}
			var joined interface{ Unwrap() []error }
			parts := []error{err}
			if errors.As(err, &joined) {
				parts = joined.Unwrap()
			}
			var got []capability.ErrorCode
			for _, part := range parts {
				code, ok := capability.CodeOf(part)
				if !ok {
					t.Fatalf("part %v carries no capability code", part)
				}
				got = append(got, code)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("Require(%v) codes = %v, want %v", tc.names, got, tc.want)
			}
		})
	}
}

func TestRequireOnTheZeroReportRejectsEveryName(t *testing.T) {
	var report capability.Report
	err := report.Require([]string{"workspace-read"})
	if code, ok := capability.CodeOf(err); !ok || code != capability.ErrorCodeCapabilityUnknown {
		t.Fatalf("CodeOf(%v) = %q, %v; want capability_unknown", err, code, ok)
	}
}

func TestErrorClassifiers(t *testing.T) {
	missing := fmt.Errorf("dependency_missing: %w", &capability.Error{Code: capability.ErrorCodeDependencyMissing, Capability: "exec-linux"})
	unavailable := &capability.Error{Code: capability.ErrorCodeCapabilityUnavailable, Capability: "exec-linux"}
	unknown := &capability.Error{Code: capability.ErrorCodeCapabilityUnknown, Capability: "nope"}
	cases := []struct {
		name          string
		err           error
		wantHealth    bool
		wantMissingOn bool
	}{
		{name: "nil", err: nil},
		{name: "dependency missing", err: &capability.Error{Code: capability.ErrorCodeDependencyMissing}, wantHealth: true, wantMissingOn: true},
		{name: "wrapped dependency missing", err: missing, wantHealth: true, wantMissingOn: true},
		{name: "capability unavailable", err: unavailable, wantHealth: true},
		{name: "unknown capability", err: unknown},
		{name: "joined dependency missing only", err: errors.Join(missing, missing), wantHealth: true, wantMissingOn: true},
		{name: "joined missing and unavailable", err: errors.Join(missing, unavailable), wantHealth: true},
		{name: "joined missing and unknown", err: errors.Join(missing, unknown)},
		{name: "joined unavailable first and missing second", err: errors.Join(unavailable, missing), wantHealth: true},
		{name: "foreign error", err: errors.New("boom")},
		{name: "empty join", err: errors.Join()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := capability.IsHealthState(tc.err); got != tc.wantHealth {
				t.Errorf("IsHealthState = %v, want %v", got, tc.wantHealth)
			}
			if got := capability.IsMissingDependency(tc.err); got != tc.wantMissingOn {
				t.Errorf("IsMissingDependency = %v, want %v", got, tc.wantMissingOn)
			}
		})
	}
}
