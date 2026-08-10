//go:build linux

package sandbox

import (
	"golang.org/x/sys/unix"
)

// rlimitSetting is one resource limit to apply in the sandbox child.
type rlimitSetting struct {
	resource int
	soft     uint64
	hard     uint64
}

// computeRlimits maps the sandbox Limits to per-process rlimit tuples. A zero
// field leaves that resource unlimited (the kernel default); the only always
// applied limit is RLIMIT_CORE, which is forced to MaxCoreSize so a crash
// never leaves a core dump carrying workspace content. Memory is enforced by
// the cgroup adapter, not RLIMIT_AS, which is unreliable under the Go
// runtime's large virtual-memory reservation.
func computeRlimits(limits Limits) []rlimitSetting {
	var out []rlimitSetting
	if cpu := uint64(limits.CPUTime.Seconds()); cpu > 0 {
		out = append(out, rlimitSetting{unix.RLIMIT_CPU, cpu, cpu})
	}
	if limits.FileBytes > 0 {
		out = append(out, rlimitSetting{unix.RLIMIT_FSIZE, uint64(limits.FileBytes), uint64(limits.FileBytes)})
	}
	if limits.MaxOpenFiles > 0 {
		out = append(out, rlimitSetting{unix.RLIMIT_NOFILE, uint64(limits.MaxOpenFiles), uint64(limits.MaxOpenFiles)})
	}
	if limits.MaxProcesses > 0 {
		out = append(out, rlimitSetting{unix.RLIMIT_NPROC, uint64(limits.MaxProcesses), uint64(limits.MaxProcesses)})
	}
	out = append(out, rlimitSetting{unix.RLIMIT_CORE, uint64(limits.MaxCoreSize), uint64(limits.MaxCoreSize)})
	return out
}

// applyRlimits sets the per-process resource limits on the calling process.
// It runs in the sandbox child before exec, so the limits bind the tool and
// are inherited across the exec.
func applyRlimits(limits Limits) error {
	for _, r := range computeRlimits(limits) {
		if err := unix.Setrlimit(r.resource, &unix.Rlimit{Cur: r.soft, Max: r.hard}); err != nil {
			return Errorf(ErrorCodeSandboxInitFailed, "setrlimit %d: %v", r.resource, err)
		}
	}
	return nil
}
