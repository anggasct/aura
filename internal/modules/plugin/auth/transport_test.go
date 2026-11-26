package auth

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/anggasct/aura/internal/platform/fault"
)

func TestNewSecretResolverHandlesEnvFileAndInvalidRefs(t *testing.T) {
	resolver := NewSecretResolver()
	t.Setenv("AURA_TEST_AUTH_SECRET", "env-secret-value")
	got, err := resolver.ResolveSecret(t.Context(), "env://AURA_TEST_AUTH_SECRET")
	if err != nil || got != "env-secret-value" {
		t.Fatalf("ResolveSecret(env) = %q, %v", got, err)
	}
	secretFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(secretFile, []byte("file-secret-value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err = resolver.ResolveSecret(t.Context(), "file://"+secretFile)
	if err != nil || got != "file-secret-value" {
		t.Fatalf("ResolveSecret(file) = %q, %v", got, err)
	}
	if _, err := resolver.ResolveSecret(t.Context(), "env://AURA_TEST_AUTH_MISSING_VAR"); !fault.HasCode(err, ErrRequired) {
		t.Fatalf("missing env error = %v, want %s", err, ErrRequired)
	}
	for _, bad := range []string{"", "secret://foo", "plain-value", "file://relative/path"} {
		if _, err := resolver.ResolveSecret(t.Context(), bad); !fault.HasCode(err, ErrInvalid) {
			t.Fatalf("ResolveSecret(%q) = %v, want %s", bad, err, ErrInvalid)
		}
	}
}

func TestNewHTTPClientRoutesLoopbackLocally(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	client := NewHTTPClient(nil)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("loopback request failed: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	client.CloseIdleConnections()
}
