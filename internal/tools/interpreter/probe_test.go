package interpreter

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"testing"
)

type recordingProber struct {
	err     error
	bundles []Bundle
}

func (p *recordingProber) Probe(_ context.Context, bundle *Bundle) error {
	p.bundles = append(p.bundles, *bundle)
	return p.err
}

type cancellingProber struct{ cancel context.CancelFunc }

func (p *cancellingProber) Probe(ctx context.Context, _ *Bundle) error {
	p.cancel()
	return ctx.Err()
}

func probedFixture(t *testing.T, prober Prober) (*fixture, *VerifiedSet, *Verifier) {
	t.Helper()
	f := newFixture(t, buildArchive(t, validEntries()))
	set, err := NewVerifiedSet(f.store.Manifest())
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := NewVerifier(f.store, set, WithProber(prober))
	if err != nil {
		t.Fatal(err)
	}
	return f, set, verifier
}

func TestVerifierProbeDecidesAvailability(t *testing.T) {
	cases := []struct {
		name         string
		probeErr     error
		wantVerified bool
		wantState    State
		wantReason   Reason
	}{
		{"probe passes", nil, true, StateInstalled, ""},
		{"probe fails", errors.New("wrong version"), false, StateInvalid, ReasonProbeFailed},
		{"runtime without a probe", ErrProbeUnsupported, true, StateInstalled, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prober := &recordingProber{err: tc.probeErr}
			f, set, verifier := probedFixture(t, prober)
			f.installed(t)
			if err := verifier.Run(t.Context()); err != nil {
				t.Fatalf("Run: %v", err)
			}
			got, _ := set.Status(fixtureRuntime)
			if set.Verified(fixtureRuntime) != tc.wantVerified || got.State != tc.wantState || got.Reason != tc.wantReason {
				t.Fatalf("status = %+v, want verified %v, state %q, reason %q", got, tc.wantVerified, tc.wantState, tc.wantReason)
			}
			if len(prober.bundles) != 1 {
				t.Fatalf("probed %d times, want once", len(prober.bundles))
			}
		})
	}
}

func TestVerifierProbesOnlyRuntimesWhoseFilesVerified(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*testing.T, *fixture)
	}{
		{"absent", func(*testing.T, *fixture) {}},
		{"tampered", func(t *testing.T, f *fixture) {
			f.installed(t)
			rewriteFile(t, f.versionDir()+"/"+bundleDirName+"/lib/data.txt", "tampered")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prober := &recordingProber{}
			f, set, verifier := probedFixture(t, prober)
			tc.setup(t, f)
			if err := verifier.Run(t.Context()); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if len(prober.bundles) != 0 || set.Verified(fixtureRuntime) {
				t.Fatalf("probed %d times and verified = %v, want neither", len(prober.bundles), set.Verified(fixtureRuntime))
			}
		})
	}
}

func TestVerifierProbeHandsOverTheBundle(t *testing.T) {
	prober := &recordingProber{}
	f, _, verifier := probedFixture(t, prober)
	f.installed(t)
	if err := verifier.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	bundle := prober.bundles[0]
	if bundle.Runtime.ID != fixtureRuntime || bundle.Dir != filepath.Join(f.versionDir(), bundleDirName) || bundle.Executable != filepath.Join(bundle.Dir, "bin", "tool") {
		t.Fatalf("bundle = %+v", bundle)
	}
}

func TestVerifierProbeStopsOnCancellationAndLeavesTheRuntimePending(t *testing.T) {
	f := newFixture(t, buildArchive(t, validEntries()))
	f.installed(t)
	set, err := NewVerifiedSet(f.store.Manifest())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	verifier, err := NewVerifier(f.store, set, WithProber(&cancellingProber{cancel: cancel}))
	if err != nil {
		t.Fatal(err)
	}
	if err := verifier.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want context cancellation", err)
	}
	if got, _ := set.Status(fixtureRuntime); got.State != StatePending || set.Verified(fixtureRuntime) {
		t.Fatalf("status after a cancelled probe = %+v, want pending", got)
	}
}

func TestBundleLocatesTheInstalledTree(t *testing.T) {
	f := newFixture(t, buildArchive(t, validEntries()))
	bundle, err := f.store.Bundle(fixtureRuntime)
	if err != nil {
		t.Fatalf("Bundle: %v", err)
	}
	want := filepath.Join(f.versionDir(), bundleDirName)
	if bundle.Dir != want || bundle.Executable != filepath.Join(want, "bin", "tool") || bundle.Runtime.Version != fixtureVersion {
		t.Fatalf("bundle = %+v, want it under %s", bundle, want)
	}
	if _, err := f.store.Bundle("ruby"); err == nil {
		t.Fatal("Bundle accepted a runtime that is not pinned")
	}
}

func TestRuntimeCommandAndEnvironment(t *testing.T) {
	runtime := Runtime{
		Argv:        []string{PlaceholderExecutable, "-I", PlaceholderSource},
		Environment: map[string]string{"ZED": "last", "ALPHA": "first"},
	}
	if got, want := runtime.Command("/bundle/bin/tool", "main.py", ""), []string{"/bundle/bin/tool", "-I", "main.py"}; !slices.Equal(got, want) {
		t.Fatalf("Command = %v, want %v", got, want)
	}
	if got, want := runtime.EnvironmentEntries(), []string{"ALPHA=first", "ZED=last"}; !slices.Equal(got, want) {
		t.Fatalf("EnvironmentEntries = %v, want %v", got, want)
	}
	if got := (&Runtime{}).EnvironmentEntries(); len(got) != 0 {
		t.Fatalf("EnvironmentEntries of an empty runtime = %v", got)
	}
}
