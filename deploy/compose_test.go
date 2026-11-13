package deploy

import (
	"maps"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/platform/capability"
)

type composeFile struct {
	Services map[string]composeService `yaml:"services"`
	Networks map[string]any            `yaml:"networks"`
}

type composeService struct {
	Image       string                       `yaml:"image"`
	User        string                       `yaml:"user"`
	ReadOnly    bool                         `yaml:"read_only"`
	Command     []string                     `yaml:"command"`
	Ports       []any                        `yaml:"ports"`
	Networks    []string                     `yaml:"networks"`
	NetworkMode string                       `yaml:"network_mode"`
	CapDrop     []string                     `yaml:"cap_drop"`
	SecurityOpt []string                     `yaml:"security_opt"`
	DependsOn   map[string]map[string]string `yaml:"depends_on"`
	Volumes     []string                     `yaml:"volumes"`

	StopGracePeriod string `yaml:"stop_grace_period"`
}

func loadCompose(t *testing.T, path string) composeFile {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var file composeFile
	if err := yaml.Unmarshal(raw, &file); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return file
}

func service(t *testing.T, file *composeFile, name string) composeService {
	t.Helper()
	svc, ok := file.Services[name]
	if !ok {
		t.Fatalf("compose.yaml has no %q service", name)
	}
	return svc
}

func TestComposeRunsOnlyAuraAndPublishesNoPort(t *testing.T) {
	file := loadCompose(t, "compose.yaml")
	services := slices.Sorted(maps.Keys(file.Services))
	if !slices.Equal(services, []string{"aura", "aura-upgrade"}) {
		t.Fatalf("services = %v, want only aura and aura-upgrade", services)
	}
	for name, svc := range file.Services {
		if len(svc.Ports) != 0 {
			t.Fatalf("%s publishes ports %v; the canonical topology publishes none", name, svc.Ports)
		}
	}
	if _, declared := file.Networks["internal"]; declared {
		t.Fatal("compose.yaml declares an internal network; aura runs on egress only")
	}
}

func TestAuraContainersAreLockedDown(t *testing.T) {
	file := loadCompose(t, "compose.yaml")
	for _, name := range []string{"aura", "aura-upgrade"} {
		svc := service(t, &file, name)
		if svc.User != "65532:65532" || !svc.ReadOnly {
			t.Fatalf("%s user = %q read_only = %v, want 65532:65532 and a read-only root filesystem", name, svc.User, svc.ReadOnly)
		}
		if !slices.Equal(svc.CapDrop, []string{"ALL"}) || !slices.Contains(svc.SecurityOpt, "no-new-privileges:true") {
			t.Fatalf("%s cap_drop = %v security_opt = %v", name, svc.CapDrop, svc.SecurityOpt)
		}
		if !strings.Contains(svc.Image, "ghcr.io/anggasct/aura:${AURA_VERSION") {
			t.Fatalf("%s image %q must default to the release tag", name, svc.Image)
		}
	}
	upgrade := service(t, &file, "aura-upgrade")
	if !slices.Equal(upgrade.Command, []string{"storage", "upgrade"}) || upgrade.NetworkMode != "none" {
		t.Fatalf("aura-upgrade command = %v network_mode = %q", upgrade.Command, upgrade.NetworkMode)
	}
}

func TestAuraStartsOnlyAfterTheUpgradeJob(t *testing.T) {
	file := loadCompose(t, "compose.yaml")
	aura := service(t, &file, "aura")
	want := map[string]map[string]string{"aura-upgrade": {"condition": "service_completed_successfully"}}
	if !maps.EqualFunc(aura.DependsOn, want, maps.Equal) {
		t.Fatalf("aura depends_on = %v, want %v", aura.DependsOn, want)
	}
	if !slices.Equal(aura.Networks, []string{"egress"}) {
		t.Fatalf("aura networks = %v, want only egress", aura.Networks)
	}
}

func TestAuraStopGracePeriodOutlastsTheShutdownTimeout(t *testing.T) {
	t.Setenv("AURA_CONFIG", "")
	result, err := config.Load("aura.yaml")
	if err != nil {
		t.Fatalf("deploy/aura.yaml does not load: %v", err)
	}
	file := loadCompose(t, "compose.yaml")
	aura := service(t, &file, "aura")
	grace, err := time.ParseDuration(aura.StopGracePeriod)
	if err != nil {
		t.Fatalf("aura stop_grace_period %q is not a duration: %v", aura.StopGracePeriod, err)
	}
	shutdown := time.Duration(result.Config.Runtime.ShutdownTimeout)
	if grace < shutdown+10*time.Second {
		t.Fatalf("aura stop_grace_period = %s, want at least the shutdown timeout %s plus 10s", grace, shutdown)
	}
}

