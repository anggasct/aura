package sandbox

import (
	"os"
	"slices"
	"syscall"
	"testing"
	"time"
)

func TestSetupWaitUsesTheSetupTimeoutIndependentlyOfTheExchangeTimeout(t *testing.T) {
	cases := []struct {
		name     string
		exchange time.Duration
		setup    time.Duration
		want     time.Duration
	}{
		{"setup timeout longer than the exchange timeout", 2 * time.Second, 30 * time.Second, 30 * time.Second},
		{"setup timeout shorter than the default wait", time.Minute, 3 * time.Second, 3 * time.Second},
		{"no setup timeout keeps the exchange-bounded default", 2 * time.Second, 0, 2 * time.Second},
		{"no setup timeout caps a long exchange at the default wait", time.Minute, 0, sessionSetupWait},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := &SessionRequest{Limits: Limits{Timeout: tc.exchange}, SetupTimeout: tc.setup}
			if got := setupWait(req); got != tc.want {
				t.Fatalf("setupWait = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSigtermDroppedByInitFollowsTheSignalDisposition(t *testing.T) {
	cases := []struct {
		name   string
		status string
		want   bool
	}{
		{"default disposition", "Name:\tsh\nSigIgn:\t0000000000000000\nSigCgt:\t0000000000000000\n", true},
		{"handler installed", "SigIgn:\t0000000000000000\nSigCgt:\t0000000000004000\n", false},
		{"explicitly ignored", "SigIgn:\t0000000000004000\nSigCgt:\t0000000000000000\n", false},
		{"other signals only", "SigIgn:\t0000000000000004\nSigCgt:\t0000000000000002\n", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			caught, okCaught := signalMask(tc.status, "SigCgt:")
			ignored, okIgnored := signalMask(tc.status, "SigIgn:")
			if !okCaught || !okIgnored {
				t.Fatalf("masks not parsed from %q", tc.status)
			}
			bit := uint64(1) << (syscall.SIGTERM - 1)
			if got := caught&bit == 0 && ignored&bit == 0; got != tc.want {
				t.Fatalf("dropped = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSignalMaskRejectsMissingAndMalformedFields(t *testing.T) {
	for name, status := range map[string]string{
		"missing":   "Name:\tsh\n",
		"malformed": "SigCgt:\tzz\n",
		"empty":     "",
	} {
		t.Run(name, func(t *testing.T) {
			if _, ok := signalMask(status, "SigCgt:"); ok {
				t.Fatalf("mask accepted from %q", status)
			}
		})
	}
}

func TestSigtermDroppedByInitFailsSafeForAMissingProcess(t *testing.T) {
	if sigtermDroppedByInit(-1) {
		t.Fatal("a missing process must get the full grace period")
	}
}

func TestScratchDirectoryExistsOnlyForTheConnectorProfile(t *testing.T) {
	bundle := t.TempDir()
	base := &Spec{WorkingDir: bundle, AllowEnv: []string{"A=1"}}
	scoped, scratch, err := withScratch(base)
	if err != nil || scratch != "" || scoped != base {
		t.Fatalf("base profile = %p %q %v, want the spec untouched", scoped, scratch, err)
	}

	connector := &Spec{Profile: ProfileConnector, WorkingDir: bundle, AllowEnv: []string{"A=1"}}
	scoped, scratch, err = withScratch(connector)
	if err != nil {
		t.Fatalf("withScratch: %v", err)
	}
	t.Cleanup(func() { removeScratch(scratch) })
	if scoped == connector || len(connector.ReadOnlyPaths) != 0 || len(connector.ReadWritePaths) != 0 || len(connector.AllowEnv) != 1 {
		t.Fatalf("the caller's spec was modified: %+v", connector)
	}
	if !slices.Equal(scoped.ReadOnlyPaths, []string{bundle}) {
		t.Errorf("read-only paths = %v, want the bundle", scoped.ReadOnlyPaths)
	}
	if !slices.Equal(scoped.ReadWritePaths, []string{scratch}) {
		t.Errorf("writable paths = %v, want only the scratch directory %q", scoped.ReadWritePaths, scratch)
	}
	if !slices.Contains(scoped.AllowEnv, "TMPDIR="+scratch) {
		t.Errorf("environment = %v, want TMPDIR at the scratch directory", scoped.AllowEnv)
	}
	if info, err := os.Stat(scratch); err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("scratch directory = %v, %v, want a private directory", info, err)
	}
	removeScratch(scratch)
	if _, err := os.Stat(scratch); !os.IsNotExist(err) {
		t.Fatalf("scratch directory survived removal: %v", err)
	}
}
