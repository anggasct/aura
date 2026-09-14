package sync

import (
	"context"
	"net"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/config"
)

type denyResolver struct{}

func (denyResolver) LookupIP(_ context.Context, _ string) ([]net.IP, error) {
	return []net.IP{net.ParseIP("203.0.113.10")}, nil
}

type loopbackResolver struct{}

func (loopbackResolver) LookupIP(_ context.Context, _ string) ([]net.IP, error) {
	return []net.IP{net.ParseIP("127.0.0.1")}, nil
}

func transportConfig(remote string) *config.Sync {
	return &config.Sync{
		Enabled:            true,
		Remote:             remote,
		Branch:             "main",
		TransportSecretRef: "env://AURA_SYNC_TRANSPORT_SECRET",
		KnownHostsRef:      "file:///run/secrets/known-hosts",
		GitBinary:          "/usr/bin/git",
		SSHBinary:          "/usr/bin/ssh",
		Include:            []string{"skills/**"},
	}
}

func TestResolveTransportHTTPS(t *testing.T) {
	t.Parallel()
	transport, err := ResolveTransport(t.Context(), transportConfig("https://git.example.com/owner/aura-assets.git"), denyResolver{})
	if err != nil {
		t.Fatalf("ResolveTransport: %v", err)
	}
	if transport.Scheme != "https" || transport.Host != "git.example.com" {
		t.Fatalf("transport = %+v, want https git.example.com", transport)
	}
	if transport.Secret.Env == "" || transport.KnownHosts.File == "" {
		t.Fatalf("transport secrets = %+v, want env and file sources", transport)
	}
}

func TestResolveTransportSSHUser(t *testing.T) {
	t.Parallel()
	transport, err := ResolveTransport(t.Context(), transportConfig("ssh://git@example.com/owner/aura-assets.git"), denyResolver{})
	if err != nil {
		t.Fatalf("ResolveTransport ssh user: %v", err)
	}
	if transport.Scheme != "ssh" || transport.Host != "example.com" {
		t.Fatalf("transport = %+v, want ssh example.com", transport)
	}
}

func TestResolveTransportRejects(t *testing.T) {
	t.Parallel()
	for _, remote := range []string{
		"http://git.example.com/owner/aura-assets.git",
		"git://git.example.com/owner/aura-assets.git",
		"https://user:token@git.example.com/owner/aura-assets.git",
		"https://git.example.com/owner/aura-assets.git?token=x",
		"ssh://user:secret@example.com/owner/aura-assets.git",
	} {
		cfg := transportConfig(remote)
		if _, err := ResolveTransport(t.Context(), cfg, denyResolver{}); err == nil {
			t.Fatalf("ResolveTransport(%q) = nil, want error", remote)
		}
	}
	if strings.HasPrefix("ssh://git@example.com:22/owner/aura-assets.git", "ssh://") {
		cfg := transportConfig("ssh://git@example.com:22/owner/aura-assets.git")
		if _, err := ResolveTransport(t.Context(), cfg, loopbackResolver{}); err == nil {
			t.Fatal("loopback SSH destination must be denied")
		}
	}
}

func TestResolveTransportRejectsLoopback(t *testing.T) {
	t.Parallel()
	if _, err := ResolveTransport(t.Context(), transportConfig("https://git.example.com/owner/aura-assets.git"), loopbackResolver{}); err == nil {
		t.Fatal("loopback destination must be denied")
	}
}

func TestResolveTransportRejectsSecretScheme(t *testing.T) {
	t.Parallel()
	for _, ref := range []string{"secret://key", "file-secret://git-deploy-key", "deploy-key"} {
		cfg := transportConfig("https://git.example.com/owner/aura-assets.git")
		cfg.TransportSecretRef = ref
		if _, err := ResolveTransport(t.Context(), cfg, denyResolver{}); err == nil {
			t.Fatalf("ResolveTransport ref %q = nil, want error", ref)
		} else if code, ok := CodeOf(err); !ok || code != ErrorCodeCredentialInvalid {
			t.Fatalf("code = %v,%v want sync_secret_invalid", code, ok)
		}
	}
}

func TestResolveTransportNil(t *testing.T) {
	t.Parallel()
	if _, err := ResolveTransport(t.Context(), nil, denyResolver{}); err == nil {
		t.Fatal("nil cfg must fail")
	}
}
