//go:build linux

package sandbox

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestCgroupLimitsAppliedOrFailsClosed(t *testing.T) {
	if !cgroupControllersWritable() {
		_, err := newCgroup(Limits{MemoryBytes: 1 << 20, MaxProcesses: 4})
		if code, ok := CodeOf(err); !ok || code != ErrorCodeSandboxInitFailed {
			t.Fatalf("newCgroup without delegated controllers = %v, want sandbox_init_failed", err)
		}
		t.Logf("controllers not delegated; fail-closed path asserted")
		return
	}

	cg, err := newCgroup(Limits{MemoryBytes: 2 << 20, MaxProcesses: 8})
	if err != nil {
		t.Fatalf("newCgroup: %v", err)
	}
	t.Cleanup(func() {
		if err := cg.destroy(); err != nil {
			t.Errorf("destroy cgroup: %v", err)
		}
	})

	for _, c := range []struct{ file, want string }{
		{"memory.max", strconv.FormatInt(2<<20, 10)},
		{"memory.swap.max", "0"},
		{"pids.max", strconv.FormatInt(8, 10)},
	} {
		got, err := os.ReadFile(filepath.Join(cg.path, c.file))
		if err != nil {
			t.Fatalf("read %s: %v", c.file, err)
		}
		if strings.TrimSpace(string(got)) != c.want {
			t.Errorf("%s = %q, want %q", c.file, strings.TrimSpace(string(got)), c.want)
		}
	}
}
