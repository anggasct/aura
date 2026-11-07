package deploy

import (
	"os"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/anggasct/aura/internal/capability"
	"github.com/anggasct/aura/internal/config"
)

type composeFile struct {
	Services map[string]composeService `yaml:"services"`
	Networks map[string]struct {
		Internal bool `yaml:"internal"`
	} `yaml:"networks"`
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

func TestRestateStaysOnThePrivateNetwork(t *testing.T) {
	file := loadCompose(t, "compose.yaml")
	restate := service(t, &file, "restate")
	if len(restate.Ports) != 0 {
		t.Fatalf("restate publishes ports %v; every Restate port must stay on the internal network", restate.Ports)
	}
	if !slices.Equal(restate.Networks, []string{"internal"}) {
		t.Fatalf("restate networks = %v, want only internal", restate.Networks)
	}
	if !file.Networks["internal"].Internal {
		t.Fatal("the internal network must be declared internal: true")
	}
	if !strings.HasPrefix(restate.Image, "docker.restate.dev/restatedev/restate:") || !strings.Contains(restate.Image, "@sha256:") {
		t.Fatalf("restate image %q must be the official image pinned by digest", restate.Image)
	}
	for name, svc := range file.Services {
		if name != "restate" && len(svc.Ports) != 0 {
			t.Fatalf("%s publishes ports %v; the canonical topology publishes none", name, svc.Ports)
		}
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

func TestAuraStartsOnlyAfterTheUpgradeJobAndAHealthyRestate(t *testing.T) {
	file := loadCompose(t, "compose.yaml")
	aura := service(t, &file, "aura")
	want := map[string]string{"aura-upgrade": "service_completed_successfully", "restate": "service_healthy"}
	for dependency, condition := range want {
		if got := aura.DependsOn[dependency]["condition"]; got != condition {
			t.Fatalf("aura depends_on %s condition = %q, want %q", dependency, got, condition)
		}
	}
	if !slices.Contains(aura.Networks, "internal") {
		t.Fatalf("aura networks = %v, want internal so Restate can reach the handler", aura.Networks)
	}
}

func TestShippedConfigIsValidForTheTopology(t *testing.T) {
	t.Setenv("AURA_CONFIG", "")
	result, err := config.Load("aura.yaml")
	if err != nil {
		t.Fatalf("deploy/aura.yaml does not load: %v", err)
	}
	durable := result.Config.Durable
	if !durable.Enabled || durable.Mode != config.DurableModeExternal || durable.HandlerURL != "http://aura:9080" {
		t.Fatalf("durable = %+v, want external mode registering http://aura:9080", durable)
	}
	if durable.Endpoint != "http://restate:8080" || durable.AdminEndpoint != "http://restate:9070" {
		t.Fatalf("durable endpoints = %s %s", durable.Endpoint, durable.AdminEndpoint)
	}
}

func TestSmokeOverrideOnlyMountsTheSmokeWorkflows(t *testing.T) {
	file := loadCompose(t, "smoke/compose.smoke.yaml")
	if len(file.Services) != 1 || !slices.Equal(service(t, &file, "aura").Volumes, []string{"./smoke/workflows:/etc/aura/workflows:ro"}) {
		t.Fatalf("smoke override = %+v", file.Services)
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
