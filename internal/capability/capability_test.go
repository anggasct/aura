package capability

import (
	"slices"
	"strings"
	"testing"
)

func requireErrorCode(t *testing.T, err error, want ErrorCode) {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil, want %q", want)
	}
	got, ok := CodeOf(err)
	if !ok || got != want {
		t.Fatalf("CodeOf(%v) = %q, %v; want %q, true", err, got, ok, want)
	}
}

func TestBuildMetadataIsSortedAndImmutable(t *testing.T) {
	build, err := NewBuild(string(ProfileCore), []string{"zeta", "alpha"}, "linux")
	if err != nil {
		t.Fatalf("NewBuild: %v", err)
	}
	got := build.CompiledCapabilities()
	if !slices.Equal(got, []string{"alpha", "zeta"}) {
		t.Fatalf("CompiledCapabilities = %v", got)
	}
	got[0] = "mutated"
	if build.CompiledCapabilities()[0] != "alpha" {
		t.Fatal("CompiledCapabilities exposed mutable build metadata")
	}
}

func TestSupportedBuildProfiles(t *testing.T) {
	for _, profile := range SupportedProfiles() {
		goos := "linux"
		if profile == ProfileCore {
			goos = "darwin"
		}
		if _, err := NewBuild(string(profile), nil, goos); err != nil {
			t.Errorf("NewBuild(%q): %v", profile, err)
		}
	}

	_, err := NewBuild("unknown", nil, "linux")
	requireErrorCode(t, err, ErrorCodeProfileInvalid)

	_, err = NewBuild(string(ProfileExecLinux), nil, "darwin")
	requireErrorCode(t, err, ErrorCodeCapabilityUnavailable)
}

func TestBuildRejectsDuplicateCompiledCapability(t *testing.T) {
	_, err := NewBuild(string(ProfileCore), []string{"sample", "sample"}, "linux")
	requireErrorCode(t, err, ErrorCodeProfileInvalid)
}

func TestBuildRejectsInvalidCompiledCapabilityName(t *testing.T) {
	_, err := NewBuild(string(ProfileCore), []string{"invalid_name"}, "linux")
	requireErrorCode(t, err, ErrorCodeProfileInvalid)
}

func TestBuildReportsEveryInvalidCapability(t *testing.T) {
	_, err := NewBuild(string(ProfileCore), []string{"invalid_name", "sample", "sample"}, "linux")
	requireErrorCode(t, err, ErrorCodeProfileInvalid)
	for _, name := range []string{"invalid_name", "sample"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("aggregate error omits %q: %v", name, err)
		}
	}
}

func TestRegistryReportsAvailabilityAndEnablement(t *testing.T) {
	registry, err := NewRegistry(
		Definition{
			Name:      "sample-effect",
			Effectful: true,
			Profiles:  []Profile{ProfileExecLinux, ProfileBrowserLinux},
		},
		Definition{
			Name:       "sample-runtime",
			Effectful:  true,
			Profiles:   []Profile{ProfileExecLinux, ProfileBrowserLinux},
			Dependency: "sample-host-runtime",
		},
	)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	build, err := NewBuild(
		string(ProfileExecLinux),
		[]string{"sample-runtime", "sample-effect"},
		"linux",
	)
	if err != nil {
		t.Fatalf("NewBuild: %v", err)
	}

	report, err := registry.Resolve(build, []string{"sample-effect"}, nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	effect, ok := report.Status("sample-effect")
	if !ok || !effect.Compiled || !effect.Available || !effect.Enabled || effect.State != StateEnabled {
		t.Fatalf("sample-effect status = %+v, %v", effect, ok)
	}
	runtimeStatus, ok := report.Status("sample-runtime")
	if !ok || !runtimeStatus.Compiled || runtimeStatus.Available || runtimeStatus.Enabled ||
		runtimeStatus.State != StateMissing || runtimeStatus.MissingDependency != "sample-host-runtime" {
		t.Fatalf("sample-runtime status = %+v, %v", runtimeStatus, ok)
	}

	statuses := report.Statuses()
	statuses[0].Name = "mutated"
	if current, _ := report.Status("sample-effect"); current.Name != "sample-effect" {
		t.Fatal("Statuses exposed mutable report state")
	}
}

func TestRegistryRejectsUnavailableCapabilities(t *testing.T) {
	registry, err := NewRegistry(Definition{
		Name:     "sample",
		Profiles: []Profile{ProfileCore},
	})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	build, err := NewBuild(string(ProfileCore), nil, "linux")
	if err != nil {
		t.Fatalf("NewBuild: %v", err)
	}
	_, err = registry.Resolve(build, []string{"sample"}, nil)
	requireErrorCode(t, err, ErrorCodeCapabilityUnavailable)

	_, err = registry.Resolve(build, []string{"unknown-test"}, nil)
	requireErrorCode(t, err, ErrorCodeCapabilityUnknown)
}

func TestRegistryIgnoresDefinitionsNotLoadedByThisProcess(t *testing.T) {
	registry, err := NewRegistry(Definition{
		Name:     "future-capability",
		Profiles: []Profile{ProfileCore},
	})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	build, err := NewBuild(string(ProfileCore), []string{"future-capability", "unregistered-build-metadata"}, "linux")
	if err != nil {
		t.Fatalf("NewBuild: %v", err)
	}
	if _, err := registry.Resolve(build, nil, nil); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
}

func TestRegistryRejectsMissingEnabledDependency(t *testing.T) {
	registry, err := NewRegistry(Definition{
		Name:       "sample-runtime",
		Profiles:   []Profile{ProfileExecLinux},
		Dependency: "sample-host-runtime",
	})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	build, err := NewBuild(string(ProfileExecLinux), []string{"sample-runtime"}, "linux")
	if err != nil {
		t.Fatalf("NewBuild: %v", err)
	}
	_, err = registry.Resolve(build, []string{"sample-runtime"}, nil)
	requireErrorCode(t, err, ErrorCodeDependencyMissing)
}

func TestRegistryRejectsEffectfulCapabilityOffLinux(t *testing.T) {
	registry, err := NewRegistry(Definition{
		Name:      "sample-effect",
		Effectful: true,
		Profiles:  []Profile{ProfileExecLinux},
	})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	malformedBuild := Build{
		profile:      ProfileExecLinux,
		capabilities: []string{"sample-effect"},
		goos:         "darwin",
	}
	_, err = registry.Resolve(malformedBuild, nil, nil)
	requireErrorCode(t, err, ErrorCodeCapabilityUnavailable)
}

func TestRegistryRejectsEffectfulCoreDefinition(t *testing.T) {
	_, err := NewRegistry(Definition{
		Name:      "sample-effect",
		Effectful: true,
		Profiles:  []Profile{ProfileCore},
	})
	if err == nil {
		t.Fatal("NewRegistry accepted an effectful core capability")
	}
}

func TestRegistryCopiesDefinitionProfiles(t *testing.T) {
	profiles := []Profile{ProfileCore}
	registry, err := NewRegistry(Definition{Name: "sample", Profiles: profiles})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	profiles[0] = ProfileExecLinux
	build, err := NewBuild(string(ProfileCore), []string{"sample"}, "linux")
	if err != nil {
		t.Fatalf("NewBuild: %v", err)
	}
	if _, err := registry.Resolve(build, nil, nil); err != nil {
		t.Fatalf("Resolve after caller mutation: %v", err)
	}
}
