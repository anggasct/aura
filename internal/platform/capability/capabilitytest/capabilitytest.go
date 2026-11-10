package capabilitytest

import (
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/platform/capability"
)

func Report(tb testing.TB, enabled ...string) capability.Report {
	tb.Helper()
	registry, names := registered(tb)
	dependencies := capability.Dependencies{
		capability.DependencyProcessContainment: true,
		capability.DependencyInterpreterRuntime: true,
	}
	report, err := registry.Resolve(build(tb, names), enabled, dependencies)
	if err != nil {
		tb.Fatalf("resolve capabilities %s: %v", strings.Join(enabled, ","), err)
	}
	return report
}

func All(tb testing.TB) capability.Report {
	tb.Helper()
	_, names := registered(tb)
	return Report(tb, names...)
}

func Host(tb testing.TB) []string {
	tb.Helper()
	return All(tb).Active()
}

func registered(tb testing.TB) (registry capability.Registry, names []string) {
	tb.Helper()
	registry, err := capability.BuiltinRegistry()
	if err != nil {
		tb.Fatalf("builtin capability registry: %v", err)
	}
	survey, err := registry.Resolve(build(tb, nil), nil, nil)
	if err != nil {
		tb.Fatalf("survey capabilities: %v", err)
	}
	statuses := survey.Statuses()
	names = make([]string, 0, len(statuses))
	for _, status := range statuses {
		names = append(names, status.Name)
	}
	return registry, names
}

func build(tb testing.TB, compiled []string) capability.Build {
	tb.Helper()
	built, err := capability.NewBuild(string(capability.ProfileExecLinux), compiled, "linux")
	if err != nil {
		tb.Fatalf("capability build: %v", err)
	}
	return built
}
