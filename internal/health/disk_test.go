package health

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func diskTargetsFor(name string) func(context.Context) []FilesystemTarget {
	return func(context.Context) []FilesystemTarget {
		return []FilesystemTarget{{Name: name, Path: "/data"}}
	}
}

func TestDiskCheckerBoundaries(t *testing.T) {
	const gib = int64(1) << 30
	usageFor := func(free int64) DiskUsage {
		return DiskUsage{FreeBytes: free, TotalBytes: 100 * gib, FreeInodes: 1000}
	}
	checker := func(usage DiskUsage) DiskChecker {
		return DiskChecker{
			Targets:         diskTargetsFor(DiskTargetDatabase),
			Usage:           func(context.Context, string) (DiskUsage, error) { return usage, nil },
			WarningPercent:  15,
			CriticalPercent: 8,
			FloorBytes:      512 << 20,
		}
	}

	cases := []struct {
		name     string
		free     int64
		wantCode string // "" means healthy: the single up aggregate
	}{
		{name: "plenty of room", free: 50 * gib},
		{name: "below warning percent", free: 10 * gib, wantCode: "disk_warning"},
		{name: "below critical percent", free: 5 * gib, wantCode: "disk_critical"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			findings := checker(usageFor(tc.free)).Check(context.Background())
			if len(findings) != 1 {
				t.Fatalf("findings = %+v", findings)
			}
			if tc.wantCode == "" {
				if findings[0].Status != StatusUp || findings[0].Code != "ok" {
					t.Errorf("finding = %+v, want up aggregate", findings[0])
				}
				return
			}
			if findings[0].Code != tc.wantCode {
				t.Errorf("code = %q, want %q", findings[0].Code, tc.wantCode)
			}
			if findings[0].Component != ComponentStorage+"/"+DiskTargetDatabase {
				t.Errorf("component = %q", findings[0].Component)
			}
		})
	}
}

// The absolute floor must dominate the percentage on tiny filesystems, and
// inode exhaustion is its own down state even with bytes to spare.
func TestDiskCheckerFloorAndInodes(t *testing.T) {
	base := DiskChecker{
		WarningPercent:  15,
		CriticalPercent: 8,
		FloorBytes:      512 << 20,
	}

	small := DiskUsage{FreeBytes: 400 << 20, TotalBytes: 600 << 20, FreeInodes: 100}
	base.Targets = diskTargetsFor(DiskTargetDatabase)
	base.Usage = func(context.Context, string) (DiskUsage, error) { return small, nil }
	findings := base.Check(context.Background())
	// The 512MiB floor dominates both reserves on this tiny volume, so
	// 400MiB free is already below the critical reserve.
	if len(findings) != 1 || findings[0].Code != "disk_critical" {
		t.Fatalf("floor findings = %+v, want disk_critical from the absolute floor", findings)
	}

	noInodes := DiskUsage{FreeBytes: 40 << 30, TotalBytes: 100 << 30, FreeInodes: 0}
	base.Targets = diskTargetsFor(DiskTargetBackups)
	base.Usage = func(context.Context, string) (DiskUsage, error) { return noInodes, nil }
	findings = base.Check(context.Background())
	if len(findings) != 1 || findings[0].Code != "inode_exhausted" || findings[0].Status != StatusDown {
		t.Fatalf("inode findings = %+v, want down inode_exhausted", findings)
	}

	base.Targets = diskTargetsFor(DiskTargetTemp)
	base.Usage = func(context.Context, string) (DiskUsage, error) { return DiskUsage{}, errors.New("statfs failed") }
	findings = base.Check(context.Background())
	if len(findings) != 1 || findings[0].Code != "disk_unknown" || findings[0].Status != StatusUnknown {
		t.Fatalf("unknown findings = %+v, want unknown disk_unknown", findings)
	}
}

// Multiple targets report independently; every healthy target collapses
// into one aggregate finding.
func TestDiskCheckerReportsPerTarget(t *testing.T) {
	usageByPath := map[string]DiskUsage{
		"/data":    {FreeBytes: 50 << 30, TotalBytes: 100 << 30, FreeInodes: 100},
		"/backups": {FreeBytes: 2 << 30, TotalBytes: 100 << 30, FreeInodes: 100},
	}
	checker := DiskChecker{
		Targets: func(context.Context) []FilesystemTarget {
			return []FilesystemTarget{
				{Name: DiskTargetDatabase, Path: "/data"},
				{Name: DiskTargetBackups, Path: "/backups"},
			}
		},
		Usage:           func(_ context.Context, path string) (DiskUsage, error) { return usageByPath[path], nil },
		WarningPercent:  15,
		CriticalPercent: 8,
		FloorBytes:      512 << 20,
	}
	findings := checker.Check(context.Background())
	// 2GiB free is under both percentage reserves of the 100GiB volume.
	if len(findings) != 1 || findings[0].Component != ComponentStorage+"/"+DiskTargetBackups || findings[0].Code != "disk_critical" {
		t.Fatalf("findings = %+v, want one disk_critical for backups", findings)
	}
}

