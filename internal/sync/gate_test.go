package sync

import (
	"path/filepath"
	"runtime"
	"testing"

	"github.com/anggasct/aura/internal/config"
)

func syncConfig() *config.Sync {
	return &config.Sync{
		Enabled:            true,
		Remote:             "ssh://git@example.com/owner/aura-assets.git",
		Branch:             "main",
		TransportSecretRef: "env://AURA_SYNC_TRANSPORT_SECRET",
		KnownHostsRef:      "env://AURA_SYNC_KNOWN_HOSTS",
		GitBinary:          "/usr/bin/git",
		SSHBinary:          "/usr/bin/ssh",
		Include:            []string{"skills/**"},
	}
}

func TestCheckGateDisabled(t *testing.T) {
	t.Parallel()
	cfg := syncConfig()
	cfg.Enabled = false
	result := CheckGate(t.Context(), cfg)
	if result.Available {
		t.Fatal("disabled sync must not be available")
	}
}

func TestCheckGateNilConfig(t *testing.T) {
	t.Parallel()
	result := CheckGate(t.Context(), nil)
	if result.Available {
		t.Fatal("nil config must not be available")
	}
}

func TestCheckGateUnpinnedBinary(t *testing.T) {
	t.Parallel()
	cfg := syncConfig()
	cfg.GitBinary = "git"
	result := CheckGate(t.Context(), cfg)
	if result.Available {
		t.Fatal("PATH-resolved binary must not be available")
	}
}

func TestCheckGateMissingBinary(t *testing.T) {
	t.Parallel()
	cfg := syncConfig()
	cfg.GitBinary = filepath.Join(t.TempDir(), "git")
	result := CheckGate(t.Context(), cfg)
	if result.Available {
		t.Fatal("missing binary must not be available")
	}
}

func TestCheckGateNonLinux(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "linux" {
		t.Skip("linux-only gate assertion runs on non-linux builds")
	}
	result := CheckGate(t.Context(), syncConfig())
	if result.Available {
		t.Fatal("non-linux sync must not be available")
	}
}
