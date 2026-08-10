//go:build linux

package sandbox

import (
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestComputeRlimits(t *testing.T) {
	t.Run("zero fields leave resources unlimited except core", func(t *testing.T) {
		got := computeRlimits(Limits{})
		if len(got) != 1 || got[0].resource != unix.RLIMIT_CORE || got[0].soft != 0 || got[0].hard != 0 {
			t.Fatalf("computeRlimits(Limits{}) = %+v, want only RLIMIT_CORE=0", got)
		}
	})

	t.Run("every bound maps to the matching resource", func(t *testing.T) {
		got := computeRlimits(Limits{
			CPUTime:      30 * time.Second,
			FileBytes:    1 << 20,
			MaxOpenFiles: 128,
			MaxProcesses: 32,
			MaxCoreSize:  0,
		})
		want := map[int]uint64{
			unix.RLIMIT_CPU:    30,
			unix.RLIMIT_FSIZE:  1 << 20,
			unix.RLIMIT_NOFILE: 128,
			unix.RLIMIT_NPROC:  32,
			unix.RLIMIT_CORE:   0,
		}
		seen := make(map[int]uint64, len(got))
		for _, r := range got {
			seen[r.resource] = r.soft
		}
		for res, v := range want {
			if seen[res] != v {
				t.Errorf("rlimit %d = %d, want %d", res, seen[res], v)
			}
		}
	})
}
