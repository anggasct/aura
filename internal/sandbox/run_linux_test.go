package sandbox

import (
	"os"
	"testing"
)

func TestUsernsProbeLeavesTheProcessNamespaceUnchanged(t *testing.T) {
	before, err := os.Readlink("/proc/self/ns/user")
	if err != nil {
		t.Fatalf("user namespace link unreadable: %v", err)
	}
	for range 5 {
		usernsSyscallPermitted()
	}
	after, err := os.Readlink("/proc/self/ns/user")
	if err != nil || after != before {
		t.Fatalf("user namespace after probing = %q (%v), want %q", after, err, before)
	}
}

func TestLandlockDetectionFollowsTheKernelABI(t *testing.T) {
	abi, err := landlockABI()
	if got, want := landlockAvailable(), err == nil && abi >= 1; got != want {
		t.Fatalf("landlockAvailable = %v, want %v (abi %d, err %v)", got, want, abi, err)
	}
}
