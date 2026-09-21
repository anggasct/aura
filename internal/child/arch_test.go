package child

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestPackageStaysOnPorts(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate package directory")
	}
	paths, err := filepath.Glob(filepath.Join(filepath.Dir(file), "*.go"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	denied := []string{
		"github.com/restatedev/sdk-go",
		"github.com/anggasct/aura/internal/model",
		"github.com/anggasct/aura/internal/tools",
		"github.com/anggasct/aura/internal/runtime/adk",
		"github.com/anggasct/aura/internal/runtime/engine",
		"github.com/anggasct/aura/internal/toolbroker",
	}
	set := token.NewFileSet()
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(set, path, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, imp := range parsed.Imports {
			target := strings.Trim(imp.Path.Value, `"`)
			for _, prefix := range denied {
				if target == prefix || strings.HasPrefix(target, prefix+"/") {
					t.Errorf("%s imports %s: depend on the durable port instead", filepath.Base(path), target)
				}
			}
		}
	}
}
