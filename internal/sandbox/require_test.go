package sandbox

import (
	"strings"
	"testing"
)

// Require is the fail-closed gate for an effectful capability: any missing
// mandatory primitive makes containment unavailable, and the error must name
// every absent primitive so the status surface can report the exact cause.
func TestRequire(t *testing.T) {
	allPresent := Primitives{UserNamespace: true, Seccomp: true, CgroupV2: true, Landlock: true, ProcessGroups: true}
	if err := Require(allPresent); err != nil {
		t.Fatalf("Require(all present) = %v, want nil", err)
	}

	// A single absent mandatory primitive fails closed and is named.
	cases := []struct {
		name    string
		have    Primitives
		missing string
	}{
		{"user namespace absent", Primitives{Seccomp: true, CgroupV2: true, Landlock: true, ProcessGroups: true}, "user_namespace"},
		{"landlock absent", Primitives{UserNamespace: true, Seccomp: true, CgroupV2: true, ProcessGroups: true}, "landlock"},
		{"seccomp absent", Primitives{UserNamespace: true, CgroupV2: true, Landlock: true, ProcessGroups: true}, "seccomp"},
		{"cgroup v2 absent", Primitives{UserNamespace: true, Seccomp: true, Landlock: true, ProcessGroups: true}, "cgroup_v2"},
		{"process groups absent", Primitives{UserNamespace: true, Seccomp: true, CgroupV2: true, Landlock: true}, "process_groups"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Require(tc.have)
			code, ok := CodeOf(err)
			if !ok || code != ErrorCodeSandboxUnavailable {
				t.Fatalf("Require = %v, want sandbox_unavailable", err)
			}
			if !strings.Contains(err.Error(), tc.missing) {
				t.Fatalf("Require error %q must name %q", err, tc.missing)
			}
		})
	}

	// Every primitive absent lists them all, so a host that can provide none
	// reports the full set rather than only the first.
	err := Require(Primitives{})
	if err == nil {
		t.Fatal("Require(empty) = nil, want sandbox_unavailable")
	}
	for _, want := range []string{"user_namespace", "landlock", "seccomp", "cgroup_v2", "process_groups"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("Require(empty) error %q must name %q", err, want)
		}
	}
}
