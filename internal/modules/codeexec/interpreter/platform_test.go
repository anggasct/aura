package interpreter

import (
	"testing"

	"github.com/anggasct/aura/internal/platform/capability"
)

func TestSupportedPlatform(t *testing.T) {
	cases := []struct {
		goos, goarch string
		want         bool
	}{
		{"linux", "amd64", true},
		{"linux", "arm64", false},
		{"darwin", "amd64", false},
		{"darwin", "arm64", false},
		{"windows", "amd64", false},
		{"", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.goos+"/"+tc.goarch, func(t *testing.T) {
			if got := Supported(tc.goos, tc.goarch); got != tc.want {
				t.Errorf("Supported = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDependenciesRequireContainmentAndPlatform(t *testing.T) {
	contained := capability.Dependencies{capability.DependencyProcessContainment: true}
	cases := []struct {
		name         string
		goos, goarch string
		base         capability.Dependencies
		want         bool
	}{
		{"linux amd64 contained", "linux", "amd64", contained, true},
		{"linux amd64 without containment", "linux", "amd64", capability.Dependencies{}, false},
		{"linux amd64 with nil base", "linux", "amd64", nil, false},
		{"linux arm64 contained", "linux", "arm64", contained, false},
		{"darwin arm64 contained", "darwin", "arm64", contained, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Dependencies(tc.goos, tc.goarch, tc.base)
			if got[capability.DependencyInterpreterRuntime] != tc.want {
				t.Errorf("interpreter-runtime = %v, want %v", got[capability.DependencyInterpreterRuntime], tc.want)
			}
			if got[capability.DependencyProcessContainment] != tc.base[capability.DependencyProcessContainment] {
				t.Errorf("process-containment changed: %v", got)
			}
		})
	}
	if _, mutated := contained[capability.DependencyInterpreterRuntime]; mutated {
		t.Error("Dependencies mutated its input")
	}
}

func TestCodeInterpreterCapabilityAvailability(t *testing.T) {
	registry, err := capability.BuiltinRegistry()
	if err != nil {
		t.Fatal(err)
	}
	contained := capability.Dependencies{capability.DependencyProcessContainment: true}
	cases := []struct {
		name          string
		profile       string
		goos, goarch  string
		wantAvailable bool
		wantMissing   string
	}{
		{"exec-linux on linux amd64", "exec-linux", "linux", "amd64", true, ""},
		{"native-linux on linux amd64", "native-linux", "linux", "amd64", true, ""},
		{"exec-linux on linux arm64", "exec-linux", "linux", "arm64", false, capability.DependencyInterpreterRuntime},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			build, err := capability.NewBuild(tc.profile, []string{"code-interpreter"}, tc.goos)
			if err != nil {
				t.Fatal(err)
			}
			report, _ := registry.Resolve(build, []string{"code-interpreter"}, Dependencies(tc.goos, tc.goarch, contained))
			status, ok := report.Status("code-interpreter")
			if !ok || status.Available != tc.wantAvailable || status.MissingDependency != tc.wantMissing {
				t.Errorf("status = %+v, %v; want available=%v missing=%q", status, ok, tc.wantAvailable, tc.wantMissing)
			}
		})
	}
	t.Run("core profile excludes the capability", func(t *testing.T) {
		build, err := capability.NewBuild("core", []string{"code-interpreter"}, "linux")
		if err != nil {
			t.Fatal(err)
		}
		report, resolveErr := registry.Resolve(build, nil, Dependencies("linux", "amd64", contained))
		if code, ok := capability.CodeOf(resolveErr); !ok || code != capability.ErrorCodeCapabilityUnavailable {
			t.Errorf("Resolve error = %v, want capability_unavailable", resolveErr)
		}
		if status, _ := report.Status("code-interpreter"); status.Available {
			t.Errorf("code-interpreter is available in the core profile: %+v", status)
		}
	})
	t.Run("a core build compiles without the capability", func(t *testing.T) {
		build, err := capability.NewBuild("core", nil, "linux")
		if err != nil {
			t.Fatal(err)
		}
		report, resolveErr := registry.Resolve(build, nil, Dependencies("linux", "amd64", contained))
		if resolveErr != nil {
			t.Fatalf("Resolve: %v", resolveErr)
		}
		if status, _ := report.Status("code-interpreter"); status.Compiled || status.Available {
			t.Errorf("code-interpreter status in core = %+v, want absent", status)
		}
	})
}
