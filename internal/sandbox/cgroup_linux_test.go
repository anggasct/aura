//go:build linux

package sandbox

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	fixturePID   = 4242
	fixtureOwn   = "user.slice/app.slice/aura.service"
	fixtureOther = "4343"
)

type recordedWrite struct {
	name  string
	value string
}

type fixtureTree struct {
	mountedTree
	current string
	writes  []recordedWrite
	failOn  string
}

func (f *fixtureTree) WriteFile(name string, data []byte, perm fs.FileMode) error {
	value := string(data)
	f.writes = append(f.writes, recordedWrite{name: name, value: value})
	if name == f.failOn {
		return &fs.PathError{Op: "write", Path: name, Err: errors.New("device or resource busy")}
	}
	switch path.Base(name) {
	case "cgroup.subtree_control":
		value = strings.ReplaceAll(value, "+", "")
	case "cgroup.procs":
		if value == strconv.Itoa(fixturePID) {
			if err := f.mountedTree.WriteFile(path.Join(f.current, "cgroup.procs"), nil, perm); err != nil {
				return err
			}
			f.current = path.Dir(name)
		}
	}
	return f.mountedTree.WriteFile(name, []byte(value), perm)
}

func (f *fixtureTree) Remove(name string) error {
	return f.within(func(root *os.Root) error { return root.RemoveAll(name) })
}

func (f *fixtureTree) controlWrites() []recordedWrite {
	var control []recordedWrite
	for _, w := range f.writes {
		if !strings.Contains(w.name, probeCgroupStem) && !strings.Contains(w.name, sandboxCgroupStem) {
			control = append(control, w)
		}
	}
	return control
}

func newFixture(t *testing.T, self string, files map[string]string) (*cgroupHost, *fixtureTree) {
	t.Helper()
	mount := t.TempDir()
	for name, content := range files {
		full := filepath.Join(mount, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatalf("mkdir fixture: %v", err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatalf("write fixture: %v", err)
		}
	}
	tree := &fixtureTree{mountedTree: mountedTree{mount: mount}, current: self}
	host := &cgroupHost{tree: tree, self: func() (string, error) { return tree.current, nil }, pid: fixturePID}
	return host, tree
}

func undelegatedUnit(procs string) map[string]string {
	return map[string]string{
		"user.slice/app.slice/cgroup.subtree_control": "cpu memory pids",
		fixtureOwn + "/cgroup.controllers":            "cpu memory pids",
		fixtureOwn + "/cgroup.subtree_control":        "",
		fixtureOwn + "/cgroup.procs":                  procs,
	}
}

func entriesWithPrefix(t *testing.T, tree *fixtureTree, dir, prefix string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(tree.mount, filepath.FromSlash(dir)))
	if err != nil {
		t.Fatalf("read fixture dir: %v", err)
	}
	var names []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), prefix) {
			names = append(names, entry.Name())
		}
	}
	return names
}

func readFixture(t *testing.T, tree *fixtureTree, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(tree.mount, filepath.FromSlash(name)))
	if err != nil {
		t.Fatalf("read fixture file: %v", err)
	}
	return strings.TrimSpace(string(data))
}

func TestCgroupRootPrefersPredelegatedOwnCgroup(t *testing.T) {
	host, tree := newFixture(t, fixtureOwn, map[string]string{
		fixtureOwn + "/cgroup.controllers":     "cpu memory pids",
		fixtureOwn + "/cgroup.subtree_control": "memory pids",
		fixtureOwn + "/cgroup.procs":           "",
	})
	root, err := host.resolve()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if root.name != fixtureOwn || root.mode != rootPredelegated {
		t.Fatalf("resolve = %+v, want own cgroup predelegated", root)
	}
	if len(tree.writes) != 0 {
		t.Fatalf("resolve issued writes: %+v", tree.writes)
	}
	if got := entriesWithPrefix(t, tree, fixtureOwn, supervisorLeaf); len(got) != 0 {
		t.Fatalf("supervisor leaf created for a predelegated cgroup: %v", got)
	}
}

