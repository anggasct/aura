//go:build durable

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestFetchRestateServerRejectsChecksumOverride(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skipf("fetch script has no pinned target for %s", runtime.GOOS)
	}
	script, err := filepath.Abs(filepath.Join("..", "..", "scripts", "fetch-restate-server.sh"))
	if err != nil {
		t.Fatalf("resolve fetch script: %v", err)
	}
	tampered := []byte("tampered restate-server archive")
	sum := sha256.Sum256(tampered)
	digest := hex.EncodeToString(sum[:])

	fakeBin := t.TempDir()
	fakeCurl := filepath.Join(fakeBin, "curl")
	body := "#!/bin/sh\nout=\"\"\nwhile [ $# -gt 0 ]; do\n  case \"$1\" in\n    --output) shift; out=\"$1\" ;;\n  esac\n  shift\ndone\nprintf '%s' '" + string(tampered) + "' > \"$out\"\n"
	if err := os.WriteFile(fakeCurl, []byte(body), 0o755); err != nil {
		t.Fatalf("write fake curl: %v", err)
	}

	installDir := filepath.Join(t.TempDir(), "install")
	command := exec.CommandContext(t.Context(), "bash", script, "1.7.14", installDir)
	command.Env = append(os.Environ(),
		"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"RESTATE_SHA256_OVERRIDE="+digest,
	)
	output, err := command.CombinedOutput()
	if err == nil {
		t.Fatalf("fetch script accepted an overridden checksum:\n%s", output)
	}
	if !strings.Contains(string(output), "checksum mismatch") {
		t.Fatalf("fetch script failed without a checksum mismatch:\n%s", output)
	}
	if _, statErr := os.Stat(filepath.Join(installDir, "restate-server")); !os.IsNotExist(statErr) {
		t.Fatalf("fetch script installed a binary after a checksum mismatch (stat err = %v)", statErr)
	}
}
