//go:build linux

package sandbox

import (
	"crypto/rand"
	"errors"
	"io/fs"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
)

const (
	cgroupV2Root       = "/sys/fs/cgroup"
	supervisorLeaf     = "aura-supervisor"
	sandboxCgroupStem  = "aura-sandbox-"
	probeCgroupStem    = "aura-probe-"
	cgroupDirMode      = 0o700
	cgroupFileMode     = 0o600
	delegationRemedy   = "run aura server under a systemd unit with Delegate=yes"
	boundsEnableValue  = "+memory +pids"
	uniqueSuffixLength = 16
)

type cgroupRootMode string

const (
	rootPredelegated cgroupRootMode = "predelegated"
	rootPrepared     cgroupRootMode = "prepared"
	rootSelfLeaf     cgroupRootMode = "self_leaf"
)

type cgroupRoot struct {
	name string
	mode cgroupRootMode
}

type cgroupTree interface {
	ReadFile(name string) ([]byte, error)
	WriteFile(name string, data []byte, perm fs.FileMode) error
	Mkdir(name string, perm fs.FileMode) error
	Remove(name string) error
}

type mountedTree struct {
	mount string
}

func (m mountedTree) within(op func(root *os.Root) error) error {
	root, err := os.OpenRoot(m.mount)
	if err != nil {
		return err
	}
	opErr := op(root)
	return errors.Join(opErr, root.Close())
}

func (m mountedTree) ReadFile(name string) ([]byte, error) {
	var data []byte
	err := m.within(func(root *os.Root) error {
		var readErr error
		data, readErr = root.ReadFile(name)
		return readErr
	})
	return data, err
}

func (m mountedTree) WriteFile(name string, data []byte, perm fs.FileMode) error {
	return m.within(func(root *os.Root) error { return root.WriteFile(name, data, perm) })
}

func (m mountedTree) Mkdir(name string, perm fs.FileMode) error {
	return m.within(func(root *os.Root) error { return root.Mkdir(name, perm) })
}

func (m mountedTree) Remove(name string) error {
	return m.within(func(root *os.Root) error { return root.Remove(name) })
}

type cgroupHost struct {
	tree cgroupTree
	self func() (string, error)
	pid  int
}

func hostCgroups() *cgroupHost {
	return &cgroupHost{tree: mountedTree{mount: cgroupV2Root}, self: selfCgroup, pid: os.Getpid()}
}

func selfCgroup() (string, error) {
	data, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "", err
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		if rel, ok := strings.CutPrefix(line, "0::"); ok {
			return treeName(rel), nil
		}
	}
	return "", errors.New("no cgroup v2 entry for this process")
}

func treeName(rel string) string {
	trimmed := strings.Trim(path.Clean("/"+rel), "/")
	if trimmed == "" {
		return "."
	}
	return trimmed
}

func displayName(name string) string {
	if name == "." {
		return "/"
	}
	return path.Base(name)
}

func (h *cgroupHost) resolve() (cgroupRoot, error) {
	own, err := h.self()
	if err != nil {
		return cgroupRoot{}, Errorf(ErrorCodeSandboxUnavailable, "locate own cgroup: %v", pathlessCause(err))
	}
	if h.delegatesBounds(own) {
		return cgroupRoot{name: own, mode: rootPredelegated}, nil
	}
	if path.Base(own) == supervisorLeaf {
		if parent := path.Dir(own); h.delegatesBounds(parent) {
			return cgroupRoot{name: parent, mode: rootPrepared}, nil
		}
	}
	return cgroupRoot{}, Errorf(ErrorCodeSandboxUnavailable, "no delegated cgroup for sandbox bounds in %s: %s", displayName(own), delegationRemedy)
}

func (h *cgroupHost) available() (cgroupRoot, error) {
	root, err := h.resolve()
	if err != nil {
		return cgroupRoot{}, err
	}
	if err := h.probe(root); err != nil {
		return cgroupRoot{}, err
	}
	return root, nil
}

func (h *cgroupHost) prepare() (cgroupRoot, error) {
	if _, err := h.resolve(); err == nil {
		return h.available()
	}
	own, err := h.self()
	if err != nil {
		return cgroupRoot{}, Errorf(ErrorCodeSandboxUnavailable, "locate own cgroup: %v", pathlessCause(err))
	}
	if err := h.requireSoleOccupant(own); err != nil {
		return cgroupRoot{}, err
	}
	if err := h.moveIntoSupervisorThenEnable(own); err != nil {
		return cgroupRoot{}, err
	}
	root := cgroupRoot{name: own, mode: rootSelfLeaf}
	if err := h.probe(root); err != nil {
		return cgroupRoot{}, err
	}
	return root, nil
}

func (h *cgroupHost) requireSoleOccupant(own string) error {
	controllers, err := h.tree.ReadFile(path.Join(own, "cgroup.controllers"))
	if err != nil || !offersBounds(controllers) {
		return Errorf(ErrorCodeSandboxUnavailable, "cgroup %s does not offer memory and pids controllers: %s", displayName(own), delegationRemedy)
	}
	procs, err := h.tree.ReadFile(path.Join(own, "cgroup.procs"))
	if err != nil {
		return Errorf(ErrorCodeSandboxUnavailable, "read processes of cgroup %s: %v", displayName(own), pathlessCause(err))
	}
	if !slices.Equal(strings.Fields(string(procs)), []string{strconv.Itoa(h.pid)}) {
		return Errorf(ErrorCodeSandboxUnavailable, "cgroup %s holds other processes: %s", displayName(own), delegationRemedy)
	}
	return nil
}

