package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestLayerViolations(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		from, to string
		violates bool
	}{
		{"platform to module", "internal/platform/turn/engine", "internal/modules/skills", true},
		{"platform to channel", "internal/platform/effect", "internal/channels/discord", true},
		{"platform to channel kit", "internal/platform/effect", "internal/channels/kit/access", true},
		{"platform to app", "internal/platform/effect", "internal/app", true},
		{"platform to platform", "internal/platform/effect", "internal/platform/turn", false},
		{"module to other module", "internal/modules/github", "internal/modules/workflow", true},
		{"module to own sub-package", "internal/modules/codeexec", "internal/modules/codeexec/interpreter", false},
		{"module to channel", "internal/modules/github", "internal/channels/webhook", true},
		{"module to channel kit", "internal/modules/memory", "internal/channels/kit/media", true},
		{"module to platform", "internal/modules/memory", "internal/platform/turn", false},
		{"module to app", "internal/modules/memory", "internal/app/config", true},
		{"channel to module", "internal/channels/discord", "internal/modules/memory", true},
		{"channel to other channel", "internal/channels/discord", "internal/channels/telegram", true},
		{"channel to channel kit", "internal/channels/discord", "internal/channels/kit/access", false},
		{"channel to platform", "internal/channels/discord", "internal/platform/turn", false},
		{"channel kit to channel", "internal/channels/kit/access", "internal/channels/discord", true},
		{"channel kit to module", "internal/channels/kit/access", "internal/modules/memory", true},
		{"channel kit to platform", "internal/channels/kit/access", "internal/platform/principal", false},
		{"plugin to wire", "plugins/gmail/connector", "internal/modules/plugin/wire", false},
		{"plugin to platform", "plugins/gmail/connector", "internal/platform/toolbroker", true},
		{"platform to cli", "internal/platform/turn", "internal/cli", true},
		{"cli to app", "internal/cli", "internal/app", false},
		{"cli to platform logging", "internal/cli", "internal/platform/logging", false},
		{"cli to platform turn", "internal/cli", "internal/platform/turn", true},
		{"cli to legacy config", "internal/cli", "internal/config", true},
		{"cli to module", "internal/cli", "internal/modules/memory", true},
		{"legacy source is exempt", "internal/config", "internal/modules/memory", false},
		{"legacy target is exempt", "internal/platform/turn", "internal/store", false},
		{"app is exempt", "internal/app", "internal/modules/memory", false},
		{"cmd is exempt", "cmd/aura", "internal/modules/memory", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := layerViolation(tc.from, tc.to); got != tc.violates {
				t.Fatalf("layerViolation(%q, %q) = %v, want %v", tc.from, tc.to, got, tc.violates)
			}
		})
	}
}

func TestCheckLayersHonoursAllowListAndReportsStaleEntries(t *testing.T) {
	t.Parallel()
	packages := []repoPackage{
		{path: "internal/modules/builtin", imports: []string{"internal/modules/codeexec/interpreter", "internal/modules/fetch", "internal/platform/tools"}},
		{path: "internal/platform/effect", imports: []string{"internal/channels/discord", "internal/channels/discord"}},
	}
	allowed, problems := parseAllow("internal/modules/builtin -> internal/modules/codeexec  # S12\n\ninternal/modules/memory -> internal/modules/profile # S08\n")
	if len(problems) != 0 {
		t.Fatalf("parseAllow problems = %v", problems)
	}
	got := checkLayers(packages, allowed)
	want := []string{
		"layers: internal/modules/builtin imports internal/modules/fetch",
		"layers: internal/platform/effect imports internal/channels/discord",
		"layers.allow:3: stale allow entry internal/modules/memory -> internal/modules/profile",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("checkLayers = %q, want %q", got, want)
	}
}

func TestCheckLayersReportsCliImportingPlatformPackages(t *testing.T) {
	t.Parallel()
	packages := []repoPackage{
		{path: "internal/cli", imports: []string{"internal/app", "internal/platform/turn"}},
	}
	got := checkLayers(packages, nil)
	want := []string{"layers: internal/cli imports internal/platform/turn"}
	if !slices.Equal(got, want) {
		t.Fatalf("checkLayers = %q, want %q", got, want)
	}
}

func TestParseAllowRejectsMalformedEntries(t *testing.T) {
	t.Parallel()
	_, problems := parseAllow("internal/modules/a internal/modules/b\n -> internal/modules/b\n")
	if len(problems) != 2 {
		t.Fatalf("problems = %q, want two malformed entries", problems)
	}
}

