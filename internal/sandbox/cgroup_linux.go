//go:build linux

package sandbox

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const cgroupV2Root = "/sys/fs/cgroup"

// cgroup is a cgroup v2 subtree the sandbox created for one child process.
// A zero value must not be used; construct one with newCgroup and destroy it
// after the child is reaped.
type cgroup struct {
	path string
}

// cgroupControllersWritable reports whether the service can write the
// controller limits the sandbox applies — pids and memory — into a child of
// its own cgroup. A process can often mkdir a child cgroup but still be denied
// particular controller files unless its scope delegated them, so every
// controller the feature uses is probed rather than just one.
func cgroupControllersWritable() bool {
	parent, err := ownCgroupPath()
	if err != nil {
		return false
	}
	dir, err := os.MkdirTemp(parent, "aura-probe-*")
	if err != nil {
		return false
	}
	defer func() { _ = os.Remove(dir) }()
	for _, probe := range []string{"pids.max", "memory.max"} {
		if os.WriteFile(filepath.Join(dir, probe), []byte("max"), 0o600) != nil {
			return false
		}
	}
	return true
}

// ownCgroupPath returns the absolute path of the calling process's current
// cgroup v2. The service must run under a delegated subtree to create child
// cgroups here without privileges.
func ownCgroupPath() (string, error) {
	data, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		rel, ok := strings.CutPrefix(line, "0::")
		if !ok {
			continue
		}
		return filepath.Join(cgroupV2Root, rel), nil
	}
	return "", errors.New("no cgroup v2 entry for this process")
}

// newCgroup creates a uniquely-named child cgroup under the service's
// current hierarchy and applies the resource limits the kernel enforces for
// memory and process count. A creation failure — the hierarchy is not
// delegated for writing — is sandbox_init_failed so the run refuses rather
// than executing an unbounded child.
func newCgroup(limits Limits) (cgroup, error) {
	parent, err := ownCgroupPath()
	if err != nil {
		return cgroup{}, Errorf(ErrorCodeSandboxInitFailed, "locate service cgroup: %v", err)
	}
	dir, err := os.MkdirTemp(parent, "aura-sandbox-*")
	if err != nil {
		return cgroup{}, Errorf(ErrorCodeSandboxInitFailed, "create child cgroup: %v", err)
	}
	cg := cgroup{path: dir}
	if err := cg.apply(limits); err != nil {
		_ = cg.destroy()
		return cgroup{}, err
	}
	return cg, nil
}

func (c cgroup) apply(limits Limits) error {
	var problems []error
	if limits.MemoryBytes > 0 {
		// Disable swap so a child cannot page its way past the memory ceiling.
		problems = append(problems,
			c.writeFile("memory.max", strconv.FormatInt(limits.MemoryBytes, 10)),
			c.writeFile("memory.swap.max", "0"),
		)
	}
	if limits.MaxProcesses > 0 {
		problems = append(problems, c.writeFile("pids.max", strconv.FormatInt(limits.MaxProcesses, 10)))
	}
	if err := errors.Join(problems...); err != nil {
		return Errorf(ErrorCodeSandboxInitFailed, "write cgroup limits: %v", err)
	}
	return nil
}

// attach moves pid into the cgroup so its descendants are bounded too.
func (c cgroup) attach(pid int) error {
	return c.writeFile("cgroup.procs", strconv.Itoa(pid))
}

// destroy removes the cgroup. It must be called only after every process has
// exited the subtree, otherwise the kernel reports it busy.
func (c cgroup) destroy() error {
	return os.Remove(c.path)
}

func (c cgroup) writeFile(name, value string) error {
	return os.WriteFile(filepath.Join(c.path, name), []byte(value), 0o600)
}
