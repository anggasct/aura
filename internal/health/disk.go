package health

import (
	"context"
	"fmt"
	"time"
)

// DiskUsage is the filesystem snapshot for one storage target. FreeInodes
// zero or negative means the filesystem cannot create new files.
type DiskUsage struct {
	FreeBytes  int64
	TotalBytes int64
	FreeInodes int64
}

// FilesystemTarget names one storage path whose filesystem must keep
// headroom. Names are stable labels used in finding components.
const (
	DiskTargetDatabase  = "database"
	DiskTargetArtifacts = "artifacts"
	DiskTargetBackups   = "backups"
	DiskTargetTemp      = "temp"
)

// FilesystemTarget binds a stable label to a path.
type FilesystemTarget struct {
	Name string
	Path string
}

// DiskChecker reports free-space and inode headroom for every storage
// target's filesystem. Thresholds combine validated percentages of the
// target filesystem's capacity with absolute floors — never fixed byte
// assumptions — so large and small volumes degrade at the right points.
//
// Usage must be an injected statfs-style probe; the checker itself never
// touches the filesystem.
type DiskChecker struct {
	Targets         func(ctx context.Context) []FilesystemTarget
	Usage           func(ctx context.Context, path string) (DiskUsage, error)
	WarningPercent  int
	CriticalPercent int
	FloorBytes      int64
	Now             func() time.Time
}

func (c DiskChecker) Check(ctx context.Context) []Finding {
	nowFn := c.Now
	if nowFn == nil {
		nowFn = func() time.Time { return time.Now().UTC() }
	}
	now := nowFn()
	targets := c.Targets(ctx)
	findings := make([]Finding, 0, len(targets))
	for _, target := range targets {
		usage, err := c.Usage(ctx, target.Path)
		if err != nil {
			findings = append(findings, Finding{
				Component: ComponentStorage + "/" + target.Name,
				Code:      "disk_unknown",
				Status:    StatusUnknown,
				Detail:    "filesystem capacity unavailable",
				CheckedAt: now,
			})
			continue
		}
		criticalFree := reserveFor(usage.TotalBytes, c.CriticalPercent, c.FloorBytes)
		warningFree := reserveFor(usage.TotalBytes, c.WarningPercent, c.FloorBytes)
		switch {
		case usage.FreeBytes < criticalFree:
			findings = append(findings, Finding{
				Component: ComponentStorage + "/" + target.Name,
				Code:      "disk_critical",
				Status:    StatusDown,
				Detail:    fmt.Sprintf("%s of %d MiB free, below the critical reserve", mibString(usage.FreeBytes), usage.TotalBytes>>20),
				CheckedAt: now,
			})
		case usage.FreeBytes < warningFree:
			findings = append(findings, Finding{
				Component: ComponentStorage + "/" + target.Name,
				Code:      "disk_warning",
				Status:    StatusDegraded,
				Detail:    fmt.Sprintf("%s of %d MiB free, below the warning reserve", mibString(usage.FreeBytes), usage.TotalBytes>>20),
				CheckedAt: now,
			})
		case usage.FreeInodes == 0:
			findings = append(findings, Finding{
				Component: ComponentStorage + "/" + target.Name,
				Code:      "inode_exhausted",
				Status:    StatusDown,
				Detail:    "no inodes available for new files",
				CheckedAt: now,
			})
		default:
			continue
		}
	}
	if len(findings) == 0 {
		return []Finding{{Component: ComponentStorage + "/paths", Code: "ok", Status: StatusUp, Detail: fmt.Sprintf("%d storage paths within reserves", len(targets)), CheckedAt: now}}
	}
	return findings
}

// reserveFor returns the free-bytes threshold below which a filesystem with
// the given capacity crosses the configured boundary: the larger of the
// percentage reserve and the absolute floor.
func reserveFor(totalBytes int64, percent int, floor int64) int64 {
	if totalBytes <= 0 || percent <= 0 {
		return floor
	}
	percentReserve := totalBytes / 100 * int64(percent)
	return max(percentReserve, floor)
}

func mibString(bytes int64) string {
	return fmt.Sprintf("%d MiB", bytes>>20)
}

// ProcessStatus is the health-owned view of the process's own resource
// limits: descriptor usage against its cap and memory use against the
// effective cgroup or rlimit ceiling.
type ProcessStatus struct {
	FDsOpen          int64
	FDsLimit         int64
	MemoryUsedBytes  int64
	MemoryLimitBytes int64
	MemoryLimitKnown bool
}

// ProcessChecker reports resource pressure that would break new work:
// descriptor exhaustion and memory pressure against limits derived from the
// process/cgroup configuration rather than fixed assumptions.
type ProcessChecker struct {
	Probe func(ctx context.Context) (ProcessStatus, bool)
}

func (c ProcessChecker) Check(ctx context.Context) []Finding {
	status, ok := c.Probe(ctx)
	if !ok {
		// No readable limits is not degradation on hosts without them; the
		// sandbox capability surface already reports containment gaps.
		return nil
	}
	now := time.Now().UTC()
	var findings []Finding
	if status.FDsLimit > 0 && status.FDsOpen >= status.FDsLimit {
		findings = append(findings, Finding{Component: ComponentProcess, Code: "fd_exhausted", Status: StatusDown,
			Detail: fmt.Sprintf("%d of %d descriptors open", status.FDsOpen, status.FDsLimit), CheckedAt: now})
	} else if status.FDsLimit > 0 && status.FDsOpen*100 >= status.FDsLimit*90 {
		findings = append(findings, Finding{Component: ComponentProcess, Code: "fd_pressure", Status: StatusDegraded,
			Detail: fmt.Sprintf("%d of %d descriptors open", status.FDsOpen, status.FDsLimit), CheckedAt: now})
	}
	if status.MemoryLimitKnown && status.MemoryLimitBytes > 0 && status.MemoryUsedBytes >= status.MemoryLimitBytes {
		findings = append(findings, Finding{Component: ComponentProcess, Code: "memory_exceeded", Status: StatusDown,
			Detail: fmt.Sprintf("%s of %s limit", mibString(status.MemoryUsedBytes), mibString(status.MemoryLimitBytes)), CheckedAt: now})
	} else if status.MemoryLimitKnown && status.MemoryLimitBytes > 0 && status.MemoryUsedBytes*100 >= status.MemoryLimitBytes*90 {
		findings = append(findings, Finding{Component: ComponentProcess, Code: "memory_pressure", Status: StatusDegraded,
			Detail: fmt.Sprintf("%s of %s limit", mibString(status.MemoryUsedBytes), mibString(status.MemoryLimitBytes)), CheckedAt: now})
	}
	if len(findings) == 0 {
		return []Finding{{Component: ComponentProcess, Code: "ok", Status: StatusUp, Detail: "resource usage within limits", CheckedAt: now}}
	}
	return findings
}

// ComponentProcess names the process-resource component.
const ComponentProcess = "process"
