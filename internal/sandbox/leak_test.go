//go:build linux

package sandbox

import (
	"os"
	"strings"
	"testing"
)

// countOpenFDs is the descriptor-leak probe shared by the one-shot and
// session teardown harnesses.
func countOpenFDs(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skipf("cannot read /proc/self/fd: %v", err)
	}
	return len(entries)
}

// countAuraCgroups is the cgroup-leak probe shared by the one-shot and
// session teardown harnesses.
func countAuraCgroups(t *testing.T) int {
	t.Helper()
	parent, err := ownCgroupPath()
	if err != nil {
		t.Skipf("cannot resolve own cgroup: %v", err)
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
