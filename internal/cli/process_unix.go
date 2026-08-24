//go:build linux || darwin

package cli

import (
	"math"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/anggasct/aura/internal/health"
)

// processProbe collects the process's own resource pressure: descriptor use
// against RLIMIT_NOFILE and memory use against the effective cgroup-v2
// limit. A host without readable limits reports no evidence rather than a
// guess; the health checker then emits no findings for it.
func processProbe() (health.ProcessStatus, bool) {
	status := health.ProcessStatus{}

	if entries, err := os.ReadDir(fdDirectory()); err == nil {
		status.FDsOpen = int64(len(entries))
	}
	var limit unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &limit); err == nil {
		// Through float64 because the rlimit width differs per platform;
		// real descriptor limits are nowhere near either bound.
		status.FDsLimit = int64(math.Min(float64(limit.Cur), math.MaxInt64))
	}

	current, currentOK := readCgroupFile("memory.current")
	maximum, maximumOK := readCgroupFile("memory.max")
	if currentOK && maximumOK && maximum > 0 {
		status.MemoryUsedBytes = current
		status.MemoryLimitBytes = maximum
		status.MemoryLimitKnown = true
	}

	if status.FDsOpen == 0 && status.FDsLimit == 0 && !status.MemoryLimitKnown {
		return status, false
	}
	return status, true
}

// fdDirectory is where the kernel exposes this process's open descriptors.
func fdDirectory() string {
	if _, err := os.Stat("/proc/self/fd"); err == nil {
		return "/proc/self/fd"
	}
	return "/dev/fd"
}

// cgroupV2Path locates this process's unified-hierarchy directory, or "".
func cgroupV2Path() string {
	data, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		if rel, ok := strings.CutPrefix(line, "0::"); ok {
			return "/sys/fs/cgroup" + rel
		}
	}
	return ""
}

// readCgroupFile reads one byte-count file from the process's own cgroup v2
// directory. The literal "max" means unbounded and reads as absent so the
// checker never divides by an infinite limit.
func readCgroupFile(name string) (int64, bool) {
	path := cgroupV2Path()
	if path == "" {
		return 0, false
	}
	data, err := os.ReadFile(path + "/" + name)
	if err != nil {
		return 0, false
	}
	text := strings.TrimSpace(string(data))
	if text == "" || text == "max" {
		return 0, false
	}
	value, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return 0, false
	}
	return value, true
}
