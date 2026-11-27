package main

import (
	"bytes"
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
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
		{"cli to module", "internal/cli", "internal/modules/memory", true},
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

func TestCheckLayersReportsViolationsDirectly(t *testing.T) {
	t.Parallel()
	packages := []repoPackage{
		{path: "internal/modules/builtin", imports: []string{"internal/modules/fetch", "internal/platform/tools"}},
		{path: "internal/platform/effect", imports: []string{"internal/channels/discord", "internal/channels/discord"}},
	}
	got := checkLayers(packages)
	want := []string{
		"layers: internal/modules/builtin imports internal/modules/fetch",
		"layers: internal/platform/effect imports internal/channels/discord",
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
	got := checkLayers(packages)
	want := []string{"layers: internal/cli imports internal/platform/turn"}
	if !slices.Equal(got, want) {
		t.Fatalf("checkLayers = %q, want %q", got, want)
	}
}

func TestCheckLayout(t *testing.T) {
	t.Parallel()
	entries := map[string][]dirEntry{
		"internal":          {{name: "platform", isDir: true}, {name: "modules", isDir: true}, {name: "tools", isDir: true}, {name: "doc.go"}},
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

func TestCheckAppLayout(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		entries []dirEntry
		want    []string
	}{
		{
			name: "valid composition root with tests and testdata",
			entries: []dirEntry{
				{name: "app.go"},
				{name: "modules.go"},
				{name: "commands.go"},
				{name: "migrations.go"},
				{name: "app_test.go"},
				{name: "durable_turn_killed_test.go"},
				{name: "config", isDir: true},
				{name: "testdata", isDir: true},
			},
		},
		{
			name: "extra production go file",
			entries: []dirEntry{
				{name: "app.go"},
				{name: "modules.go"},
				{name: "commands.go"},
				{name: "migrations.go"},
				{name: "extra.go"},
				{name: "config", isDir: true},
			},
			want: []string{
				"app-layout: internal/app/extra.go is not in the allowed composition-root file set (app.go, modules.go, commands.go, migrations.go)",
			},
		},
		{
			name: "extra subdirectory",
			entries: []dirEntry{
				{name: "app.go"},
				{name: "config", isDir: true},
				{name: "helpers", isDir: true},
			},
			want: []string{
				"app-layout: internal/app/helpers is not an allowed subdirectory (config)",
			},
		},
		{
			name: "extra non-test non-go file and extra subdir",
			entries: []dirEntry{
				{name: "app.go"},
				{name: "notes.txt"},
				{name: "scratch", isDir: true},
				{name: "app_linux_test.go"},
			},
			want: []string{
				"app-layout: internal/app/notes.txt is not in the allowed composition-root file set (app.go, modules.go, commands.go, migrations.go)",
				"app-layout: internal/app/scratch is not an allowed subdirectory (config)",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := checkAppLayout(tc.entries)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("checkAppLayout = %q, want %q", got, tc.want)
			}
		})
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
			got := findFakes([]sourcePackage{sourceFixture(t, tc.dir, tc.source)})
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
	kitImport := modulePath + "/internal/platform/sqlite/sqlitetest"
	cases := []struct {
		name   string
		listed listedPackage
		want   []string
	}{
		{
			name:   "production import",
			listed: listedPackage{ImportPath: modulePath + "/internal/modules/memory", Name: "memory", Imports: []string{kitImport}},
			want:   []string{"fakes: internal/modules/memory imports the test kit internal/platform/sqlite/sqlitetest from a production file"},
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
			kit := sourcePackage{path: "internal/platform/sqlite/sqlitetest", name: "sqlitetest"}
			pkg := repo.newPackage(&tc.listed)
			subject := sourcePackage{path: pkg.path, name: pkg.name, imports: pkg.productionImports}
			got := findFakes([]sourcePackage{kit, subject})
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
		{name: "test kit", dir: "internal/platform/sqlite/sqlitetest", source: "type T struct{}\nfunc (t *T) SetX(dep Dep) {}\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := findSetters([]sourcePackage{sourceFixture(t, tc.dir, tc.source)})
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
		{name: "test kit", dir: "internal/platform/sqlite/sqlitetest", source: "type Error struct{}\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := findCodedErrors([]sourcePackage{sourceFixture(t, tc.dir, tc.source)})
			if !slices.Equal(got, tc.want) {
				t.Fatalf("findCodedErrors = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCodedErrorsExemptsPluginWireProtocolBoundary(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		dir    string
		source string
		want   []string
	}{
		{name: "plugin wire contract", dir: pluginWire, source: "type Error struct{}\nfunc CodeOf(err error) string { return \"\" }\n"},
		{name: "out-of-process plugin connector", dir: "plugins/gmail/connector", source: "type Error struct{}\n"},
		{
			name:   "plugin module is still checked",
			dir:    "internal/modules/plugin",
			source: "type Error struct{}\nfunc CodeOf(err error) string { return \"\" }\n",
			want: []string{
				"codederrors: internal/modules/plugin declares its own CodeOf; use fault.CodeOf",
				"codederrors: internal/modules/plugin declares its own Error type; use platform/fault",
			},
		},
		{
			name:   "plugin connector host is still checked",
			dir:    "internal/modules/plugin/connector",
			source: "type Error struct{}\n",
			want: []string{
				"codederrors: internal/modules/plugin/connector declares its own Error type; use platform/fault",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := findCodedErrors([]sourcePackage{sourceFixture(t, tc.dir, tc.source)})
			if !slices.Equal(got, tc.want) {
				t.Fatalf("findCodedErrors = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestAllowListAntiResurrection(t *testing.T) {
	t.Parallel()
	for _, check := range []string{"layers", "fakes", "setters", "codederrors", "operation-definitions"} {
		t.Run(check+" fails when allow-list file exists", func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			allowPath := filepath.Join(dir, check+".allow")
			deps := runDeps{
				allowDir:      dir,
				moduleMapFile: filepath.Join(dir, "module-map.txt"),
				readGroups:    func() (map[string][]dirEntry, error) { return map[string][]dirEntry{}, nil },
				readApp:       func() ([]dirEntry, error) { return nil, nil },
				loadRepo:      func(context.Context) (repository, error) { return repository{}, nil },
			}
			var stdout, stderr bytes.Buffer
			if code := runWithDeps(t.Context(), []string{check}, &stdout, &stderr, deps); code != 0 {
				t.Fatalf("without allow-list: code = %d stdout=%q stderr=%q, want 0", code, stdout.String(), stderr.String())
			}
			if err := os.WriteFile(allowPath, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			stdout.Reset()
			stderr.Reset()
			code := runWithDeps(t.Context(), []string{check}, &stdout, &stderr, deps)
			want := filepath.ToSlash(allowPath) + ": allow-list files are forbidden after close-out\n"
			if code != 1 || stdout.String() != want {
				t.Fatalf("with allow-list: code = %d stdout=%q stderr=%q, want 1 and %q", code, stdout.String(), stderr.String(), want)
			}
		})
	}
}

type typedFixture struct {
	dir    string
	source string
}

type fixtureImporter map[string]*types.Package

func (f fixtureImporter) Import(importPath string) (*types.Package, error) {
	if pkg, ok := f[importPath]; ok {
		return pkg, nil
	}
	return nil, fmt.Errorf("fixture package %s is not declared", importPath)
}

func typedSources(t *testing.T, fixtures ...typedFixture) []sourcePackage {
	t.Helper()
	fset := token.NewFileSet()
	imp := fixtureImporter{}
	sources := make([]sourcePackage, 0, len(fixtures))
	for _, fixture := range fixtures {
		name := path.Base(fixture.dir)
		file, err := parser.ParseFile(fset, name+".go", "package "+name+"\n"+fixture.source, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse fixture %s: %v", fixture.dir, err)
		}
		source := sourcePackage{path: fixture.dir, name: name, files: []*ast.File{file}, fset: fset}
		for _, spec := range file.Imports {
			source.imports = append(source.imports, relativePath(strings.Trim(spec.Path.Value, `"`)))
		}
		checked, err := typeCheck(&source, imp)
		if err != nil {
			t.Fatalf("type-check fixture %s: %v", fixture.dir, err)
		}
		imp[modulePath+"/"+fixture.dir] = checked
		sources = append(sources, source)
	}
	return sources
}

func TestOperationDefinitionsAreOwnedByTheOperationPackage(t *testing.T) {
	t.Parallel()
	const toolsImport = "github.com/anggasct/aura/internal/platform/tools"
	toolsFixture := typedFixture{dir: toolsPackage, source: "type Definition struct{ Name string; Operation string; OwnerNotice bool }\ntype Other struct{ Operation string }\n"}
	aliasFixture := typedFixture{dir: "internal/platform/shapes", source: "import \"" + toolsImport + "\"\ntype Tool = tools.Definition\n"}
	const shapesImport = "github.com/anggasct/aura/internal/platform/shapes"
	cases := []struct {
		name   string
		dir    string
		source string
		extra  []typedFixture
		alone  bool
		want   []string
	}{
		{
			name:   "a literal with an operation outside the operation package",
			dir:    "internal/modules/widget",
			source: "import \"" + toolsImport + "\"\nvar d = tools.Definition{Name: \"x\", Operation: \"widget.run\"}\n",
			want:   []string{"operation-definitions: internal/modules/widget sets Definition.Operation outside internal/platform/operation"},
		},
		{
			name:   "a pointer literal with an owner notice",
			dir:    "internal/modules/widget",
			source: "import \"" + toolsImport + "\"\nvar d = &tools.Definition{OwnerNotice: true}\n",
			want:   []string{"operation-definitions: internal/modules/widget sets Definition.OwnerNotice outside internal/platform/operation"},
		},
		{
			name:   "a slice of definitions with elided types",
			dir:    "internal/modules/widget",
			source: "import \"" + toolsImport + "\"\nvar d = []tools.Definition{{Name: \"x\"}, {Operation: \"a.b\"}}\n",
			want:   []string{"operation-definitions: internal/modules/widget sets Definition.Operation outside internal/platform/operation"},
		},
		{
			name:   "a slice of definition pointers with elided types",
			dir:    "internal/modules/widget",
			source: "import \"" + toolsImport + "\"\nvar d = []*tools.Definition{{Operation: \"a.b\"}}\n",
			want:   []string{"operation-definitions: internal/modules/widget sets Definition.Operation outside internal/platform/operation"},
		},
		{
			name:   "a map of definitions with elided types",
			dir:    "internal/modules/widget",
			source: "import \"" + toolsImport + "\"\nvar d = map[string]*tools.Definition{\"k\": {OwnerNotice: true}}\n",
			want:   []string{"operation-definitions: internal/modules/widget sets Definition.OwnerNotice outside internal/platform/operation"},
		},
		{
			name:   "an assignment through a pointer parameter",
			dir:    "internal/modules/widget",
			source: "import \"" + toolsImport + "\"\nfunc set(d *tools.Definition) { d.OwnerNotice = true }\n",
			want:   []string{"operation-definitions: internal/modules/widget sets Definition.OwnerNotice outside internal/platform/operation"},
		},
		{
			name:   "an assignment through a slice element and a struct field",
			dir:    "internal/modules/widget",
			source: "import \"" + toolsImport + "\"\ntype holder struct{ def tools.Definition }\nfunc set(ds []tools.Definition, h *holder) { ds[0].Operation = \"a.b\"; h.def.OwnerNotice = true }\n",
			want: []string{
				"operation-definitions: internal/modules/widget sets Definition.Operation outside internal/platform/operation",
				"operation-definitions: internal/modules/widget sets Definition.OwnerNotice outside internal/platform/operation",
			},
		},
		{
			name:   "an address taken from the field",
			dir:    "internal/modules/widget",
			source: "import \"" + toolsImport + "\"\nfunc take(d *tools.Definition) *string { return &d.Operation }\n",
			want:   []string{"operation-definitions: internal/modules/widget sets Definition.Operation outside internal/platform/operation"},
		},
		{
			name:   "a dot import",
			dir:    "internal/modules/widget",
			source: "import . \"" + toolsImport + "\"\nvar d = Definition{Operation: \"a.b\"}\n",
			want:   []string{"operation-definitions: internal/modules/widget sets Definition.Operation outside internal/platform/operation"},
		},
		{
			name:   "a renamed import",
			dir:    "internal/modules/widget",
			source: "import tl \"" + toolsImport + "\"\nvar d = tl.Definition{Operation: \"a.b\"}\n",
			want:   []string{"operation-definitions: internal/modules/widget sets Definition.Operation outside internal/platform/operation"},
		},
		{
			name:   "a local type alias",
			dir:    "internal/modules/widget",
			source: "import \"" + toolsImport + "\"\ntype D = tools.Definition\nvar d = D{Operation: \"a.b\"}\n",
			want:   []string{"operation-definitions: internal/modules/widget sets Definition.Operation outside internal/platform/operation"},
		},
		{
			name:   "an alias declared in another package",
			dir:    "internal/modules/widget",
			source: "import \"" + shapesImport + "\"\nvar d = shapes.Tool{OwnerNotice: true}\n",
			extra:  []typedFixture{aliasFixture},
			want:   []string{"operation-definitions: internal/modules/widget sets Definition.OwnerNotice outside internal/platform/operation"},
		},
		{
			name:   "a literal without field names",
			dir:    "internal/modules/widget",
			source: "import \"" + toolsImport + "\"\nvar d = tools.Definition{\"x\", \"a.b\", true}\n",
			want:   []string{"operation-definitions: internal/modules/widget builds a Definition without field names outside internal/platform/operation"},
		},
		{
			name:   "the tools package itself",
			dir:    "internal/platform/tools",
			source: "type Definition struct{ Name string; Operation string }\nvar d = Definition{Operation: \"a.b\"}\n",
			alone:  true,
			want:   []string{"operation-definitions: internal/platform/tools sets Definition.Operation outside internal/platform/operation"},
		},
		{
			name:   "the operation package",
			dir:    "internal/platform/operation",
			source: "import \"" + toolsImport + "\"\nvar d = tools.Definition{Operation: \"a.b\", OwnerNotice: true}\n",
		},
		{
			name:   "another type with an Operation field in a package that imports tools",
			dir:    "internal/modules/widget",
			source: "import \"" + toolsImport + "\"\ntype request struct{ Operation string }\nvar _ tools.Definition\nvar r = request{Operation: \"x\"}\nfunc set(r *request) { r.Operation = \"y\" }\n",
		},
		{
			name:   "the other struct of the tools package",
			dir:    "internal/modules/widget",
			source: "import \"" + toolsImport + "\"\nvar o = tools.Other{Operation: \"x\"}\nfunc set(o *tools.Other) { o.Operation = \"y\" }\n",
		},
		{
			name:   "reading the fields",
			dir:    "internal/modules/widget",
			source: "import \"" + toolsImport + "\"\nfunc read(d *tools.Definition) (string, bool) { return d.Operation, d.OwnerNotice }\n",
		},
		{
			name:   "a definition that sets neither field",
			dir:    "internal/modules/widget",
			source: "import \"" + toolsImport + "\"\nvar d = tools.Definition{Name: \"x\"}\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fixtures := []typedFixture{{dir: tc.dir, source: tc.source}}
			if !tc.alone {
				fixtures = append(append([]typedFixture{toolsFixture}, tc.extra...), fixtures...)
			}
			subject := typedSources(t, fixtures...)
			got := findOperationDefinitions(subject[len(subject)-1:])
			if !slices.Equal(got, tc.want) {
				t.Fatalf("findOperationDefinitions = %q, want %q", got, tc.want)
			}
		})
	}
	t.Run("a test kit may build definitions", func(t *testing.T) {
		t.Parallel()
		subject := typedSources(t, toolsFixture, typedFixture{dir: "internal/modules/widget/widgettest", source: "import \"" + toolsImport + "\"\nvar d = tools.Definition{Operation: \"a.b\"}\n"})
		if got := findOperationDefinitions(subject[1:]); len(got) != 0 {
			t.Fatalf("findOperationDefinitions = %q, want none", got)
		}
	})
	t.Run("a package that was not type-checked is not skipped silently", func(t *testing.T) {
		t.Parallel()
		subject := sourcePackage{path: "internal/modules/widget", name: "widget", imports: []string{toolsPackage}}
		if !isOperationCandidate(&subject) {
			t.Fatal("a package importing the tools package is not a candidate for type-checking")
		}
		plain := sourcePackage{path: "internal/modules/plain", name: "plain"}
		if isOperationCandidate(&plain) {
			t.Fatal("a package with no tools import and no Operation identifier is a candidate")
		}
		named := sourceFixture(t, "internal/modules/named", "type request struct{ Operation string }\n")
		if !isOperationCandidate(&named) {
			t.Fatal("a package that names an Operation field is not a candidate")
		}
	})
	t.Run("an export that cannot be imported fails the run", func(t *testing.T) {
		t.Parallel()
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, "widget.go", "package widget\nimport \"github.com/anggasct/aura/internal/platform/tools\"\nvar d = tools.Definition{}\n", parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		source := sourcePackage{path: "internal/modules/widget", name: "widget", imports: []string{toolsPackage}, files: []*ast.File{file}, fset: fset}
		load := func(context.Context) (map[string]string, error) { return map[string]string{}, nil }
		if err := typeOperationCandidates(t.Context(), []sourcePackage{source}, load); err == nil {
			t.Fatal("typeOperationCandidates accepted a package whose imports have no export data")
		}
	})
}

func TestModuleMap(t *testing.T) {
	t.Parallel()
	samplePackages := []repoPackage{
		{path: "scripts/archcheck", name: "main"},
		{path: "plugins/gmail/connector", name: "connector"},
		{path: "internal/channels/discord", name: "discord"},
		{path: "internal/modules/memory", name: "memory"},
		{path: "internal/platform/turn/engine", name: "runtimeengine"},
		{path: "internal/platform/sqlite/sqlitetest", name: "sqlitetest"},
		{path: "internal/platform/sqlite", name: "sqlite"},
		{path: "internal/cli", name: "cli"},
		{path: "internal/app/config", name: "config"},
		{path: "internal/app", name: "app"},
		{path: "cmd/aura-connector-gmail", name: "main"},
		{path: "cmd/aura", name: "main"},
	}
	wantRendered := strings.Join([]string{
		"cmd/",
		"  cmd/aura",
		"  cmd/aura-connector-gmail",
		"",
		"internal/app/",
		"  internal/app",
		"  internal/app/config",
		"",
		"internal/cli/",
		"  internal/cli",
		"",
		"internal/platform/",
		"  internal/platform/sqlite",
		"  internal/platform/sqlite/sqlitetest",
		"  internal/platform/turn/engine (package runtimeengine)",
		"",
		"internal/modules/",
		"  internal/modules/memory",
		"",
		"internal/channels/",
		"  internal/channels/discord",
		"",
		"plugins/",
		"  plugins/gmail/connector",
		"",
	}, "\n")

	t.Run("deterministic rendering omits package main and annotates mismatched names", func(t *testing.T) {
		t.Parallel()
		got, err := renderModuleMap(samplePackages)
		if err != nil {
			t.Fatalf("renderModuleMap: %v", err)
		}
		if got != wantRendered {
			t.Fatalf("renderModuleMap =\n%s\nwant:\n%s", got, wantRendered)
		}
		if strings.Contains(got, "(package main)") {
			t.Fatalf("rendered map must omit (package main):\n%s", got)
		}
		if strings.Contains(got, "scripts/archcheck") {
			t.Fatalf("rendered map must exclude scripts/:\n%s", got)
		}
	})

	t.Run("unknown section under internal fails rather than dropping package", func(t *testing.T) {
		t.Parallel()
		_, err := renderModuleMap([]repoPackage{{path: "internal/orphan", name: "orphan"}})
		if err == nil || !strings.Contains(err.Error(), "internal/orphan") {
			t.Fatalf("renderModuleMap error = %v, want error naming internal/orphan", err)
		}
	})

	t.Run("stdout mode prints rendered map and exits 0", func(t *testing.T) {
		t.Parallel()
		deps := runDeps{
			allowDir:      t.TempDir(),
			moduleMapFile: filepath.Join(t.TempDir(), "module-map.txt"),
			loadRepo:      func(context.Context) (repository, error) { return repository{packages: samplePackages}, nil },
		}
		var stdout, stderr bytes.Buffer
		code := runWithDeps(t.Context(), []string{"modulemap"}, &stdout, &stderr, deps)
		if code != 0 || stdout.String() != wantRendered || stderr.Len() != 0 {
			t.Fatalf("code=%d stdout=%q stderr=%q, want 0 and rendered map", code, stdout.String(), stderr.String())
		}
	})

	t.Run("write creates 0644 file and check detects match added or missing package", func(t *testing.T) {
		t.Parallel()
		target := filepath.Join(t.TempDir(), "golden", "module-map.txt")
		load := func(pkgs []repoPackage) func(context.Context) (repository, error) {
			return func(context.Context) (repository, error) { return repository{packages: pkgs}, nil }
		}
		deps := runDeps{allowDir: t.TempDir(), moduleMapFile: target, loadRepo: load(samplePackages)}
		var stdout, stderr bytes.Buffer
		if code := runWithDeps(t.Context(), []string{"modulemap", "-write", "-file", target}, &stdout, &stderr, deps); code != 0 {
			t.Fatalf("-write code=%d stderr=%q, want 0", code, stderr.String())
		}
		info, err := os.Stat(target)
		if err != nil {
			t.Fatalf("stat %s: %v", target, err)
		}
		if mode := info.Mode().Perm(); mode != 0o644 {
			t.Fatalf("file mode = %o, want 0644", mode)
		}
		stdout.Reset()
		stderr.Reset()
		if code := runWithDeps(t.Context(), []string{"modulemap", "-check", "-file", target}, &stdout, &stderr, deps); code != 0 {
			t.Fatalf("-check match code=%d stdout=%q stderr=%q, want 0", code, stdout.String(), stderr.String())
		}

		added := append(slices.Clone(samplePackages), repoPackage{path: "internal/modules/newmod", name: "newmod"})
		deps.loadRepo = load(added)
		stdout.Reset()
		stderr.Reset()
		if code := runWithDeps(t.Context(), []string{"modulemap", "-check", "-file", target}, &stdout, &stderr, deps); code != 1 || !strings.Contains(stdout.String(), "differs at line") {
			t.Fatalf("-check added package code=%d stdout=%q stderr=%q, want 1 with differing line", code, stdout.String(), stderr.String())
		}

		missing := slices.DeleteFunc(slices.Clone(samplePackages), func(p repoPackage) bool { return p.path == "internal/modules/memory" })
		deps.loadRepo = load(missing)
		stdout.Reset()
		stderr.Reset()
		if code := runWithDeps(t.Context(), []string{"modulemap", "-check", "-file", target}, &stdout, &stderr, deps); code != 1 || !strings.Contains(stdout.String(), "differs at line") {
			t.Fatalf("-check missing package code=%d stdout=%q stderr=%q, want 1 with differing line", code, stdout.String(), stderr.String())
		}
	})

	t.Run("flag validation rejects invalid modulemap invocations with exit 2", func(t *testing.T) {
		t.Parallel()
		deps := runDeps{
			allowDir:      t.TempDir(),
			moduleMapFile: filepath.Join(t.TempDir(), "module-map.txt"),
			loadRepo:      func(context.Context) (repository, error) { return repository{packages: samplePackages}, nil },
		}
		cases := []struct {
			name string
			args []string
		}{
			{"check and write together", []string{"modulemap", "-check", "-write"}},
			{"unknown flag", []string{"modulemap", "-unknown"}},
			{"extra positional arg", []string{"modulemap", "extra"}},
			{"flag in multi-check", []string{"layout", "-check"}},
			{"flag after modulemap in multi-check", []string{"layout", "modulemap", "-check"}},
			{"unknown check", []string{"nope"}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				var stdout, stderr bytes.Buffer
				if code := runWithDeps(t.Context(), tc.args, &stdout, &stderr, deps); code != 2 || stderr.Len() == 0 {
					t.Fatalf("args %v: code=%d stdout=%q stderr=%q, want 2 with stderr message", tc.args, code, stdout.String(), stderr.String())
				}
			})
		}
	})

	t.Run("multi-check layout modulemap runs check against default golden file", func(t *testing.T) {
		t.Parallel()
		target := filepath.Join(t.TempDir(), "module-map.txt")
		if err := os.WriteFile(target, []byte(wantRendered), 0o600); err != nil {
			t.Fatal(err)
		}
		deps := runDeps{
			allowDir:      t.TempDir(),
			moduleMapFile: target,
			readGroups:    func() (map[string][]dirEntry, error) { return map[string][]dirEntry{}, nil },
			readApp:       func() ([]dirEntry, error) { return nil, nil },
			loadRepo:      func(context.Context) (repository, error) { return repository{packages: samplePackages}, nil },
		}
		var stdout, stderr bytes.Buffer
		if code := runWithDeps(t.Context(), []string{"layout", "modulemap"}, &stdout, &stderr, deps); code != 0 {
			t.Fatalf("layout modulemap match: code=%d stdout=%q stderr=%q, want 0", code, stdout.String(), stderr.String())
		}
		if err := os.WriteFile(target, []byte("cmd/\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		stdout.Reset()
		stderr.Reset()
		if code := runWithDeps(t.Context(), []string{"layout", "modulemap"}, &stdout, &stderr, deps); code != 1 || !strings.Contains(stdout.String(), "differs at line") {
			t.Fatalf("layout modulemap mismatch: code=%d stdout=%q stderr=%q, want 1 with differing line", code, stdout.String(), stderr.String())
		}
	})
}