func TestCheckLayout(t *testing.T) {
	t.Parallel()
	entries := map[string][]dirEntry{
		"internal":          {{name: "platform", isDir: true}, {name: "modules", isDir: true}, {name: "store", isDir: true}, {name: "tools", isDir: true}, {name: "doc.go"}},
		"internal/modules":  {{name: "memory", isDir: true}, {name: "registry.go"}},
		"internal/platform": {{name: "turn", isDir: true}, {name: "README"}},
	}
	got := checkLayout(entries)
	want := []string{
		"layout: internal/tools is not an allowed top-level directory",
		"layout: file internal/doc.go is outside a package group",
		"layout: internal/modules/registry.go sits directly in a group directory",
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("checkLayout = %q, want %q", got, want)
	}
}

func TestCheckStdNames(t *testing.T) {
	t.Parallel()
	packages := []repoPackage{
		{path: "internal/platform/turn", name: "turn"},
		{path: "internal/modules/sync", name: "sync"},
		{path: "internal/modules/plugin", name: "plugin"},
		{path: "cmd/aura", name: "main"},
	}
	stdNames := map[string]string{"sync": "sync", "plugin": "plugin", "runtime": "runtime"}
	got := checkStdNames(packages, stdNames, map[string]bool{"sync": true, "runtime": true})
	if len(got) != 1 || !strings.Contains(got[0], "internal/modules/sync") {
		t.Fatalf("checkStdNames = %q, want only internal/modules/sync", got)
	}
}

func sourceFixture(t *testing.T, dir, source string) sourcePackage {
	t.Helper()
	name := path.Base(dir)
	file, err := parser.ParseFile(token.NewFileSet(), name+".go", "package "+name+"\n"+source, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse fixture %s: %v", dir, err)
	}
	return sourcePackage{path: dir, name: name, files: append([]*ast.File(nil), file)}
}

func fixtureViolations(file string, fields int, findings []finding) []string {
	return baseline{file: file, fields: fields}.apply(findings, "")
}

func TestFakesReportsFakeNamesOutsideTestKits(t *testing.T) {
	t.Parallel()
	declarations := "type FakeX struct{}\nfunc NewFake() *FakeX { return nil }\nvar StubY int\nconst MockZ = 1\n"
	cases := []struct {
		name   string
		dir    string
		source string
		want   []string
	}{
		{
			name:   "fake names in a production package",
			dir:    "internal/platform/widget",
			source: declarations,
			want: []string{
				"fakes: internal/platform/widget declares FakeX in a production file",
				"fakes: internal/platform/widget declares MockZ in a production file",
				"fakes: internal/platform/widget declares NewFake in a production file",
				"fakes: internal/platform/widget declares StubY in a production file",
			},
		},
		{
			name:   "fake names in a test kit",
			dir:    "internal/platform/durable/durabletest",
			source: declarations,
		},
		{
			name:   "names that only start with a fake word",
			dir:    "internal/platform/widget",
			source: "type Fakeness struct{}\ntype Faker struct{}\nfunc NewFaker() {}\nvar Stubby int\nconst Mockingbird = 1\nfunc (f *Faker) Fake() {}\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := fixtureViolations("fakes.allow", 2, findFakes([]sourcePackage{sourceFixture(t, tc.dir, tc.source)}))
			if !slices.Equal(got, tc.want) {
				t.Fatalf("findFakes = %q, want %q", got, tc.want)
			}
		})
	}
	t.Run("test files are not production sources", func(t *testing.T) {
		t.Parallel()
		listed := listedPackage{Dir: "/repo/pkg", GoFiles: []string{"a.go"}, IgnoredGoFiles: []string{"b_windows.go", "a_test.go"}}
		got := productionFiles(&listed)
		want := []string{"/repo/pkg/a.go", "/repo/pkg/b_windows.go"}
		if !slices.Equal(got, want) {
			t.Fatalf("productionFiles = %q, want %q", got, want)
		}
	})
	t.Run("an unparsable file fails with its path", func(t *testing.T) {
		t.Parallel()
		broken := filepath.Join(t.TempDir(), "broken.go")
		if err := os.WriteFile(broken, []byte("package broken\nfunc ("), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := parseSources([]repoPackage{{path: "internal/broken", files: []string{broken}}})
		if err == nil || !strings.Contains(err.Error(), broken) {
			t.Fatalf("parseSources error = %v, want one naming %s", err, broken)
		}
	})
}

