package toolsbuiltin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The cli composition root must wire builtin tools exclusively through this
// package: no direct adapter construction and no broker or effect-executor
// assembly outside New.
func TestCliWiresBuiltinToolsOnlyThroughThisPackage(t *testing.T) {
	forbiddenImports := []string{
		"github.com/anggasct/aura/internal/tools",
		"github.com/anggasct/aura/internal/tools/exec",
		"github.com/anggasct/aura/internal/tools/fetch",
		"github.com/anggasct/aura/internal/tools/filesystem",
		"github.com/anggasct/aura/internal/tools/search",
	}
	forbiddenCalls := []string{"toolbroker.New(", "effect.NewExecutor("}
	err := filepath.WalkDir(filepath.Join("..", "..", "cli"), func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		source := string(content)
		for _, imported := range forbiddenImports {
			if strings.Contains(source, "\""+imported+"\"") {
				t.Errorf("%s imports %s: tool adapters are assembled only in this package", path, imported)
			}
		}
		for _, call := range forbiddenCalls {
			if strings.Contains(source, call) {
				t.Errorf("%s contains %s: broker and effect assembly live in this package's constructor", path, call)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scan internal/cli: %v", err)
	}
}
