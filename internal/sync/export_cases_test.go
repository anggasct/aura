package sync

import (
	"strings"
	"testing"
)

func exportRootsFor(source *memSource) map[string]string {
	return map[string]string{"skills": "/skills", "config-templates": "/templates"}
}

func seedExportSource() *memSource {
	source := newMemSource()
	source.addDir("/skills", ".", []string{"reviewed"})
	source.addDir("/skills", "reviewed", []string{"deploy"})
	source.addDir("/skills", "reviewed/deploy", []string{"SKILL.md"})
	source.addFile("/skills", "reviewed/deploy/SKILL.md", "---\nname: deploy\n---\n# Deploy\r\n")
	source.addDir("/templates", ".", []string{"aura.yaml"})
	source.addFile("/templates", "aura.yaml", "version: 1\n")
	return source
}

func TestExportRootsDeterministic(t *testing.T) {
	t.Parallel()
	first, err := ExportRoots(t.Context(), seedExportSource(), exportRootsFor(nil), []string{"config-templates/**", "skills/**"})
	if err != nil {
		t.Fatalf("ExportRoots: %v", err)
	}
	second, err := ExportRoots(t.Context(), seedExportSource(), exportRootsFor(nil), []string{"skills/**", "config-templates/**"})
	if err != nil {
		t.Fatalf("ExportRoots: %v", err)
	}
	if first.Digest != second.Digest {
		t.Fatalf("digest differs across manifest order: %q vs %q", first.Digest, second.Digest)
	}
	if len(first.Entries) != 2 || first.Entries[0].Path != "config-templates/aura.yaml" || first.Entries[1].Path != "skills/reviewed/deploy/SKILL.md" {
		t.Fatalf("entries = %+v, want sorted export paths", first.Entries)
	}
	if first.Empty {
		t.Fatal("non-empty export must not report empty")
	}
}

func TestExportRootsLineEndingNormalization(t *testing.T) {
	t.Parallel()
	crlf := seedExportSource()
	lf := seedExportSource()
	lf.files[memKey("/skills", "reviewed/deploy/SKILL.md")] = []byte("---\nname: deploy\n---\n# Deploy\n")
	first, err := ExportRoots(t.Context(), crlf, exportRootsFor(nil), []string{"skills/**"})
	if err != nil {
		t.Fatalf("ExportRoots: %v", err)
	}
	second, err := ExportRoots(t.Context(), lf, exportRootsFor(nil), []string{"skills/**"})
	if err != nil {
		t.Fatalf("ExportRoots: %v", err)
	}
	if first.Digest != second.Digest {
		t.Fatalf("CRLF and LF exports differ: %q vs %q", first.Digest, second.Digest)
	}
}

func TestExportRootsEmpty(t *testing.T) {
	t.Parallel()
	source := newMemSource()
	source.addDir("/skills", ".", []string{})
	snapshot, err := ExportRoots(t.Context(), source, exportRootsFor(nil), []string{"skills/**"})
	if err != nil {
		t.Fatalf("ExportRoots: %v", err)
	}
	if !snapshot.Empty || len(snapshot.Entries) != 0 {
		t.Fatalf("snapshot = %+v, want empty", snapshot)
	}
}

func TestExportRootsRejects(t *testing.T) {
	t.Parallel()
	cases := map[string]func(*memSource){
		"symlink": func(source *memSource) {
			source.addDir("/skills", ".", []string{"link"})
			source.symlinks[memKey("/skills", "link")] = true
		},
		"special": func(source *memSource) {
			source.addDir("/skills", ".", []string{"dev"})
			source.special[memKey("/skills", "dev")] = true
		},
		"database": func(source *memSource) {
			source.addDir("/skills", ".", []string{"state.db"})
			source.addFile("/skills", "state.db", "x")
		},
		"secret": func(source *memSource) {
			source.addDir("/skills", ".", []string{"deploy.key"})
			source.addFile("/skills", "deploy.key", "x")
		},
		"workspace-escape": func(source *memSource) {
			source.addDir("/skills", ".", []string{".."})
		},
	}
	for name, seed := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			source := newMemSource()
			seed(source)
			_, err := ExportRoots(t.Context(), source, exportRootsFor(nil), []string{"skills/**"})
			if err == nil {
				t.Fatal("export must fail")
			}
			if code, ok := CodeOf(err); !ok || code != ErrorCodePathDenied {
				t.Fatalf("code = %v,%v want sync_path_denied", code, ok)
			}
		})
	}
}

func TestExportDenialHidesPath(t *testing.T) {
	t.Parallel()
	source := newMemSource()
	source.addDir("/skills", ".", []string{"super-secret-backup.key"})
	source.addFile("/skills", "super-secret-backup.key", "x")
	_, err := ExportRoots(t.Context(), source, exportRootsFor(nil), []string{"skills/**"})
	if err == nil {
		t.Fatal("export must fail")
	}
	if strings.Contains(err.Error(), "super-secret-backup") {
		t.Fatalf("denial must not echo the path: %v", err)
	}
}