func TestFakesReportsProductionImportsOfTestKits(t *testing.T) {
	t.Parallel()
	repo := &repository{stdNames: map[string]string{"testing": "testing"}, importedStd: map[string]bool{}}
	kitImport := modulePath + "/internal/store/storetest"
	cases := []struct {
		name   string
		listed listedPackage
		want   []string
	}{
		{
			name:   "production import",
			listed: listedPackage{ImportPath: modulePath + "/internal/modules/memory", Name: "memory", Imports: []string{kitImport}},
			want:   []string{"fakes: internal/modules/memory imports the test kit internal/store/storetest from a production file"},
		},
		{
			name:   "test imports",
			listed: listedPackage{ImportPath: modulePath + "/internal/modules/memory", Name: "memory", TestImports: []string{kitImport}, XTestImports: []string{kitImport}},
		},
		{
			name:   "test kit importing another test kit",
			listed: listedPackage{ImportPath: modulePath + "/internal/platform/durable/durabletest", Name: "durabletest", Imports: []string{kitImport}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			kit := sourcePackage{path: "internal/store/storetest", name: "storetest"}
			pkg := repo.newPackage(&tc.listed)
			subject := sourcePackage{path: pkg.path, name: pkg.name, imports: pkg.productionImports}
			got := fixtureViolations("fakes.allow", 2, findFakes([]sourcePackage{kit, subject}))
			if !slices.Equal(got, tc.want) {
				t.Fatalf("findFakes = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSettersReportsSingleArgumentDependencySetters(t *testing.T) {
	t.Parallel()
	const dir = "internal/modules/widget"
	report := func(method string) []string {
		return []string{"setters: " + dir + " " + method + " injects a dependency after construction"}
	}
	cases := []struct {
		name   string
		dir    string
		source string
		want   []string
	}{
		{name: "named dependency", dir: dir, source: "type T struct{}\nfunc (t *T) SetX(dep Dep) {}\n", want: report("T.SetX")},
		{name: "unnamed dependency", dir: dir, source: "type T struct{}\nfunc (t *T) SetX(Dep) {}\n", want: report("T.SetX")},
		{name: "func", dir: dir, source: "type T struct{}\nfunc (t *T) SetClock(now func() time.Time) {}\n", want: report("T.SetClock")},
		{name: "channel", dir: dir, source: "type T struct{}\nfunc (t T) SetInterrupts(ch <-chan struct{}) {}\n", want: report("T.SetInterrupts")},
		{name: "pointer", dir: dir, source: "type T struct{}\nfunc (t *T) SetTTY(r *TTYRenderer) {}\n", want: report("T.SetTTY")},
		{name: "qualified type", dir: dir, source: "type T struct{}\nfunc (t *T) SetLevel(l slog.Level) {}\n", want: report("T.SetLevel")},
		{name: "generic receiver", dir: dir, source: "type T[K any] struct{}\nfunc (t *T[K]) SetX(dep Dep) {}\n", want: report("T.SetX")},
		{name: "named type of another package type", dir: dir, source: "type T struct{}\ntype Mode Other\nfunc (t *T) SetMode(m Mode) {}\n", want: report("T.SetMode")},
		{name: "bool", dir: dir, source: "type T struct{}\nfunc (t *T) SetDraining(draining bool) {}\n"},
		{name: "string", dir: dir, source: "type T struct{}\nfunc (t *T) SetSessionID(id string) {}\n"},
		{name: "named basic type", dir: dir, source: "type T struct{}\ntype Mode int\nfunc (t *T) SetMode(m Mode) {}\n"},
		{name: "named type of a named basic type", dir: dir, source: "type T struct{}\ntype Base uint8\ntype Mode = Base\nfunc (t *T) SetMode(m Mode) {}\n"},
		{name: "no parameter", dir: dir, source: "type T struct{}\nfunc (t *T) SetIrrecoverable() {}\n"},
		{name: "several parameters", dir: dir, source: "type T struct{}\nfunc (t *T) SetState(ctx context.Context, id, state string) {}\n"},
		{name: "two names of one type", dir: dir, source: "type T struct{}\nfunc (t *T) SetRange(low, high Dep) {}\n"},
		{name: "variadic", dir: dir, source: "type T struct{}\nfunc (t *T) SetIssuers(issuers ...string) {}\n"},
		{name: "function without receiver", dir: dir, source: "func SetDefault(l *slog.Logger) {}\n"},
		{name: "name without a capital letter", dir: dir, source: "type T struct{}\nfunc (t *T) Setup(dep Dep) {}\nfunc (t *T) Set(dep Dep) {}\nfunc (t *T) Settle(dep Dep) {}\n"},
		{name: "test kit", dir: "internal/store/storetest", source: "type T struct{}\nfunc (t *T) SetX(dep Dep) {}\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := fixtureViolations("setters.allow", 2, findSetters([]sourcePackage{sourceFixture(t, tc.dir, tc.source)}))
			if !slices.Equal(got, tc.want) {
				t.Fatalf("findSetters = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCodedErrorsReportsCopiedErrorTypesOutsideFault(t *testing.T) {
	t.Parallel()
	const dir = "internal/modules/widget"
	errorType := "codederrors: " + dir + " declares its own Error type; use platform/fault"
	codeOf := "codederrors: " + dir + " declares its own CodeOf; use fault.CodeOf"
	cases := []struct {
		name   string
		dir    string
		source string
		want   []string
	}{
		{name: "struct type", dir: dir, source: "type Error struct{}\n", want: []string{errorType}},
		{name: "alias", dir: dir, source: "type Error = fault.Error[ErrorCode]\n", want: []string{errorType}},
		{name: "function", dir: dir, source: "func CodeOf(err error) ErrorCode { return \"\" }\n", want: []string{codeOf}},
		{name: "both", dir: dir, source: "type Error struct{}\nfunc CodeOf(err error) ErrorCode { return \"\" }\n", want: []string{codeOf, errorType}},
		{name: "fault package", dir: faultPackage, source: "type Error struct{}\nfunc CodeOf(err error) string { return \"\" }\n"},
		{name: "method", dir: dir, source: "type Probe struct{}\nfunc (p *Probe) CodeOf() string { return \"\" }\n"},
		{name: "error code type", dir: dir, source: "type ErrorCode string\nfunc Error() {}\nvar CodeOf = 1\n"},
		{name: "test kit", dir: "internal/store/storetest", source: "type Error struct{}\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := fixtureViolations("codederrors.allow", 1, findCodedErrors([]sourcePackage{sourceFixture(t, tc.dir, tc.source)}))
			if !slices.Equal(got, tc.want) {
				t.Fatalf("findCodedErrors = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBaselinesReportStaleDuplicateAndMalformedEntries(t *testing.T) {
	t.Parallel()
	setters := baseline{file: "setters.allow", fields: 2}
	found := []finding{
		{entry: "internal/app Real.SetReal", message: "setters: internal/app Real.SetReal injects a dependency after construction"},
		{entry: "internal/app Other.SetOther", message: "setters: internal/app Other.SetOther injects a dependency after construction"},
		{message: "fakes: internal/app imports the test kit internal/store/storetest from a production file"},
	}
	cases := []struct {
		name    string
		content string
		want    []string
	}{
		{
			name:    "entries cover the findings they match",
			content: "# header\n\ninternal/app Real.SetReal  # reason\ninternal/app Other.SetOther\n",
			want:    []string{"fakes: internal/app imports the test kit internal/store/storetest from a production file"},
		},
		{
			name:    "stale entry",
			content: "internal/app Real.SetReal  # reason\ninternal/app Other.SetOther\ninternal/app Nope.SetNope  # gone\n",
			want: []string{
				"fakes: internal/app imports the test kit internal/store/storetest from a production file",
				"setters.allow:3: stale allow entry internal/app Nope.SetNope",
			},
		},
		{
			name:    "duplicate entry",
			content: "internal/app Real.SetReal\ninternal/app  Real.SetReal  # again\ninternal/app Other.SetOther\n",
			want: []string{
				"fakes: internal/app imports the test kit internal/store/storetest from a production file",
				"setters.allow:2: duplicate entry internal/app Real.SetReal",
			},
		},
		{
			name:    "too many fields",
			content: "internal/app Real.SetReal extra\ninternal/app Other.SetOther\n",
			want: []string{
				"fakes: internal/app imports the test kit internal/store/storetest from a production file",
				"setters.allow:1: malformed entry \"internal/app Real.SetReal extra\"",
				"setters: internal/app Real.SetReal injects a dependency after construction",
			},
		},
		{
			name:    "missing baseline",
			content: "",
			want: []string{
				"fakes: internal/app imports the test kit internal/store/storetest from a production file",
				"setters: internal/app Other.SetOther injects a dependency after construction",
				"setters: internal/app Real.SetReal injects a dependency after construction",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := setters.apply(found, tc.content)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("apply = %q, want %q", got, tc.want)
			}
		})
	}
	t.Run("a file that does not exist reads as an empty baseline", func(t *testing.T) {
		t.Parallel()
		content, err := readOptional(filepath.Join(t.TempDir(), "missing.allow"))
		if err != nil || content != "" {
			t.Fatalf("readOptional = %q, %v, want an empty baseline", content, err)
		}
	})
}
