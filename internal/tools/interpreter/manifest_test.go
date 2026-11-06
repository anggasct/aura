package interpreter

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDefaultManifestPinsEveryRuntime(t *testing.T) {
	manifest, err := DefaultManifest()
	if err != nil {
		t.Fatalf("DefaultManifest: %v", err)
	}
	want := map[string]string{"python": "python/bin/python3.12", "javascript": "node-v26.13.0-linux-x64/bin/node", "go": "go/bin/go"}
	got := manifest.IDs()
	if len(got) != len(want) {
		t.Fatalf("IDs = %v, want %d runtimes", got, len(want))
	}
	for id, executable := range want {
		runtime, ok := manifest.Lookup(id)
		if !ok {
			t.Errorf("runtime %q is not pinned", id)
			continue
		}
		if runtime.Executable != executable {
			t.Errorf("%s executable = %q, want %q", id, runtime.Executable, executable)
		}
		if !strings.HasSuffix(runtime.Archive, ".tar.gz") || !sha256HexPattern.MatchString(runtime.SHA256) {
			t.Errorf("%s archive %q or digest %q is not well formed", id, runtime.Archive, runtime.SHA256)
		}
	}
}

func TestManifestAccessorsReturnCopies(t *testing.T) {
	manifest, err := DefaultManifest()
	if err != nil {
		t.Fatal(err)
	}
	runtime, _ := manifest.Lookup("javascript")
	runtime.Argv[0] = "mutated"
	runtime.Environment["OPENSSL_CONF"] = "mutated"
	again, _ := manifest.Lookup("javascript")
	if again.Argv[0] != PlaceholderExecutable || again.Environment["OPENSSL_CONF"] != "/dev/null" {
		t.Fatalf("Lookup exposed mutable manifest data: %+v", again)
	}
	all := manifest.Runtimes()
	all[0].Version = "mutated"
	if manifest.Runtimes()[0].Version == "mutated" {
		t.Fatal("Runtimes exposed mutable manifest data")
	}
	if _, ok := manifest.Lookup("ruby"); ok {
		t.Fatal("Lookup found a runtime that is not pinned")
	}
}

func validRuntime() Runtime {
	return Runtime{
		ID: "python", Version: "3.12.1", Archive: "py-3.12.1.tar.gz", SHA256: strings.Repeat("a", 64),
		Executable: "python/bin/python3", Argv: []string{PlaceholderExecutable, "-I", PlaceholderSource},
		Environment: map[string]string{"PYTHONIOENCODING": "utf-8"},
	}
}

