//go:build linux

package sandbox

import (
	"os"
	"strings"
	"testing"
)

func countOpenFDs(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skipf("cannot read /proc/self/fd: %v", err)
	}
	return len(entries)
}

func countAuraCgroups(t *testing.T) int {
	t.Helper()
	parent, ok := accountingCgroup()
	if !ok {
		t.Skip("cannot resolve the accounting cgroup")
	}
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Skipf("cannot read cgroup parent: %v", err)
	}
	count := 0
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "aura-sandbox-") {
			count++
		}
	}
	return count
}