func TestProcessCheckerPressureMatrix(t *testing.T) {
	cases := []struct {
		name     string
		status   ProcessStatus
		wantCode string // "" for the up aggregate
	}{
		{name: "quiet process", status: ProcessStatus{FDsOpen: 10, FDsLimit: 1024}},
		{name: "fd near limit", status: ProcessStatus{FDsOpen: 950, FDsLimit: 1024}, wantCode: "fd_pressure"},
		{name: "fd exhausted", status: ProcessStatus{FDsOpen: 1024, FDsLimit: 1024}, wantCode: "fd_exhausted"},
		{name: "memory near limit", status: ProcessStatus{MemoryUsedBytes: 950 << 20, MemoryLimitBytes: 1 << 30, MemoryLimitKnown: true}, wantCode: "memory_pressure"},
		{name: "memory over limit", status: ProcessStatus{MemoryUsedBytes: 1 << 30, MemoryLimitBytes: 1 << 30, MemoryLimitKnown: true}, wantCode: "memory_exceeded"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			findings := ProcessChecker{Probe: func(context.Context) (ProcessStatus, bool) { return tc.status, true }}.Check(context.Background())
			if len(findings) != 1 {
				t.Fatalf("findings = %+v", findings)
			}
			if tc.wantCode == "" {
				if findings[0].Status != StatusUp {
					t.Errorf("finding = %+v, want up aggregate", findings[0])
				}
				return
			}
			if findings[0].Code != tc.wantCode {
				t.Errorf("code = %q, want %q", findings[0].Code, tc.wantCode)
			}
		})
	}

	unavailable := ProcessChecker{Probe: func(context.Context) (ProcessStatus, bool) { return ProcessStatus{}, false }}
	if findings := unavailable.Check(context.Background()); findings != nil {
		t.Errorf("unavailable limits produced %+v, want no findings", findings)
	}
}

// A check that degrades after a good observation must surface its staleness:
// the stale unknown finding names the last observation instead of silently
// reporting old data as current.
type variableChecker struct {
	delay    time.Duration
	findings []Finding
}

func (c *variableChecker) Check(ctx context.Context) []Finding {
	select {
	case <-time.After(c.delay):
		return c.findings
	case <-ctx.Done():
		return nil
	}
}

func TestRegistryStalenessCarriesLastObservation(t *testing.T) {
	checker := &variableChecker{findings: []Finding{{Component: "backup", Code: "ok", Status: StatusUp}}}
	registry, err := NewRegistry(RegisteredCheck{ID: "backup", Checker: checker, Timeout: 20 * time.Millisecond})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	first := registry.Evaluate(context.Background())
	if first[0].Code != "ok" || first[0].Stale {
		t.Fatalf("first evaluation = %+v, want fresh ok", first[0])
	}

	checker.delay = time.Second
	findings := registry.Evaluate(context.Background())
	if len(findings) != 1 || findings[0].Code != timeoutCode || !findings[0].Stale {
		t.Fatalf("stale findings = %+v", findings)
	}
	if got := findings[0].Detail; !strings.Contains(got, "last observation") || !strings.Contains(got, "up ok") {
		t.Errorf("stale detail = %q, want the last observation summary", got)
	}
}

// Timed-out checks and their per-attempt contexts must not leak goroutines:
// repeated evaluations settle back to the baseline goroutine count. The
// blocking checker signals entry and completion explicitly, so the
// assertion runs only after every checker goroutine has provably exited —
// no sleeps or scheduler-dependent polling.
func TestRegistryEvaluationsDoNotLeakGoroutines(t *testing.T) {
	block := make(chan struct{})
	entered := make(chan struct{}, 12)
	var running sync.WaitGroup
	blocking := checkerFunc(func(context.Context) []Finding {
		running.Add(1)
		entered <- struct{}{}
		<-block
		running.Done()
		return nil
	})
	registry, err := NewRegistry(
		RegisteredCheck{ID: "a", Checker: blocking, Timeout: 15 * time.Millisecond},
		RegisteredCheck{ID: "b", Checker: blocking, Timeout: 15 * time.Millisecond},
	)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	first := registry.Evaluate(context.Background())
	if len(first) != 2 {
		t.Fatalf("first evaluation findings = %d, want 2 timeouts", len(first))
	}
	// Both checkers are still parked inside their attempt; confirm they
	// entered before relying on the count.
	for range 2 {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("checker never entered its attempt")
		}
	}

	before := runtime.NumGoroutine()
	for range 5 {
		registry.Evaluate(context.Background())
	}
	// Drain the entry signals of the later evaluations so their checkers
	// are accounted for before the release.
	for range 10 {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("later checker never entered its attempt")
		}
	}

	close(block)
	running.Wait()
	runtime.GC()
	if got := runtime.NumGoroutine(); got > before {
		t.Fatalf("goroutines after evaluations = %d, baseline %d", got, before)
	}
}

// checkerFunc adapts a function to the Checker interface.
type checkerFunc func(context.Context) []Finding

func (f checkerFunc) Check(ctx context.Context) []Finding { return f(ctx) }
