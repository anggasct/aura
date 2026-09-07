//go:build linux

package sandbox

import (
	"golang.org/x/sys/unix"
)

type rlimitSetting struct {
	resource int
	soft     uint64
	hard     uint64
}

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

func applyRlimits(limits Limits) error {
	for _, r := range computeRlimits(limits) {
		if err := unix.Setrlimit(r.resource, &unix.Rlimit{Cur: r.soft, Max: r.hard}); err != nil {
			return Errorf(ErrorCodeSandboxInitFailed, "setrlimit %d: %v", r.resource, err)
		}
	}
	return nil
}
