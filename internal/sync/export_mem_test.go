package sync

import (
	"errors"
	"io"
	"io/fs"
	"slices"
	"testing"
	"time"
)

func errTestNotFound() error {
	return &fs.PathError{Op: "open", Err: errors.New("no such file")}
}

type memFile struct {
	name    string
	data    []byte
	size    int64
	modTime time.Time
}

func (f *memFile) Stat() (fs.FileInfo, error) { return f, nil }
func (f *memFile) Read(b []byte) (int, error) {
	if len(f.data) == 0 {
		return 0, io.EOF
	}
	n := copy(b, f.data)
	f.data = f.data[n:]
	return n, nil
}
func (f *memFile) Close() error       { return nil }
func (f *memFile) Name() string       { return f.name }
func (f *memFile) Size() int64        { return f.size }
func (f *memFile) Mode() fs.FileMode  { return 0o644 }
func (f *memFile) ModTime() time.Time { return f.modTime }
func (f *memFile) IsDir() bool        { return false }
func (f *memFile) Sys() any           { return nil }

type memDirInfo struct {
	name string
}

func (d *memDirInfo) Name() string      { return d.name }
func (d *memDirInfo) Size() int64       { return 0 }
func (d *memDirInfo) Mode() fs.FileMode { return fs.ModeDir | 0o755 }
func (d *memDirInfo) ModTime() time.Time {
	return time.Time{}
}
func (d *memDirInfo) IsDir() bool { return true }
func (d *memDirInfo) Sys() any    { return nil }

type memSymlinkInfo struct {
	name string
	size int64
}

func (s *memSymlinkInfo) Name() string      { return s.name }
func (s *memSymlinkInfo) Size() int64       { return s.size }
func (s *memSymlinkInfo) Mode() fs.FileMode { return fs.ModeSymlink | 0o777 }
func (s *memSymlinkInfo) ModTime() time.Time {
	return time.Time{}
}
func (s *memSymlinkInfo) IsDir() bool { return false }
func (s *memSymlinkInfo) Sys() any    { return nil }

type memSource struct {
	files    map[string][]byte
	children map[string][]string
	symlinks map[string]bool
	special  map[string]bool
}

func newMemSource() *memSource {
	return &memSource{files: map[string][]byte{}, children: map[string][]string{}, symlinks: map[string]bool{}, special: map[string]bool{}}
}

func memKey(root, rel string) string {
	return root + "\x00" + rel
}

func (m *memSource) addDir(root, rel string, children []string) {
	m.children[memKey(root, rel)] = slices.Clone(children)
}

func (m *memSource) addFile(root, rel, content string) {
	m.files[memKey(root, rel)] = []byte(content)
}

func (m *memSource) ReadDir(root, rel string) ([]string, error) {
	children, ok := m.children[memKey(root, rel)]
	if !ok {
		return nil, errTestNotFound()
	}
	out := slices.Clone(children)
	slices.Sort(out)
	return out, nil
}

func (m *memSource) Open(root, rel string) (fs.File, error) {
	data, ok := m.files[memKey(root, rel)]
	if !ok {
		return nil, errTestNotFound()
	}
	clone := slices.Clone(data)
	return &memFile{name: rel, data: clone, size: int64(len(clone))}, nil
}

func (m *memSource) Lstat(root, rel string) (fs.FileInfo, error) {
	key := memKey(root, rel)
	if m.symlinks[key] {
		size := int64(8)
		if data, ok := m.files[key]; ok {
			size = int64(len(data))
		}
		return &memSymlinkInfo{name: rel, size: size}, nil
	}
	if m.special[key] {
		return &memSpecialInfo{name: rel}, nil
	}
	if data, ok := m.files[key]; ok {
		clone := slices.Clone(data)
		return &memFile{name: rel, data: clone, size: int64(len(clone))}, nil
	}
	if _, ok := m.children[key]; ok {
		return &memDirInfo{name: rel}, nil
	}
	return nil, errTestNotFound()
}

type memSpecialInfo struct {
	name string
}

func (s *memSpecialInfo) Name() string      { return s.name }
func (s *memSpecialInfo) Size() int64       { return 0 }
func (s *memSpecialInfo) Mode() fs.FileMode { return fs.ModeDevice }
func (s *memSpecialInfo) ModTime() time.Time {
	return time.Time{}
}
func (s *memSpecialInfo) IsDir() bool { return false }
func (s *memSpecialInfo) Sys() any    { return nil }

var _ = testing.T{}
