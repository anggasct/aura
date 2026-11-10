package auth

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestNoneAndStaticSecretRunBehindTheSameInterface(t *testing.T) {
	cases := []struct {
		name       string
		spec       *Spec
		wantHeader string
		wantValue  string
	}{
		{"none", &Spec{PluginID: testPluginID, Kind: KindNone}, "", ""},
		{"static default header", &Spec{PluginID: testPluginID, Kind: KindStaticSecret, SecretRef: staticRef}, "Authorization", "Bearer static-token-value"},
		{"static custom header", &Spec{PluginID: testPluginID, Kind: KindStaticSecret, SecretRef: staticRef, Header: "X-Api-Key"}, "X-Api-Key", "static-token-value"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			strategy, err := h.registry().Strategy(tc.spec)
			if err != nil {
				t.Fatalf("Strategy: %v", err)
			}
			if err := strategy.Ready(t.Context()); err != nil {
				t.Fatalf("Ready: %v", err)
			}
			request, err := authorizeOnce(t, strategy)
			if err != nil {
				t.Fatalf("Authorize: %v", err)
			}
			if tc.wantHeader == "" {
				if len(request.Header) != 0 {
					t.Fatalf("none set headers: %v", request.Header)
				}
				return
			}
			if got := request.Header.Get(tc.wantHeader); got != tc.wantValue {
				t.Fatalf("%s = %q, want %q", tc.wantHeader, got, tc.wantValue)
			}
			if !h.redactor.has("static-token-value") {
				t.Fatal("the static secret was not registered for redaction")
			}
		})
	}
}

func TestStaticSecretFollowsRotationAndFailsWhenTheSecretDisappears(t *testing.T) {
	h := newHarness(t)
	strategy, err := h.registry().Strategy(&Spec{PluginID: testPluginID, Kind: KindStaticSecret, SecretRef: staticRef})
	if err != nil {
		t.Fatalf("Strategy: %v", err)
	}
	h.secrets.set(staticRef, "rotated-value")
	request, err := authorizeOnce(t, strategy)
	if err != nil || request.Header.Get("Authorization") != "Bearer rotated-value" {
		t.Fatalf("Authorization = %q, err = %v", request.Header.Get("Authorization"), err)
	}
	h.secrets.set(staticRef, "  ")
	requireCode(t, strategy.Ready(t.Context()), ErrRequired)
	request, err = authorizeOnce(t, strategy)
	requireCode(t, err, ErrRequired)
	if request.Header.Get("Authorization") != "" {
		t.Fatal("an unavailable secret still produced a header")
	}
}

func TestRegistryRejectsUnusableSpecs(t *testing.T) {
	h := newHarness(t)
	registry := h.registry()
	cases := []struct {
		name string
		spec *Spec
	}{
		{"nil", nil},
		{"unknown kind", &Spec{PluginID: testPluginID, Kind: "browser_session"}},
		{"static without a secret", &Spec{PluginID: testPluginID, Kind: KindStaticSecret}},
		{"oauth2 without endpoints", &Spec{PluginID: testPluginID, Kind: KindOAuth2, Fingerprint: "fp"}},
		{"oauth2 without a fingerprint", h.spec(func(spec *Spec) { spec.Fingerprint = "" })},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			strategy, err := registry.Strategy(tc.spec)
			if strategy != nil {
				t.Fatalf("strategy = %T, want none", strategy)
			}
			requireCode(t, err, ErrInvalid)
		})
	}
}

func TestNewRegistryRequiresItsCollaborators(t *testing.T) {
	h := newHarness(t)
	cases := []struct {
		name string
		opts *Options
	}{
		{"nil options", nil},
		{"no resolver", &Options{Dir: h.dir, HTTPClient: http.DefaultClient, Redactor: h.redactor}},
		{"no client", &Options{Dir: h.dir, Secrets: h.secrets, Redactor: h.redactor}},
		{"no redactor", &Options{Dir: h.dir, Secrets: h.secrets, HTTPClient: http.DefaultClient}},
		{"relative directory", &Options{Dir: "relative", Secrets: h.secrets, HTTPClient: http.DefaultClient, Redactor: h.redactor}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			registry, err := NewRegistry(tc.opts)
			if registry != nil {
				t.Fatal("registry built from incomplete options")
			}
			requireCode(t, err, ErrInvalid)
		})
	}
}

func TestTokenNamespaceModesAndErasure(t *testing.T) {
	h := newHarness(t)
	registry := h.registry()
	h.loggedIn(h.spec(nil))
	expectMode := func(path string, want os.FileMode) {
		t.Helper()
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("Stat(%s): %v", path, err)
		}
		if info.Mode().Perm() != want {
			t.Fatalf("mode of %s = %o, want %o", filepath.Base(path), info.Mode().Perm(), want)
		}
	}
	expectMode(h.dir, 0o700)
	expectMode(filepath.Join(h.dir, testPluginID), 0o700)
	expectMode(h.tokenPath(), 0o600)
	entries, err := os.ReadDir(filepath.Join(h.dir, testPluginID))
	if err != nil || len(entries) != 1 {
		t.Fatalf("namespace entries = %v, err = %v; want only the token file", entries, err)
	}
	if err := registry.Erase(testPluginID); err != nil {
		t.Fatalf("Erase: %v", err)
	}
	if _, err := os.Stat(filepath.Join(h.dir, testPluginID)); !os.IsNotExist(err) {
		t.Fatalf("namespace after erase: %v", err)
	}
	if err := registry.Erase(testPluginID); err != nil {
		t.Fatalf("erasing twice: %v", err)
	}
	requireCode(t, h.strategy(h.spec(nil)).Ready(t.Context()), ErrRequired)
}

func TestEraseWithoutANamespaceSucceeds(t *testing.T) {
	h := newHarness(t)
	if err := h.registry().Erase(testPluginID); err != nil {
		t.Fatalf("Erase: %v", err)
	}
}

func TestPluginIdsThatComposePathsAreRejected(t *testing.T) {
	h := newHarness(t)
	registry := h.registry()
	for _, id := range []string{"", "..", "../escape", "a/b", "Upper", "x"} {
		t.Run(id, func(t *testing.T) {
			requireCode(t, registry.Erase(id), ErrInvalid)
			strategy, err := registry.Strategy(h.spec(func(spec *Spec) { spec.PluginID = id }))
			if strategy != nil {
				t.Fatalf("strategy = %T, want none", strategy)
			}
			requireCode(t, err, ErrInvalid)
		})
	}
}
