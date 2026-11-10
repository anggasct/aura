package toolsbuiltin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCliWiresBuiltinToolsOnlyThroughThisPackage(t *testing.T) {
	forbiddenImports := []string{
		"github.com/anggasct/aura/internal/platform/tools",
		"github.com/anggasct/aura/internal/modules/shellexec",
		"github.com/anggasct/aura/internal/modules/fetch",
		"github.com/anggasct/aura/internal/modules/filesystem",
		"github.com/anggasct/aura/internal/modules/search",
	}
	forbiddenCalls := []string{"toolbroker.New(", "effect.NewExecutor("}
	for _, dir := range []string{"app", "cli"} {
		err := filepath.WalkDir(filepath.Join("..", "..", dir), func(path string, entry os.DirEntry, walkErr error) error {
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
			t.Fatalf("scan internal/app and internal/cli: %v", err)
		}
	}
}