func TestShippedConfigIsValidForTheTopology(t *testing.T) {
	t.Setenv("AURA_CONFIG", "")
	result, err := config.Load("aura.yaml")
	if err != nil {
		t.Fatalf("deploy/aura.yaml does not load: %v", err)
	}
	if result.Config.Storage.Path != "/var/lib/aura/data" {
		t.Fatalf("storage path = %q, want it inside the aura-data volume", result.Config.Storage.Path)
	}
	if result.Config.Tools.Workspace != "/var/lib/aura/workspace" {
		t.Fatalf("tools workspace = %q, want it inside the aura-data volume", result.Config.Tools.Workspace)
	}
	raw, err := os.ReadFile("aura.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var top map[string]any
	if err := yaml.Unmarshal(raw, &top); err != nil {
		t.Fatal(err)
	}
	if _, present := top["durable"]; present {
		t.Fatal("deploy/aura.yaml sets a durable section; the shipped example runs on the default configuration")
	}
}

func TestExecProfileFixtureLoadsForTheExecBuild(t *testing.T) {
	build, err := capability.ParseBuild("exec-linux", "exec-linux", "linux")
	if err != nil {
		t.Fatal(err)
	}
	registry, err := capability.BuiltinRegistry()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("AURA_CONFIG", "")
	result, err := config.LoadWithOptions("smoke/exec-profile.yaml", config.LoadOptions{Build: build, Registry: registry, Dependencies: capability.Dependencies{capability.DependencyProcessContainment: true}})
	if err != nil {
		t.Fatalf("smoke/exec-profile.yaml does not load for the exec-linux build: %v", err)
	}
	if !slices.Equal(result.Config.Capabilities.Enabled, []string{"exec-linux"}) {
		t.Fatalf("enabled capabilities = %v, want exec-linux", result.Config.Capabilities.Enabled)
	}
}

func TestCodeInterpreterFixtureNeedsABuildThatCompilesIt(t *testing.T) {
	registry, err := capability.BuiltinRegistry()
	if err != nil {
		t.Fatal(err)
	}
	contained := capability.Dependencies{capability.DependencyProcessContainment: true, capability.DependencyInterpreterRuntime: true}
	cases := []struct {
		name         string
		profile      string
		compiled     string
		dependencies capability.Dependencies
		wantCode     capability.ErrorCode
	}{
		{name: "exec build that compiles the interpreter", profile: "exec-linux", compiled: "exec-linux,code-interpreter", dependencies: contained},
		{name: "exec build without the interpreter", profile: "exec-linux", compiled: "exec-linux", dependencies: contained, wantCode: capability.ErrorCodeCapabilityUnavailable},
		{name: "core build", profile: "core", compiled: "", dependencies: contained, wantCode: capability.ErrorCodeCapabilityUnavailable},
		{name: "host without the interpreter runtime", profile: "exec-linux", compiled: "exec-linux,code-interpreter", dependencies: capability.Dependencies{capability.DependencyProcessContainment: true}, wantCode: capability.ErrorCodeDependencyMissing},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			build, err := capability.ParseBuild(tc.profile, tc.compiled, "linux")
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("AURA_CONFIG", "")
			result, err := config.LoadWithOptions("smoke/code-interpreter-profile.yaml", config.LoadOptions{Build: build, Registry: registry, Dependencies: tc.dependencies})
			if err != nil {
				t.Fatalf("smoke/code-interpreter-profile.yaml does not load: %v", err)
			}
			if tc.wantCode != "" {
				if code, ok := capability.CodeOf(result.CapabilityStateError); !ok || code != tc.wantCode {
					t.Fatalf("capability state error = %v, want code %q", result.CapabilityStateError, tc.wantCode)
				}
				return
			}
			if result.CapabilityStateError != nil {
				t.Fatalf("capability state error = %v", result.CapabilityStateError)
			}
			if !slices.Equal(result.Config.Capabilities.Enabled, []string{"exec-linux", "code-interpreter"}) {
				t.Fatalf("enabled capabilities = %v", result.Config.Capabilities.Enabled)
			}
			if !slices.Contains(result.CapabilityReport.Active(), "code-interpreter") {
				t.Fatalf("active capabilities = %v, want code-interpreter", result.CapabilityReport.Active())
			}
		})
	}
}
