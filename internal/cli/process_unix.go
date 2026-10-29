//go:build linux || darwin

package cli

import (
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/anggasct/aura/internal/health"
	"github.com/anggasct/aura/internal/sandbox"
)

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

func fdDirectory() string {
	if _, err := os.Stat("/proc/self/fd"); err == nil {
		return "/proc/self/fd"
	}
	return "/dev/fd"
}

func readCgroupFile(name string) (int64, bool) {
	dir, ok := sandbox.AccountingCgroup()
	if !ok {
		return 0, false
	}
	data, err := os.ReadFile(filepath.Join(dir, name))
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
