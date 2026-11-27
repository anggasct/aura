package app

import (
	"os"
	"path/filepath"
	"testing"
)

func compareAppGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", "golden", name)
	if *updateGolden {
		if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if got != string(want) {
		t.Fatalf("%s differs from the output; rerun with -update to regenerate\n%s", path, firstLineDifference(got, string(want)))
	}
}
