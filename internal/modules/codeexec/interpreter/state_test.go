package interpreter

import (
	"context"
	"sync"
	"testing"
)

func TestVerifiedSetStartsPendingAndRecordsVerifications(t *testing.T) {
	f := newFixture(t, buildArchive(t, validEntries()))
	set, err := NewVerifiedSet(f.store.Manifest())
	if err != nil {
		t.Fatal(err)
	}
	statuses := set.Statuses()
	if len(statuses) != 1 || statuses[0].State != StatePending || set.Verified(fixtureRuntime) {
		t.Fatalf("initial statuses = %+v, want one pending runtime", statuses)
	}
	if _, ok := set.Status("ruby"); ok {
		t.Fatal("Status found a runtime that is not pinned")
	}
	f.installed(t)
	verifier, err := NewVerifier(f.store, set)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifier.Run(t.Context()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !set.Verified(fixtureRuntime) {
		t.Fatalf("statuses after Run = %+v, want the runtime verified", set.Statuses())
	}
	set.record(Verification{Runtime: "ruby", State: StateInstalled})
	if _, ok := set.Status("ruby"); ok {
		t.Fatal("record accepted a runtime that is not pinned")
	}
}

func TestVerifierRecordsTamperedAndAbsentRuntimes(t *testing.T) {
	f := newFixture(t, buildArchive(t, validEntries()))
	set, err := NewVerifiedSet(f.store.Manifest())
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := NewVerifier(f.store, set)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifier.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got, _ := set.Status(fixtureRuntime); got.State != StateAbsent {
		t.Fatalf("status without an install = %+v, want absent", got)
	}
	f.installed(t)
	rewriteFile(t, f.versionDir()+"/"+bundleDirName+"/lib/data.txt", "tampered")
	if err := verifier.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got, _ := set.Status(fixtureRuntime); got.State != StateInvalid || got.Reason != ReasonDigestMismatch || set.Verified(fixtureRuntime) {
		t.Fatalf("status after tampering = %+v, want invalid with a digest mismatch", got)
	}
}

func TestVerifierStopsOnCancellationAndLeavesTheRuntimePending(t *testing.T) {
	f := newFixture(t, buildArchive(t, validEntries()))
	f.installed(t)
	set, err := NewVerifiedSet(f.store.Manifest())
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := NewVerifier(f.store, set)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := verifier.Run(ctx); err == nil {
		t.Fatal("Run with a cancelled context succeeded")
	}
	if got, _ := set.Status(fixtureRuntime); got.State != StatePending {
		t.Fatalf("status after a cancelled run = %+v, want pending", got)
	}
}

func TestVerifiedSetIsSafeForConcurrentUse(t *testing.T) {
	f := newFixture(t, buildArchive(t, validEntries()))
	f.installed(t)
	set, err := NewVerifiedSet(f.store.Manifest())
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := NewVerifier(f.store, set)
	if err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	for range 4 {
		group.Go(func() {
			for range 50 {
				_ = set.Statuses()
				_ = set.Verified(fixtureRuntime)
			}
		})
	}
	group.Go(func() { _ = verifier.Run(t.Context()) })
	group.Wait()
	if !set.Verified(fixtureRuntime) {
		t.Fatalf("statuses = %+v, want the runtime verified", set.Statuses())
	}
}

func TestConstructorsRejectMissingDependencies(t *testing.T) {
	if _, err := NewStore("", nil); err == nil {
		t.Error("NewStore accepted a nil manifest")
	}
	manifest, err := DefaultManifest()
	if err != nil {
		t.Fatal(err)
	}
	_, err = NewStore("", manifest)
	requireCode(t, err, ErrorCodeInvalidArgument)
	_, err = NewVerifiedSet(nil)
	requireCode(t, err, ErrorCodeInvalidArgument)
	_, err = NewVerifier(nil, nil)
	requireCode(t, err, ErrorCodeInvalidArgument)
}
