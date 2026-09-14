package sync

import (
	"strings"
	"testing"
)

func TestNormalizeManifest(t *testing.T) {
	t.Parallel()
	roots, err := NormalizeManifest([]string{"config-templates/**", "skills/**"})
	if err != nil {
		t.Fatalf("NormalizeManifest: %v", err)
	}
	if len(roots) != 2 || roots[0] != "config-templates" || roots[1] != "skills" {
		t.Fatalf("roots = %v, want sorted config-templates and skills", roots)
	}
}

func TestNormalizeManifestRejects(t *testing.T) {
	t.Parallel()
	for _, include := range [][]string{
		nil,
		{},
		{"workspace/**"},
		{"skills/**", "skills/**"},
		{"skills"},
		{"../skills/**"},
	} {
		if _, err := NormalizeManifest(include); err == nil {
			t.Fatalf("NormalizeManifest(%v) = nil, want error", include)
		}
	}
}

func TestFilterExportableDenies(t *testing.T) {
	t.Parallel()
	roots := []string{"skills", "config-templates"}
	denied := []string{
		"../escape.md",
		"/abs/path.md",
		"workspace/note.md",
		"skills/state.db",
		"skills/state.db-wal",
		"skills/blob.sqlite-shm",
		"skills/keys/deploy.key",
		"skills/token.pem",
		"skills/auth.token",
		"skills/.env",
		"skills/known_hosts",
		"skills/.git/config",
		"skills/artifacts/blob.bin",
		"skills/logs/run.log",
		"skills/cache/item",
		"skills/workspace/draft.md",
		"skills/approvals/pending.json",
		"skills/pending/item.md",
		"skills/tmp/item.md",
		"skills/.ssh/config",
		"config-templates/id_rsa",
	}
	for _, rel := range denied {
		if err := FilterExportable(roots, rel, 64); err == nil {
			code, ok := CodeOf(err)
			if !ok {
				t.Fatalf("FilterExportable(%q) = nil, want denial", rel)
			}
			_ = code
			t.Fatalf("FilterExportable(%q) = nil, want denial", rel)
		}
	}
}

func TestFilterExportableAllows(t *testing.T) {
	t.Parallel()
	roots := []string{"skills", "config-templates"}
	for _, rel := range []string{
		"skills/reviewed/deploy/SKILL.md",
		"config-templates/aura.yaml",
	} {
		if err := FilterExportable(roots, rel, 64); err != nil {
			t.Fatalf("FilterExportable(%q): %v", rel, err)
		}
	}
}

func TestFilterExportableSizeBound(t *testing.T) {
	t.Parallel()
	if err := FilterExportable([]string{"skills"}, "skills/reviewed/a/SKILL.md", maxManifestBytes+1); err == nil {
		t.Fatal("oversized entry must be denied")
	}
}

func TestDeniedPathNeverLeaksContent(t *testing.T) {
	t.Parallel()
	err := FilterExportable([]string{"skills"}, "skills/secret-key-material.key", 16)
	if err == nil {
		t.Fatal("denied path must fail")
	}
	if strings.Contains(err.Error(), "secret-key-material") {
		t.Fatalf("denial must not echo the path: %v", err)
	}
	if code, ok := CodeOf(err); !ok || code != ErrorCodePathDenied {
		t.Fatalf("code = %v,%v want sync_path_denied", code, ok)
	}
}
