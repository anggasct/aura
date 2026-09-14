package sync

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/anggasct/aura/internal/config"
)

type GateResult struct {
	Available bool
	Reason    string
}

func CheckGate(_ context.Context, cfg *config.Sync) GateResult {
	if cfg == nil {
		return GateResult{Reason: "sync section is not configured"}
	}
	if !cfg.Enabled {
		return GateResult{Reason: "sync is disabled"}
	}
	if runtime.GOOS != "linux" {
		return GateResult{Reason: "sync requires Linux"}
	}
	for _, binary := range []struct {
		name  string
		value string
	}{
		{"git_binary", cfg.GitBinary},
		{"ssh_binary", cfg.SSHBinary},
	} {
		cleaned := filepath.Clean(binary.value)
		if !filepath.IsAbs(cleaned) || cleaned != binary.value || strings.TrimSpace(binary.value) == "" {
			return GateResult{Reason: binary.name + " is not a pinned absolute path"}
		}
		info, err := os.Stat(cleaned)
		if err != nil || info.IsDir() || info.Mode().Perm()&0o111 == 0 {
			return GateResult{Reason: binary.name + " is not an executable file"}
		}
	}
	return GateResult{Available: true}
}
