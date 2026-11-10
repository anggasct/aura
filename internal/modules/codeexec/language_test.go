package codeexec

import (
	"slices"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/modules/codeexec/interpreter"
)

func defaultRuntime(t *testing.T, id string) interpreter.Runtime {
	t.Helper()
	manifest, err := interpreter.DefaultManifest()
	if err != nil {
		t.Fatal(err)
	}
	runtime, ok := manifest.Lookup(id)
	if !ok {
		t.Fatalf("the manifest has no %s runtime", id)
	}
	return runtime
}

func TestGoBuildEnvironmentPinsEveryPathInsideTheRunAndTheBundle(t *testing.T) {
	const (
		bundleDir = "/data/runtimes/go/1.27.3/bundle"
		runDir    = "/data/runs/run-1"
	)
	bundle := interpreter.Bundle{Runtime: defaultRuntime(t, "go"), Dir: bundleDir, Executable: bundleDir + "/go/bin/go"}
	lang, _ := languageFor("go")
	want := []string{
		"HOME=", "TMPDIR=" + runDir + "/.go/tmp", "LANG=C.UTF-8",
		"CGO_ENABLED=0", "GOENV=off", "GOFLAGS=-mod=mod", "GOMAXPROCS=4", "GOPROXY=off", "GOTOOLCHAIN=local", "GOWORK=off",
		"GOCACHE=" + runDir + "/.go/cache", "GOPATH=" + runDir + "/.go/path", "GOROOT=" + bundleDir + "/go", "PATH=" + bundleDir + "/go/bin",
	}
	got := buildEnvironment(runDir, &bundle, &lang)
	if !slices.Equal(got, want) {
		t.Fatalf("build environment = %v, want %v", got, want)
	}
	keys := map[string]bool{}
	for _, entry := range got {
		key, value, _ := strings.Cut(entry, "=")
		if keys[key] {
			t.Errorf("%s is set twice", key)
		}
		keys[key] = true
		if strings.HasPrefix(value, "/") && !strings.HasPrefix(value, runDir) && !strings.HasPrefix(value, bundleDir) {
			t.Errorf("%s=%s points outside the run directory and the bundle", key, value)
		}
	}
}

func TestRunEnvironmentIsMinimalAndCarriesNoToolchainSettings(t *testing.T) {
	const runDir = "/data/runs/run-1"
	cases := []struct {
		runtime string
		extra   []string
	}{
		{"python", nil},
		{"javascript", []string{"OPENSSL_CONF=/dev/null"}},
		{"go", nil},
	}
	for _, tc := range cases {
		t.Run(tc.runtime, func(t *testing.T) {
			runtime := defaultRuntime(t, tc.runtime)
			got := environment(runDir, &runtime)
			want := append([]string{"HOME=" + runDir, "TMPDIR=" + runDir, "LANG=C.UTF-8"}, tc.extra...)
			if !slices.Equal(got, want) {
				t.Fatalf("environment = %v, want %v", got, want)
			}
			for _, entry := range got {
				if strings.HasPrefix(entry, "PATH=") || strings.HasPrefix(entry, "GO") || strings.HasPrefix(entry, "CGO") {
					t.Fatalf("%s must not carry %s", tc.runtime, entry)
				}
			}
		})
	}
}

func TestSingleStageRuntimesHaveNoBuildEnvironment(t *testing.T) {
	for _, id := range []string{"python", "javascript"} {
		runtime := defaultRuntime(t, id)
		lang, _ := languageFor(id)
		if runtime.Builds() || lang.buildEnvironment != nil || lang.scratch != nil || lang.workspace != nil {
			t.Errorf("%s must run in one phase with no toolchain workspace", id)
		}
	}
}

func TestGoManifestKeepsBothInvocationsProfileOwned(t *testing.T) {
	runtime := defaultRuntime(t, "go")
	if !runtime.Builds() {
		t.Fatal("go must build before it runs")
	}
	build := runtime.BuildCommand("/bundle/go/bin/go", "main.go", "/run/.go/program")
	if want := []string{"/bundle/go/bin/go", "build", "-buildvcs=false", "-o", "/run/.go/program", "main.go"}; !slices.Equal(build, want) {
		t.Fatalf("build argv = %v, want %v", build, want)
	}
	if run := runtime.Command("/bundle/go/bin/go", "main.go", "/run/.go/program"); !slices.Equal(run, []string{"/run/.go/program"}) {
		t.Fatalf("run argv = %v", run)
	}
	want := map[string]string{
		"CGO_ENABLED": "0", "GOENV": "off", "GOFLAGS": "-mod=mod", "GOMAXPROCS": "4", "GOPROXY": "off", "GOTOOLCHAIN": "local", "GOWORK": "off",
	}
	for key, value := range want {
		if runtime.Build.Environment[key] != value {
			t.Errorf("build %s = %q, want %q", key, runtime.Build.Environment[key], value)
		}
	}
	if len(runtime.Build.Environment) != len(want) || len(runtime.Environment) != 0 {
		t.Errorf("build environment = %v, run environment = %v", runtime.Build.Environment, runtime.Environment)
	}
}

func TestGoProgramLivesInsideTheScratchDirectoryAndOutsideTheOutputDirectory(t *testing.T) {
	runtime := defaultRuntime(t, "go")
	lang, _ := languageFor("go")
	if !strings.HasPrefix(runtime.Build.Program, goScratchDir+"/") || strings.HasPrefix(runtime.Build.Program, outputDirName+"/") {
		t.Fatalf("program path %q must sit in %s and never in %s", runtime.Build.Program, goScratchDir, outputDirName)
	}
	for _, sub := range lang.scratch {
		if sub == runtime.Build.Program || strings.HasPrefix(runtime.Build.Program, sub+"/") {
			t.Fatalf("the build scratch %q would delete the program %q", sub, runtime.Build.Program)
		}
	}
}

func TestGoModuleFollowsThePinnedToolchainVersion(t *testing.T) {
	for version, want := range map[string]string{"1.27.3": "go 1.27.3", "1.28.0+build": "go 1.28.0"} {
		bundle := interpreter.Bundle{Runtime: interpreter.Runtime{Version: version}}
		files := goWorkspace(&bundle)
		if len(files) != 1 || files[0].name != "go.mod" || !strings.Contains(files[0].content, want+"\n") {
			t.Errorf("workspace for %s = %+v, want %q", version, files, want)
		}
	}
}
