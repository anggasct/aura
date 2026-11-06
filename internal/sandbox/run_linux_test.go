package sandbox

import (
	"os"
	"syscall"
	"testing"
	"testing/fstest"
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

func TestPidNamespaceAvailability(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		want  bool
	}{
		{"enabled", map[string]string{"self/ns/pid": "", "sys/user/max_pid_namespaces": "63834\n"}, true},
		{"limit is zero", map[string]string{"self/ns/pid": "", "sys/user/max_pid_namespaces": "0\n"}, false},
		{"limit is unreadable", map[string]string{"self/ns/pid": ""}, false},
		{"limit is malformed", map[string]string{"self/ns/pid": "", "sys/user/max_pid_namespaces": "many"}, false},
		{"kernel without pid namespaces", map[string]string{"sys/user/max_pid_namespaces": "63834\n"}, false},
		{"empty proc", map[string]string{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			proc := fstest.MapFS{}
			for name, content := range tc.files {
				proc[name] = &fstest.MapFile{Data: []byte(content)}
			}
			if got := pidNamespaceAvailable(proc); got != tc.want {
				t.Fatalf("pidNamespaceAvailable = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestNegotiateReportsThePidNamespaceOfTheHost(t *testing.T) {
	primitives, err := Negotiate()
	if err != nil {
		t.Fatalf("Negotiate: %v", err)
	}
	if got, want := primitives.PidNamespace, pidNamespaceAvailable(os.DirFS(procRoot)); got != want {
		t.Fatalf("Negotiate pid namespace = %v, want %v", got, want)
	}
}

func TestChildSysProcAttrIsolatesTheProcessTree(t *testing.T) {
	attr := childSysProcAttr()
	for name, flag := range map[string]uintptr{
		"user": syscall.CLONE_NEWUSER, "pid": syscall.CLONE_NEWPID, "net": syscall.CLONE_NEWNET,
	} {
		if attr.Cloneflags&flag == 0 {
			t.Errorf("clone flags %#x lack the %s namespace", attr.Cloneflags, name)
		}
	}
	if !attr.Setpgid {
		t.Error("the child must lead its own process group")
	}
}
