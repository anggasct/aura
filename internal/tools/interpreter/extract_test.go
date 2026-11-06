package interpreter

import (
	"archive/tar"
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func fuzzLimits() extractLimits {
	return extractLimits{maxExpanded: 1 << 16, maxEntries: 64, maxStream: 1 << 18}
}

func FuzzExtractArchive(f *testing.F) {
	f.Add(buildTar(f, validEntries()))
	f.Add(buildTar(f, []tarEntry{linkEntry("a", "b"), linkEntry("b", "a")}))
	f.Add(buildTar(f, []tarEntry{dirEntry("d/"), linkEntry("d/s", ".."), linkEntry("l", "d/s/..")}))
	f.Add(buildTar(f, []tarEntry{{name: "x", typeflag: tar.TypeLink, link: "y", mode: 0o644}}))
	f.Add(buildTar(f, []tarEntry{fileEntry("../x", "x", 0o644)}))
	f.Add([]byte("not a tar"))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		outer := t.TempDir()
		bundle := filepath.Join(outer, "bundle")
		if err := os.Mkdir(bundle, 0o700); err != nil {
			t.Fatal(err)
		}
		root, err := os.OpenRoot(bundle)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			_ = root.Close()
			parent, err := os.OpenRoot(outer)
			if err == nil {
				_ = removeTree(parent, "bundle")
				_ = parent.Close()
			}
		}()
		err = extractArchive(t.Context(), bytes.NewReader(gzipStored(t, data)), root, fuzzLimits())
		if err != nil {
			var coded *Error
			if !errors.As(err, &coded) {
				t.Fatalf("error without a code: %v", err)
			}
			if coded.Code != ErrorCodeArchiveInvalid && coded.Code != ErrorCodeIO {
				t.Fatalf("unexpected error code %q", coded.Code)
			}
		}
		siblings, readErr := os.ReadDir(outer)
		if readErr != nil || len(siblings) != 1 {
			t.Fatalf("extraction wrote outside the bundle: %v, %v", siblings, readErr)
		}
		walkErr := filepath.WalkDir(bundle, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			mode := entry.Type()
			if mode&(fs.ModeDevice|fs.ModeNamedPipe|fs.ModeSocket|fs.ModeCharDevice|fs.ModeSetuid|fs.ModeSetgid) != 0 {
				t.Fatalf("special entry %s extracted", path)
			}
			if mode&fs.ModeSymlink != 0 {
				target, err := os.Readlink(path)
				if err != nil || filepath.IsAbs(target) {
					t.Fatalf("symlink %s has target %q, %v", path, target, err)
				}
			}
			return nil
		})
		if walkErr != nil {
			t.Fatalf("walk extracted tree: %v", walkErr)
		}
	})
}