func manifestWith(t *testing.T, mutate func(*Runtime)) []byte {
	t.Helper()
	runtime := validRuntime()
	mutate(&runtime)
	data, err := json.Marshal(manifestFile{Runtimes: []Runtime{runtime}})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func duplicateRuntimeManifest(t *testing.T) []byte {
	t.Helper()
	data, err := json.Marshal(manifestFile{Runtimes: []Runtime{validRuntime(), validRuntime()}})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestParseManifestRejectsMalformedInput(t *testing.T) {
	valid := manifestWith(t, func(*Runtime) {})
	cases := []struct {
		name string
		data []byte
	}{
		{name: "empty input", data: nil},
		{name: "not json", data: []byte("runtimes: []")},
		{name: "no runtimes", data: []byte(`{"runtimes":[]}`)},
		{name: "unknown field", data: []byte(strings.Replace(string(valid), `"id"`, `"extra":1,"id"`, 1))},
		{name: "unknown top-level field", data: []byte(`{"runtimes":[],"extra":1}`)},
		{name: "trailing content", data: append(append([]byte{}, valid...), []byte(`{}`)...)},
		{name: "oversized", data: append(append([]byte{}, valid...), []byte(strings.Repeat(" ", maxManifestBytes))...)},
		{name: "duplicate runtime", data: duplicateRuntimeManifest(t)},
		{name: "uppercase id", data: manifestWith(t, func(r *Runtime) { r.ID = "Python" })},
		{name: "id with a slash", data: manifestWith(t, func(r *Runtime) { r.ID = "py/thon" })},
		{name: "version with a slash", data: manifestWith(t, func(r *Runtime) { r.Version = "1/2" })},
		{name: "version starting with a dot", data: manifestWith(t, func(r *Runtime) { r.Version = ".1" })},
		{name: "archive with a path", data: manifestWith(t, func(r *Runtime) { r.Archive = "dir/py.tar.gz" })},
		{name: "archive not tar.gz", data: manifestWith(t, func(r *Runtime) { r.Archive = "py.zip" })},
		{name: "short digest", data: manifestWith(t, func(r *Runtime) { r.SHA256 = "abc" })},
		{name: "uppercase digest", data: manifestWith(t, func(r *Runtime) { r.SHA256 = strings.Repeat("A", 64) })},
		{name: "absolute executable", data: manifestWith(t, func(r *Runtime) { r.Executable = "/usr/bin/python3" })},
		{name: "traversing executable", data: manifestWith(t, func(r *Runtime) { r.Executable = "python/../../bin/sh" })},
		{name: "empty executable", data: manifestWith(t, func(r *Runtime) { r.Executable = "" })},
		{name: "dot executable", data: manifestWith(t, func(r *Runtime) { r.Executable = "." })},
		{name: "argv without executable first", data: manifestWith(t, func(r *Runtime) { r.Argv = []string{"-I", PlaceholderSource} })},
		{name: "argv with the executable twice", data: manifestWith(t, func(r *Runtime) {
			r.Argv = []string{PlaceholderExecutable, PlaceholderExecutable, PlaceholderSource}
		})},
		{name: "argv without a source", data: manifestWith(t, func(r *Runtime) { r.Argv = []string{PlaceholderExecutable, "-I"} })},
		{name: "argv with two sources", data: manifestWith(t, func(r *Runtime) {
			r.Argv = []string{PlaceholderExecutable, PlaceholderSource, PlaceholderSource}
		})},
		{name: "argv with an unknown placeholder", data: manifestWith(t, func(r *Runtime) {
			r.Argv = []string{PlaceholderExecutable, "{stdin}", PlaceholderSource}
		})},
		{name: "argv with an empty entry", data: manifestWith(t, func(r *Runtime) {
			r.Argv = []string{PlaceholderExecutable, "", PlaceholderSource}
		})},
		{name: "environment setting PATH", data: manifestWith(t, func(r *Runtime) { r.Environment = map[string]string{"PATH": "/usr/bin"} })},
		{name: "environment setting LD_PRELOAD", data: manifestWith(t, func(r *Runtime) { r.Environment = map[string]string{"LD_PRELOAD": "x.so"} })},
		{name: "environment lower-case key", data: manifestWith(t, func(r *Runtime) { r.Environment = map[string]string{"home": "x"} })},
		{name: "environment control character", data: manifestWith(t, func(r *Runtime) { r.Environment = map[string]string{"HOME": "a\nb"} })},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseManifest(tc.data)
			requireCode(t, err, ErrorCodeManifestInvalid)
		})
	}
}

func TestParseManifestAcceptsAValidRuntime(t *testing.T) {
	manifest, err := ParseManifest(manifestWith(t, func(*Runtime) {}))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if ids := manifest.IDs(); len(ids) != 1 || ids[0] != "python" {
		t.Fatalf("IDs = %v", ids)
	}
}

func FuzzParseManifest(f *testing.F) {
	f.Add(embeddedManifest)
	f.Add([]byte(`{"runtimes":[]}`))
	f.Add([]byte(`{"runtimes":[{"id":"a"}]}`))
	f.Add([]byte(`{"runtimes":null}`))
	f.Add([]byte(`[]`))
	f.Add([]byte(""))
	f.Fuzz(func(t *testing.T, data []byte) {
		manifest, err := ParseManifest(data)
		if err != nil {
			if _, ok := CodeOf(err); !ok {
				t.Fatalf("error without a code: %v", err)
			}
			return
		}
		for _, runtime := range manifest.Runtimes() {
			if problems := validateRuntime(&runtime); len(problems) != 0 {
				t.Fatalf("accepted runtime fails validation: %v", problems)
			}
		}
	})
}
