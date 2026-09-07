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

type cgroup struct {
	path string
}

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

func (c cgroup) attach(pid int) error {
	return c.writeFile("cgroup.procs", strconv.Itoa(pid))
}

func (c cgroup) destroy() error {
	return os.Remove(c.path)
}

func (c cgroup) writeFile(name, value string) error {
	return os.WriteFile(filepath.Join(c.path, name), []byte(value), 0o600)
}
