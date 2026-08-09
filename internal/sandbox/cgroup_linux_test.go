//go:build linux

package sandbox

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// cgroup controller delegation is a runtime condition, not a guarantee: a
// process can often mkdir a child cgroup but still be denied writes to the
// controller files unless its scope delegated them. Where writes succeed,
// exercise the real apply/destroy path; where they do not, assert that
// newCgroup fails closed with sandbox_init_failed rather than skipping.
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

// cgroupControllersWritable reports whether the service can write controller
// limits into a child of its own cgroup — the actual precondition for cgroup
// enforcement, stronger than being able to create the directory.
func cgroupControllersWritable() bool {
	parent, err := ownCgroupPath()
	if err != nil {
		return false
	}
	dir, err := os.MkdirTemp(parent, "aura-probe-*")
	if err != nil {
		return false
	}
	defer os.Remove(dir)
	return os.WriteFile(filepath.Join(dir, "pids.max"), []byte("8"), 0o644) == nil
}
