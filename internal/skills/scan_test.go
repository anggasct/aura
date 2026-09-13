package skills

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

const validSkillDocument = "---\nname: pdf-tools\ndescription: Handle PDF files.\n---\n# Instructions\nDo things.\n"

func writePackageFile(t *testing.T, dir, name, content string) {
	t.Helper()
	full := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func makeValidPackage(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writePackageFile(t, dir, "SKILL.md", validSkillDocument)
	writePackageFile(t, dir, filepath.Join("scripts", "run.sh"), "#!/bin/sh\necho hi\n")
	writePackageFile(t, dir, filepath.Join("references", "doc.md"), "# Reference\n")
	return dir
}

func scanOK(t *testing.T, dir string) *ScannedDir {
	t.Helper()
	result := ScanDir(t.Context(), dir, "local")
	if result.Err != nil {
		t.Fatalf("scan error: %v", result.Err)
	}
	if len(result.Findings) != 0 {
		t.Fatalf("findings = %v", result.Findings)
	}
	if result.Scanned == nil {
		t.Fatal("scan result is nil")
	}
	return result.Scanned
}

func scanFinding(t *testing.T, dir, want string) {
	t.Helper()
	result := ScanDir(t.Context(), dir, "local")
	if result.Err != nil {
		t.Fatalf("scan error: %v", result.Err)
	}
	if !containsFinding(result.Findings, want) {
		t.Errorf("findings = %v, want %s", result.Findings, want)
	}
}

func TestScanValidPackage(t *testing.T) {
	dir := makeValidPackage(t)
	first := scanOK(t, dir)
	if first.Manifest.Name != "pdf-tools" {
		t.Errorf("name = %q", first.Manifest.Name)
	}
	if len(first.Files) != 3 {
		t.Errorf("files = %d, want 3", len(first.Files))
	}
	if len(first.Digest) != 64 {
		t.Errorf("digest = %q", first.Digest)
	}
	second := scanOK(t, dir)
	if first.Digest != second.Digest {
		t.Errorf("digest unstable: %s vs %s", first.Digest, second.Digest)
	}
}

func TestScanDirInvalid(t *testing.T) {
	t.Run("missing skill file", func(t *testing.T) {
		dir := t.TempDir()
		writePackageFile(t, dir, "README.md", "# hi\n")
		scanFinding(t, dir, FindingSkillFileMissing)
	})
	t.Run("invalid manifest quarantines", func(t *testing.T) {
		dir := t.TempDir()
		writePackageFile(t, dir, "SKILL.md", "---\ndescription: no name\n---\n")
		scanFinding(t, dir, FindingNameMissing)
	})
	t.Run("escape symlink", func(t *testing.T) {
		dir := t.TempDir()
		writePackageFile(t, dir, "SKILL.md", validSkillDocument)
		if err := os.Symlink("/etc/hostname", filepath.Join(dir, "evil")); err != nil {
			t.Skipf("symlink unsupported: %v", err)
		}
		scanFinding(t, dir, FindingSymlinkEscape)
	})
	t.Run("parent escape symlink", func(t *testing.T) {
		dir := t.TempDir()
		writePackageFile(t, dir, "SKILL.md", validSkillDocument)
		if err := os.Symlink("..", filepath.Join(dir, "up")); err != nil {
			t.Skipf("symlink unsupported: %v", err)
		}
		scanFinding(t, dir, FindingSymlinkEscape)
	})
	t.Run("unresolvable symlink", func(t *testing.T) {
		dir := t.TempDir()
		writePackageFile(t, dir, "SKILL.md", validSkillDocument)
		if err := os.Symlink("no-such-target", filepath.Join(dir, "dangling")); err != nil {
			t.Skipf("symlink unsupported: %v", err)
		}
		scanFinding(t, dir, FindingSymlinkUnresolvable)
	})
	t.Run("symlink loop", func(t *testing.T) {
		dir := t.TempDir()
		writePackageFile(t, dir, "SKILL.md", validSkillDocument)
		if err := os.Symlink("b", filepath.Join(dir, "a")); err != nil {
			t.Skipf("symlink unsupported: %v", err)
		}
		if err := os.Symlink("a", filepath.Join(dir, "b")); err != nil {
			t.Skipf("symlink unsupported: %v", err)
		}
		scanFinding(t, dir, FindingSymlinkUnresolvable)
	})
	t.Run("directory symlink", func(t *testing.T) {
		dir := t.TempDir()
		writePackageFile(t, dir, "SKILL.md", validSkillDocument)
		writePackageFile(t, dir, filepath.Join("real", "f.txt"), "x")
		if err := os.Symlink("real", filepath.Join(dir, "linkdir")); err != nil {
			t.Skipf("symlink unsupported: %v", err)
		}
		scanFinding(t, dir, FindingSymlinkDirectory)
	})
	t.Run("hard link", func(t *testing.T) {
		dir := t.TempDir()
		writePackageFile(t, dir, "SKILL.md", validSkillDocument)
		writePackageFile(t, dir, "data.txt", "shared bytes")
		if err := os.Link(filepath.Join(dir, "data.txt"), filepath.Join(dir, "data-copy.txt")); err != nil {
			t.Skipf("hard links unsupported: %v", err)
		}
		scanFinding(t, dir, FindingHardLink)
	})
	t.Run("special file", func(t *testing.T) {
		dir := t.TempDir()
		writePackageFile(t, dir, "SKILL.md", validSkillDocument)
		if err := syscall.Mkfifo(filepath.Join(dir, "pipe"), 0o600); err != nil {
			t.Skipf("fifo unsupported: %v", err)
		}
		scanFinding(t, dir, FindingSpecialFile)
	})
	t.Run("deep tree", func(t *testing.T) {
		dir := t.TempDir()
		deep := dir
		for range maxPackageDepth + 1 {
			deep = filepath.Join(deep, "d")
		}
		writePackageFile(t, deep, "SKILL.md", validSkillDocument)
		writePackageFile(t, dir, "SKILL.md", validSkillDocument)
		scanFinding(t, dir, FindingTreeTooDeep)
	})
	t.Run("too many files", func(t *testing.T) {
		dir := t.TempDir()
		writePackageFile(t, dir, "SKILL.md", validSkillDocument)
		for index := range maxPackageFiles {
			writePackageFile(t, dir, "extra-"+strings.Repeat("a", index%16)+"-"+strings.Repeat("b", index/16)+".txt", "x")
		}
		scanFinding(t, dir, FindingTooManyFiles)
	})
	t.Run("oversized file", func(t *testing.T) {
		dir := t.TempDir()
		writePackageFile(t, dir, "SKILL.md", validSkillDocument)
		writePackageFile(t, dir, "big.bin", strings.Repeat("a", maxSkillFileBytes+1))
		scanFinding(t, dir, FindingFileTooLarge)
	})
	t.Run("case collision", func(t *testing.T) {
		dir := t.TempDir()
		writePackageFile(t, dir, "SKILL.md", validSkillDocument)
		writePackageFile(t, dir, "Skill.md", validSkillDocument)
		scanFinding(t, dir, FindingCaseCollision)
	})
	t.Run("control character name", func(t *testing.T) {
		dir := t.TempDir()
		writePackageFile(t, dir, "SKILL.md", validSkillDocument)
		writePackageFile(t, dir, "bad\nname.txt", "x")
		scanFinding(t, dir, FindingPathInvalid)
	})
}

func TestScanDirArguments(t *testing.T) {
	dir := makeValidPackage(t)
	var nilCtx context.Context
	result := ScanDir(nilCtx, dir, "local")
	if result.Err == nil {
		t.Errorf("nil ctx should fail")
	} else if code, ok := CodeOf(result.Err); !ok || code != ErrorCodeInvalidArgument {
		t.Errorf("code = %v, %v", code, ok)
	}
	for _, scope := range []string{"", "has space", "has\ttab"} {
		result := ScanDir(context.Background(), dir, scope)
		if result.Err == nil {
			t.Errorf("scope %q should fail", scope)
		}
	}
	result = ScanDir(context.Background(), filepath.Join(dir, "missing"), "local")
	if result.Err == nil {
		t.Errorf("missing dir should fail")
	} else if code, ok := CodeOf(result.Err); !ok || code != ErrorCodeInvalidArgument {
		t.Errorf("code = %v, %v", code, ok)
	}
}

func TestScanDigestChangesWithBytes(t *testing.T) {
	first := scanOK(t, makeValidPackage(t))
	dir := makeValidPackage(t)
	writePackageFile(t, dir, filepath.Join("references", "doc.md"), "# Changed\n")
	second := scanOK(t, dir)
	if first.Digest == second.Digest {
		t.Errorf("digest did not change after edit")
	}
}
