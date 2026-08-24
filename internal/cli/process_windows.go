//go:build windows

package cli

import (
	"github.com/anggasct/aura/internal/health"
)

// processProbe collects the process's own resource pressure. Windows hosts
// have no portable descriptor or cgroup limits to read, so the probe
// reports no evidence and the checker stays silent.
func processProbe() (health.ProcessStatus, bool) {
	return health.ProcessStatus{}, false
}

// processProbeFn is the seam tests use to pin process-resource evidence;
// production reads the real limits.
var processProbeFn = processProbe