func TestPrepareHostMovesSelfIntoLeaf(t *testing.T) {
	host, tree := newFixture(t, fixtureOwn, undelegatedUnit(strconv.Itoa(fixturePID)))
	root, err := host.prepare()
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if root.name != fixtureOwn || root.mode != rootSelfLeaf {
		t.Fatalf("prepare = %+v, want own cgroup self_leaf", root)
	}
	want := []recordedWrite{
		{name: fixtureOwn + "/" + supervisorLeaf + "/cgroup.procs", value: strconv.Itoa(fixturePID)},
		{name: fixtureOwn + "/cgroup.subtree_control", value: boundsEnableValue},
	}
	if got := tree.controlWrites(); !slices.Equal(got, want) {
		t.Fatalf("control writes = %+v, want %+v", got, want)
	}
	var probed []string
	for _, w := range tree.writes {
		if strings.Contains(w.name, probeCgroupStem) {
			probed = append(probed, path.Base(w.name))
		}
	}
	if !slices.Equal(probed, []string{"memory.max", "memory.swap.max", "pids.max"}) {
		t.Fatalf("probe writes = %v, want memory.max, memory.swap.max, pids.max", probed)
	}
	if got := entriesWithPrefix(t, tree, fixtureOwn, probeCgroupStem); len(got) != 0 {
		t.Fatalf("probe cgroup left behind: %v", got)
	}
	if got := readFixture(t, tree, fixtureOwn+"/"+supervisorLeaf+"/cgroup.procs"); got != strconv.Itoa(fixturePID) {
		t.Fatalf("supervisor procs = %q, want the service pid", got)
	}
}

func TestPrepareHostFailsClosedWhenUnitShared(t *testing.T) {
	host, tree := newFixture(t, fixtureOwn, undelegatedUnit(strconv.Itoa(fixturePID)+"\n"+fixtureOther))
	_, err := host.prepare()
	if code, ok := CodeOf(err); !ok || code != ErrorCodeSandboxUnavailable {
		t.Fatalf("prepare = %v, want sandbox_unavailable", err)
	}
	detail := errorDetail(err)
	for _, want := range []string{"aura.service", "Delegate=yes"} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail %q lacks %q", detail, want)
		}
	}
	for _, leaked := range []string{"user.slice", tree.mount} {
		if strings.Contains(detail, leaked) {
			t.Errorf("detail %q leaks path fragment %q", detail, leaked)
		}
	}
	if len(tree.writes) != 0 {
		t.Fatalf("prepare wrote to a shared cgroup: %+v", tree.writes)
	}
	requireErr := Require(Primitives{UserNamespace: true, Seccomp: true, Landlock: true, ProcessGroups: true})
	if code, ok := CodeOf(requireErr); !ok || code != ErrorCodeSandboxUnavailable {
		t.Fatalf("Require without cgroup v2 = %v, want sandbox_unavailable", requireErr)
	}
}

func TestPrepareHostRollsBackOnEnableFailure(t *testing.T) {
	host, tree := newFixture(t, fixtureOwn, undelegatedUnit(strconv.Itoa(fixturePID)))
	tree.failOn = fixtureOwn + "/cgroup.subtree_control"
	_, err := host.prepare()
	if code, ok := CodeOf(err); !ok || code != ErrorCodeSandboxUnavailable {
		t.Fatalf("prepare = %v, want sandbox_unavailable", err)
	}
	if !strings.Contains(errorDetail(err), "enable bounds controllers") {
		t.Fatalf("detail %q does not name the failing step", errorDetail(err))
	}
	if strings.Contains(errorDetail(err), "user.slice") {
		t.Fatalf("detail %q leaks the cgroup path", errorDetail(err))
	}
	want := []recordedWrite{
		{name: fixtureOwn + "/" + supervisorLeaf + "/cgroup.procs", value: strconv.Itoa(fixturePID)},
		{name: fixtureOwn + "/cgroup.subtree_control", value: boundsEnableValue},
		{name: fixtureOwn + "/cgroup.procs", value: strconv.Itoa(fixturePID)},
	}
	if got := tree.controlWrites(); !slices.Equal(got, want) {
		t.Fatalf("control writes = %+v, want %+v", got, want)
	}
	if got := entriesWithPrefix(t, tree, fixtureOwn, supervisorLeaf); len(got) != 0 {
		t.Fatalf("supervisor leaf not removed on rollback: %v", got)
	}
	if tree.current != fixtureOwn {
		t.Fatalf("service left in %q after rollback, want %q", tree.current, fixtureOwn)
	}
}

