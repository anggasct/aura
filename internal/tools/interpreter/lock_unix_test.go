//go:build linux || darwin

package interpreter

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInstallAndRemoveRefuseWhileAnotherHoldsTheLock(t *testing.T) {
	f := newFixture(t, buildArchive(t, validEntries()))
	runtimesRoot, err := f.store.openRuntimesRoot()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = runtimesRoot.Close() }()
	held, err := acquireInstallLock(runtimesRoot)
	if err != nil {
		t.Fatalf("acquireInstallLock: %v", err)
	}
	_, err = f.store.Install(t.Context(), fixtureRuntime, f.archivePath)
	requireCode(t, err, ErrorCodeInstallBusy)
	_, err = f.store.Remove(t.Context(), fixtureRuntime, true)
	requireCode(t, err, ErrorCodeInstallBusy)
	if _, err := f.store.Remove(t.Context(), fixtureRuntime, false); err != nil {
		t.Errorf("a dry run needs no lock: %v", err)
	}
	held.release()
	f.installed(t)
	if _, err := os.Stat(filepath.Join(f.versionDir(), digestFileName)); err != nil {
		t.Errorf("install after the lock was released: %v", err)
	}
}