func (h *cgroupHost) moveIntoSupervisorThenEnable(own string) error {
	leaf := path.Join(own, supervisorLeaf)
	if err := h.tree.Mkdir(leaf, cgroupDirMode); err != nil {
		return Errorf(ErrorCodeSandboxUnavailable, "create supervisor cgroup in %s: %v", displayName(own), pathlessCause(err))
	}
	pid := strconv.Itoa(h.pid)
	if err := h.write(path.Join(leaf, "cgroup.procs"), pid); err != nil {
		rollbackErr := h.tree.Remove(leaf)
		return Errorf(ErrorCodeSandboxUnavailable, "move service into supervisor cgroup in %s: %v", displayName(own), pathlessCause(err, rollbackErr))
	}
	if err := h.write(path.Join(own, "cgroup.subtree_control"), boundsEnableValue); err != nil {
		rollbackErr := errors.Join(h.write(path.Join(own, "cgroup.procs"), pid), h.tree.Remove(leaf))
		return Errorf(ErrorCodeSandboxUnavailable, "enable bounds controllers in %s: %v", displayName(own), pathlessCause(err, rollbackErr))
	}
	return nil
}

func (h *cgroupHost) probe(root cgroupRoot) error {
	dir, err := h.mkdirUnique(root.name, probeCgroupStem)
	if err != nil {
		return Errorf(ErrorCodeSandboxUnavailable, "create probe cgroup in %s: %v", displayName(root.name), pathlessCause(err))
	}
	defer func() { _ = h.tree.Remove(dir) }()
	for _, file := range []string{"memory.max", "memory.swap.max", "pids.max"} {
		if err := h.write(path.Join(dir, file), "max"); err != nil {
			return Errorf(ErrorCodeSandboxUnavailable, "write %s in probe cgroup under %s: %v", file, displayName(root.name), pathlessCause(err))
		}
	}
	return nil
}

func (h *cgroupHost) accounting() (string, error) {
	if root, err := h.resolve(); err == nil {
		return root.name, nil
	}
	return h.self()
}

func (h *cgroupHost) delegatesBounds(dir string) bool {
	data, err := h.tree.ReadFile(path.Join(dir, "cgroup.subtree_control"))
	return err == nil && offersBounds(data)
}

func offersBounds(controllers []byte) bool {
	fields := strings.Fields(string(controllers))
	return slices.Contains(fields, "memory") && slices.Contains(fields, "pids")
}

func (h *cgroupHost) mkdirUnique(parent, stem string) (string, error) {
	name := path.Join(parent, stem+strings.ToLower(rand.Text()[:uniqueSuffixLength]))
	if err := h.tree.Mkdir(name, cgroupDirMode); err != nil {
		return "", err
	}
	return name, nil
}

func (h *cgroupHost) write(name, value string) error {
	return h.tree.WriteFile(name, []byte(value), cgroupFileMode)
}

func pathlessCause(errs ...error) error {
	causes := make([]error, 0, len(errs))
	for _, err := range errs {
		var pathErr *fs.PathError
		if errors.As(err, &pathErr) {
			causes = append(causes, pathErr.Err)
			continue
		}
		causes = append(causes, err)
	}
	return errors.Join(causes...)
}

type cgroup struct {
	host *cgroupHost
	name string
}

func (h *cgroupHost) newCgroup(limits Limits) (cgroup, error) {
	root, err := h.resolve()
	if err != nil {
		return cgroup{}, Errorf(ErrorCodeSandboxInitFailed, "resolve sandbox cgroup root: %v", err)
	}
	name, err := h.mkdirUnique(root.name, sandboxCgroupStem)
	if err != nil {
		return cgroup{}, Errorf(ErrorCodeSandboxInitFailed, "create child cgroup: %v", pathlessCause(err))
	}
	cg := cgroup{host: h, name: name}
	if err := cg.apply(limits); err != nil {
		_ = cg.destroy()
		return cgroup{}, err
	}
	return cg, nil
}

func (c cgroup) apply(limits Limits) error {
	var problems []error
	if limits.MemoryBytes > 0 {
		problems = append(problems,
			c.writeFile("memory.max", strconv.FormatInt(limits.MemoryBytes, 10)),
			c.writeFile("memory.swap.max", "0"),
		)
	}
	if limits.MaxProcesses > 0 {
		problems = append(problems, c.writeFile("pids.max", strconv.FormatInt(limits.MaxProcesses, 10)))
	}
	if err := pathlessCause(problems...); err != nil {
		return Errorf(ErrorCodeSandboxInitFailed, "write cgroup limits: %v", err)
	}
	return nil
}

func (c cgroup) attach(pid int) error {
	return c.writeFile("cgroup.procs", strconv.Itoa(pid))
}

func (c cgroup) destroy() error {
	return c.host.tree.Remove(c.name)
}

func (c cgroup) writeFile(name, value string) error {
	return c.host.write(path.Join(c.name, name), value)
}

func prepareHost() (HostPreparation, error) {
	root, err := hostCgroups().prepare()
	if err != nil {
		return HostPreparation{}, err
	}
	return HostPreparation{CgroupRoot: displayName(root.name), Mode: string(root.mode)}, nil
}

func accountingCgroup() (string, bool) {
	name, err := hostCgroups().accounting()
	if err != nil {
		return "", false
	}
	return path.Join(cgroupV2Root, name), true
}
