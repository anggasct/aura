package sync

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"io/fs"
	"os"
	"path"
	"slices"
	"strings"
)

const (
	maxExportFiles     = 4096
	maxExportTotalByte = 32 << 20
)

type ExportEntry struct {
	Path   string
	Size   int64
	Digest string
}

type Export struct {
	Roots   []string
	Entries []ExportEntry
	Digest  string
	Empty   bool
}

type Source interface {
	ReadDir(root, rel string) ([]string, error)
	Open(root, rel string) (fs.File, error)
	Lstat(root, rel string) (fs.FileInfo, error)
}

type dirSource struct{}

func (dirSource) ReadDir(root, rel string) ([]string, error) {
	handle, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer func() { _ = handle.Close() }()
	dir, err := handle.Open(rel)
	if err != nil {
		return nil, err
	}
	defer func() { _ = dir.Close() }()
	names, err := dir.Readdirnames(-1)
	if err != nil {
		return nil, err
	}
	slices.Sort(names)
	return names, nil
}

func (dirSource) Open(root, rel string) (fs.File, error) {
	handle, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer func() { _ = handle.Close() }()
	return handle.Open(rel)
}

func (dirSource) Lstat(root, rel string) (fs.FileInfo, error) {
	handle, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer func() { _ = handle.Close() }()
	info, err := handle.Lstat(rel)
	if err != nil {
		return nil, err
	}
	return info, nil
}

func DirSource() Source {
	return dirSource{}
}

func ExportRoots(ctx context.Context, source Source, roots map[string]string, include []string) (Export, error) {
	if ctx == nil {
		return Export{}, errNilArgument("ctx")
	}
	if source == nil {
		return Export{}, errNilArgument("source")
	}
	ordered, err := NormalizeManifest(include)
	if err != nil {
		return Export{}, err
	}
	snapshot := Export{Roots: ordered}
	var total int64
	var fileCount int
	for _, root := range ordered {
		dir, ok := roots[root]
		if !ok || strings.TrimSpace(dir) == "" {
			return Export{}, Errorf(ErrorCodeManifestInvalid, "manifest root is not configured")
		}
		if err := ctx.Err(); err != nil {
			return Export{}, err
		}
		entries, err := collectRoot(source, root, dir, ordered, &total, &fileCount)
		if err != nil {
			return Export{}, err
		}
		snapshot.Entries = append(snapshot.Entries, entries...)
	}
	slices.SortFunc(snapshot.Entries, func(a, b ExportEntry) int { return strings.Compare(a.Path, b.Path) })
	snapshot.Digest = digestSnapshot(snapshot.Entries)
	snapshot.Empty = len(snapshot.Entries) == 0
	return snapshot, nil
}

func collectRoot(source Source, root, dir string, ordered []string, total *int64, fileCount *int) ([]ExportEntry, error) {
	var entries []ExportEntry
	queue := []string{"."}
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		children, err := source.ReadDir(dir, name)
		if err != nil {
			if name == "." {
				return nil, Errorf(ErrorCodeManifestInvalid, "manifest root is not readable")
			}
			return nil, Errorf(ErrorCodePathDenied, "entry is not readable")
		}
		for _, child := range children {
			rel := path.Join(name, child)
			if name == "." {
				rel = child
			}
			exportRel := root + "/" + rel
			info, err := source.Lstat(dir, rel)
			if err != nil {
				return nil, Errorf(ErrorCodePathDenied, "entry is not readable")
			}
			if info.Mode()&fs.ModeSymlink != 0 {
				return nil, Errorf(ErrorCodePathDenied, "entry is not exportable")
			}
			if info.IsDir() {
				if DeniedPath(exportRel) {
					continue
				}
				queue = append(queue, rel)
				continue
			}
			if !info.Mode().IsRegular() {
				return nil, Errorf(ErrorCodePathDenied, "entry is not exportable")
			}
			if err := FilterExportable(ordered, exportRel, info.Size()); err != nil {
				return nil, err
			}
			if *fileCount >= maxExportFiles {
				return nil, Errorf(ErrorCodePathDenied, "export exceeds the file bound")
			}
			digest, err := digestFile(source, dir, rel, info)
			if err != nil {
				return nil, err
			}
			*total += info.Size()
			if *total > maxExportTotalByte {
				return nil, Errorf(ErrorCodePathDenied, "export exceeds the total bound")
			}
			entries = append(entries, ExportEntry{Path: exportRel, Size: info.Size(), Digest: digest})
			*fileCount++
		}
	}
	return entries, nil
}

func digestFile(source Source, dir, rel string, expected fs.FileInfo) (string, error) {
	size := expected.Size()
	if size < 0 || size > maxManifestBytes {
		return "", Errorf(ErrorCodePathDenied, "entry exceeds the export bound")
	}
	file, err := source.Open(dir, rel)
	if err != nil {
		return "", Errorf(ErrorCodePathDenied, "entry is not readable")
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil {
		return "", Errorf(ErrorCodePathDenied, "entry is not readable")
	}
	if !opened.Mode().IsRegular() {
		return "", Errorf(ErrorCodePathDenied, "entry is not exportable")
	}
	if opened.Size() != size || !opened.ModTime().Equal(expected.ModTime()) {
		return "", Errorf(ErrorCodePathDenied, "entry changed during export")
	}
	raw, err := io.ReadAll(io.LimitReader(file, size+1))
	if err != nil {
		return "", Errorf(ErrorCodePathDenied, "entry is not readable")
	}
	if int64(len(raw)) != size {
		return "", Errorf(ErrorCodePathDenied, "entry changed during export")
	}
	verified, err := file.Stat()
	if err != nil {
		return "", Errorf(ErrorCodePathDenied, "entry is not readable")
	}
	if !verified.Mode().IsRegular() {
		return "", Errorf(ErrorCodePathDenied, "entry is not exportable")
	}
	if verified.Size() != size || !verified.ModTime().Equal(expected.ModTime()) {
		return "", Errorf(ErrorCodePathDenied, "entry changed during export")
	}
	sum := sha256.Sum256(normalizeContent(raw))
	return hex.EncodeToString(sum[:]), nil
}

func digestSnapshot(entries []ExportEntry) string {
	writer := sha256.New()
	_, _ = writer.Write([]byte("sync-export/v1\n"))
	for _, entry := range entries {
		_, _ = writer.Write([]byte(entry.Path))
		_, _ = writer.Write([]byte{0})
		_, _ = writer.Write([]byte(entry.Digest))
		_, _ = writer.Write([]byte{0})
	}
	return hex.EncodeToString(writer.Sum(nil))
}

func digestContent(raw []byte) string {
	sum := sha256.Sum256(normalizeContent(raw))
	return hex.EncodeToString(sum[:])
}

func normalizeContent(raw []byte) []byte {
	return bytes.ReplaceAll(raw, []byte("\r\n"), []byte("\n"))
}