func TestCgroupRootNeverMigratesOutsideServer(t *testing.T) {
	host, tree := newFixture(t, fixtureOwn, undelegatedUnit(strconv.Itoa(fixturePID)))
	if _, err := host.available(); err == nil {
		t.Fatal("available succeeded on an undelegated cgroup")
	} else if code, ok := CodeOf(err); !ok || code != ErrorCodeSandboxUnavailable {
		t.Fatalf("available = %v, want sandbox_unavailable", err)
	}
	if _, err := host.newCgroup(Limits{MemoryBytes: 1 << 20}); err == nil {
		t.Fatal("newCgroup succeeded on an undelegated cgroup")
	}
	if len(tree.writes) != 0 {
		t.Fatalf("non-server resolution wrote: %+v", tree.writes)
	}
	if got := entriesWithPrefix(t, tree, fixtureOwn, supervisorLeaf); len(got) != 0 {
		t.Fatalf("non-server resolution created a supervisor leaf: %v", got)
	}
}

func TestCgroupRootRecognisesPreparedLeaf(t *testing.T) {
	host, tree := newFixture(t, fixtureOwn+"/"+supervisorLeaf, map[string]string{
		fixtureOwn + "/cgroup.controllers":                            "cpu memory pids",
		fixtureOwn + "/cgroup.subtree_control":                        "memory pids",
		fixtureOwn + "/cgroup.procs":                                  "",
		fixtureOwn + "/" + supervisorLeaf + "/cgroup.subtree_control": "",
		fixtureOwn + "/" + supervisorLeaf + "/cgroup.procs":           strconv.Itoa(fixturePID),
	})
	root, err := host.resolve()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if root.name != fixtureOwn || root.mode != rootPrepared {
		t.Fatalf("resolve = %+v, want parent prepared", root)
	}
	before, err := os.Stat(filepath.Join(tree.mount, fixtureOwn, "cgroup.subtree_control"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if _, err := host.prepare(); err != nil {
		t.Fatalf("second prepare: %v", err)
	}
	if got := tree.controlWrites(); len(got) != 0 {
		t.Fatalf("second prepare rewrote control files: %+v", got)
	}
	after, err := os.Stat(filepath.Join(tree.mount, fixtureOwn, "cgroup.subtree_control"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("second prepare touched subtree_control")
	}
}

func TestNewCgroupCreatesUnderResolvedRoot(t *testing.T) {
	host, tree := newFixture(t, fixtureOwn, undelegatedUnit(strconv.Itoa(fixturePID)))
	if _, err := host.prepare(); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	cg, err := host.newCgroup(Limits{MemoryBytes: 2 << 20, MaxProcesses: 8})
	if err != nil {
		t.Fatalf("newCgroup: %v", err)
	}
	if path.Dir(cg.name) != fixtureOwn {
		t.Fatalf("child cgroup %q is not a sibling of the supervisor leaf", cg.name)
	}
	for _, c := range []struct{ file, want string }{
		{"memory.max", strconv.FormatInt(2<<20, 10)},
		{"memory.swap.max", "0"},
		{"pids.max", "8"},
	} {
		if got := readFixture(t, tree, cg.name+"/"+c.file); got != c.want {
			t.Errorf("%s = %q, want %q", c.file, got, c.want)
		}
	}
	if err := cg.destroy(); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if got := entriesWithPrefix(t, tree, fixtureOwn, sandboxCgroupStem); len(got) != 0 {
		t.Fatalf("child cgroup left after destroy: %v", got)
	}
}

func TestNewCgroupFailsClosedWithoutDelegatedRoot(t *testing.T) {
	host, tree := newFixture(t, fixtureOwn, undelegatedUnit(strconv.Itoa(fixturePID)))
	_, err := host.newCgroup(Limits{MemoryBytes: 2 << 20, MaxProcesses: 8})
	if code, ok := CodeOf(err); !ok || code != ErrorCodeSandboxInitFailed {
		t.Fatalf("newCgroup = %v, want sandbox_init_failed", err)
	}
	if got := entriesWithPrefix(t, tree, fixtureOwn, sandboxCgroupStem); len(got) != 0 {
		t.Fatalf("child cgroup created without a delegated root: %v", got)
	}
}

func TestCgroupLimitsAppliedOrFailsClosed(t *testing.T) {
	host := hostCgroups()
	if _, err := host.available(); err != nil {
		_, err := host.newCgroup(Limits{MemoryBytes: 1 << 20, MaxProcesses: 4})
		if code, ok := CodeOf(err); !ok || code != ErrorCodeSandboxInitFailed {
			t.Fatalf("newCgroup without delegated controllers = %v, want sandbox_init_failed", err)
		}
		t.Logf("controllers not delegated; fail-closed path asserted")
		return
	}

	cg, err := host.newCgroup(Limits{MemoryBytes: 2 << 20, MaxProcesses: 8})
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
		got, err := host.tree.ReadFile(path.Join(cg.name, c.file))
		if err != nil {
			t.Fatalf("read %s: %v", c.file, err)
		}
		if strings.TrimSpace(string(got)) != c.want {
			t.Errorf("%s = %q, want %q", c.file, strings.TrimSpace(string(got)), c.want)
		}
	}
}

func TestRunFailsClosedWhenCgroupCreationFails(t *testing.T) {
	host, _ := newFixture(t, fixtureOwn, undelegatedUnit(strconv.Itoa(fixturePID)))
	t.Run("run", func(t *testing.T) {
		sentinel := filepath.Join(t.TempDir(), "ran")
		spec := Spec{WorkingDir: t.TempDir(), Limits: Limits{Timeout: 5 * time.Second}}
		_, err := runWithCgroups(t.Context(), host, &spec, "/usr/bin/touch", sentinel)
		if code, ok := CodeOf(err); !ok || code != ErrorCodeSandboxInitFailed {
			t.Fatalf("runWithCgroups = %v, want sandbox_init_failed", err)
		}
		if _, statErr := os.Stat(sentinel); !errors.Is(statErr, fs.ErrNotExist) {
			t.Fatalf("command executed without a cgroup: %v", statErr)
		}
	})
	t.Run("session", func(t *testing.T) {
		sentinel := filepath.Join(t.TempDir(), "ran")
		req := testSessionRequest(t)
		req.Executable = "/usr/bin/touch"
		req.Arguments = []string{sentinel}
		_, err := startSessionWithCgroups(t.Context(), host, req, req.spec())
		if code, ok := CodeOf(err); !ok || code != ErrorCodeSandboxInitFailed {
			t.Fatalf("startSessionWithCgroups = %v, want sandbox_init_failed", err)
		}
		if _, statErr := os.Stat(sentinel); !errors.Is(statErr, fs.ErrNotExist) {
			t.Fatalf("session child executed without a cgroup: %v", statErr)
		}
	})
}

func TestAccountingCgroupUsesPreparedRoot(t *testing.T) {
	t.Run("prepared", func(t *testing.T) {
		host, _ := newFixture(t, fixtureOwn, undelegatedUnit(strconv.Itoa(fixturePID)))
		if _, err := host.prepare(); err != nil {
			t.Fatalf("prepare: %v", err)
		}
		got, err := host.accounting()
		if err != nil || got != fixtureOwn {
			t.Fatalf("accounting = %q, %v, want %q", got, err, fixtureOwn)
		}
	})
	t.Run("unprepared", func(t *testing.T) {
		host, _ := newFixture(t, fixtureOwn, undelegatedUnit(strconv.Itoa(fixturePID)))
		got, err := host.accounting()
		if err != nil || got != fixtureOwn {
			t.Fatalf("accounting = %q, %v, want own cgroup %q", got, err, fixtureOwn)
		}
	})
}

func TestTreeNameNormalisesKernelPaths(t *testing.T) {
	cases := []struct{ name, rel, want string }{
		{"root", "/", "."},
		{"unit", "/user.slice/app.slice/aura.service", "user.slice/app.slice/aura.service"},
		{"dotted", "/a/../b", "b"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := treeName(tc.rel); got != tc.want {
				t.Fatalf("treeName(%q) = %q, want %q", tc.rel, got, tc.want)
			}
		})
	}
}
