//go:build linux

package workspace

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/anggasct/aura/internal/platform/fault"
)

func TestOpenReadStaysBeneathTheWorkspace(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.md")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "docs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "docs", "policy.md"), []byte("policy"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Dir(outside), filepath.Join(root, "linked-dir")); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name       string
		path       string
		wantBody   string
		wantCode   bool
		wantAbsent bool
	}{
		{name: "a file", path: "docs/policy.md", wantBody: "policy"},
		{name: "a symlink to a file outside", path: "link.md", wantCode: true},
		{name: "a path through a symlinked directory", path: "linked-dir/secret.md", wantCode: true},
		{name: "a parent segment", path: "../secret.md", wantCode: true},
		{name: "an absolute path", path: outside, wantCode: true},
		{name: "a missing file", path: "docs/missing.md", wantAbsent: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			file, err := OpenRead(root, tc.path)

			switch {
			case tc.wantCode:
				if !fault.HasCode(err, ErrorCodeOutside) {
					t.Fatalf("OpenRead(%q) = %v, want %s", tc.path, err, ErrorCodeOutside)
				}
			case tc.wantAbsent:
				if !errors.Is(err, fs.ErrNotExist) {
					t.Fatalf("OpenRead(%q) = %v, want a missing file", tc.path, err)
				}
			default:
				if err != nil {
					t.Fatalf("OpenRead(%q): %v", tc.path, err)
				}
				defer func() { _ = file.Close() }()
				body, err := io.ReadAll(file)
				if err != nil || string(body) != tc.wantBody {
					t.Fatalf("body = %q, %v, want %q", body, err, tc.wantBody)
				}
			}
		})
	}
}
