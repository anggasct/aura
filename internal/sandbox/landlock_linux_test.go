//go:build linux

package sandbox

import (
	"testing"

	"golang.org/x/sys/unix"
)

func TestLandlockWriteMaskPerABI(t *testing.T) {
	v1 := landlockWriteMask(1)
	v2 := landlockWriteMask(2)
	v3 := landlockWriteMask(3)

	if v1&unix.LANDLOCK_ACCESS_FS_WRITE_FILE == 0 {
		t.Error("write mask must always include WRITE_FILE")
	}
	if v1&unix.LANDLOCK_ACCESS_FS_READ_FILE == 0 {
		t.Error("write mask must include the read mask")
	}
	if v1&unix.LANDLOCK_ACCESS_FS_REFER != 0 {
		t.Error("ABI v1 must not request REFER")
	}
	if v2&unix.LANDLOCK_ACCESS_FS_REFER == 0 {
		t.Error("ABI v2 must request REFER")
	}
	if v2&unix.LANDLOCK_ACCESS_FS_TRUNCATE != 0 {
		t.Error("ABI v2 must not request TRUNCATE")
	}
	if v3&unix.LANDLOCK_ACCESS_FS_TRUNCATE == 0 {
		t.Error("ABI v3 must request TRUNCATE")
	}
}

// landlockABI is a kernel query (no confinement), so it is safe to call in
// the test process. Where Landlock is present it must report a positive ABI;
// where it is absent it must surface an error rather than a false version.
func TestLandlockABIOrUnavailable(t *testing.T) {
	abi, err := landlockABI()
	if !landlockAvailable() {
		if err == nil && abi > 0 {
			t.Fatalf("landlock unavailable but ABI query returned %d", abi)
		}
		t.Logf("landlock not available; unavailable path asserted")
		return
	}
	if err != nil || abi <= 0 {
		t.Fatalf("landlock present but ABI = (%d, %v), want >0", abi, err)
	}
	t.Logf("landlock ABI version %d", abi)
}
