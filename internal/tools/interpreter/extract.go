package interpreter

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
)

const (
	maxArchiveBytes     = 512 << 20
	maxExpandedBytes    = 1 << 30
	maxArchiveEntries   = 100000
	maxStreamBytes      = maxExpandedBytes + 256<<20
	maxEntryPathLength  = 4096
	maxPathComponent    = 255
	maxSymlinkTarget    = 4096
	maxSymlinkHops      = 40
	copyChunkBytes      = 256 << 10
	directoryPermission = fs.FileMode(0o555)
	executablePerm      = fs.FileMode(0o555)
	regularPerm         = fs.FileMode(0o444)
)

type extractLimits struct {
	maxExpanded int64
	maxEntries  int
	maxStream   int64
}

func defaultExtractLimits() extractLimits {
	return extractLimits{maxExpanded: maxExpandedBytes, maxEntries: maxArchiveEntries, maxStream: maxStreamBytes}
}

type entryKind byte

const (
	kindDirectory entryKind = 'd'
	kindFile      entryKind = 'f'
	kindSymlink   entryKind = 'l'
)

type errStreamLimit struct{}

func (errStreamLimit) Error() string { return "archive expands beyond the allowed size" }

type readFunc func(p []byte) (int, error)

func (f readFunc) Read(p []byte) (int, error) { return f(p) }

type writeFunc func(p []byte) (int, error)

func (f writeFunc) Write(p []byte) (int, error) { return f(p) }

func boundedReader(ctx context.Context, src io.Reader, limit int64) io.Reader {
	remaining := limit
	return readFunc(func(p []byte) (int, error) {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		if remaining <= 0 {
			var probe [1]byte
			n, err := src.Read(probe[:])
			if n > 0 {
				return 0, errStreamLimit{}
			}
			return 0, err
		}
		if int64(len(p)) > remaining {
			p = p[:remaining]
		}
		n, err := src.Read(p)
		remaining -= int64(n)
		return n, err
	})
}

func copyWithContext(ctx context.Context, dst io.Writer, src io.Reader) (int64, error) {
	buffer := make([]byte, copyChunkBytes)
	guarded := writeFunc(func(p []byte) (int, error) {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		return dst.Write(p)
	})
	return io.CopyBuffer(guarded, struct{ io.Reader }{src}, buffer)
}

type extractor struct {
	root     *os.Root
	limits   extractLimits
	kinds    map[string]entryKind
	links    map[string]string
	dirs     []string
	entries  int
	expanded int64
}

func extractArchive(ctx context.Context, archive io.Reader, bundle *os.Root, limits extractLimits) error {
	gz, err := gzip.NewReader(boundedReader(ctx, archive, limits.maxStream))
	if err != nil {
		return archiveError(ctx, err)
	}
	defer func() { _ = gz.Close() }()
	stream := boundedReader(ctx, gz, limits.maxStream)
	x := &extractor{root: bundle, limits: limits, kinds: map[string]entryKind{}, links: map[string]string{}}
	reader := tar.NewReader(stream)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return archiveError(ctx, err)
		}
		if err := x.entry(ctx, header, reader); err != nil {
			return err
		}
	}
	if _, err := copyWithContext(ctx, io.Discard, stream); err != nil {
		return archiveError(ctx, err)
	}
	if err := x.checkLinks(); err != nil {
		return err
	}
	return x.seal()
}

func archiveError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	var coded *Error
	if errors.As(err, &coded) {
		return err
	}
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		return ioError("extract archive", err)
	}
	var limit errStreamLimit
	if errors.As(err, &limit) {
		return Errorf(ErrorCodeArchiveInvalid, "archive expands beyond the allowed size")
	}
	return Errorf(ErrorCodeArchiveInvalid, "archive is not a readable gzip-compressed tar file")
}

func (x *extractor) entry(ctx context.Context, header *tar.Header, reader io.Reader) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	x.entries++
	if x.entries > x.limits.maxEntries {
		return entryInvalid(x.entries, "archive has too many entries")
	}
	if header.Mode&0o7000 != 0 {
		return entryInvalid(x.entries, "setuid, setgid and sticky bits are not allowed")
	}
	name, ok := cleanEntryName(header.Name)
	if !ok {
		return entryInvalid(x.entries, "entry name is not a safe relative path")
	}
	if name == "." {
		if header.Typeflag != tar.TypeDir {
			return entryInvalid(x.entries, "entry name is not a safe relative path")
		}
		return nil
	}
	if err := x.ensureParents(name); err != nil {
		return err
	}
	switch header.Typeflag {
	case tar.TypeDir:
		return x.directory(name)
	case tar.TypeReg:
		return x.file(ctx, name, header, reader)
	case tar.TypeSymlink:
		return x.symlink(name, header.Linkname)
	case tar.TypeLink:
		return entryInvalid(x.entries, "hard links are not allowed")
	case tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
		return entryInvalid(x.entries, "device and fifo entries are not allowed")
	default:
		return entryInvalid(x.entries, "entry type is not supported")
	}
}

func entryInvalid(index int, reason string) error {
	return Errorf(ErrorCodeArchiveInvalid, "entry %d: %s", index, reason)
}

func cleanEntryName(raw string) (string, bool) {
	if strings.HasPrefix(raw, "/") {
		return "", false
	}
	name := strings.TrimSuffix(raw, "/")
	for strings.HasPrefix(name, "./") {
		name = strings.TrimPrefix(name, "./")
	}
	if name == "" && raw != "" {
		name = "."
	}
	if name == "." {
		return name, true
	}
	if len(name) > maxEntryPathLength || !fs.ValidPath(name) || strings.ContainsRune(name, '\\') {
		return "", false
	}
	for component := range strings.SplitSeq(name, "/") {
		if len(component) > maxPathComponent {
			return "", false
		}
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return "", false
		}
	}
	return name, true
}

func (x *extractor) ensureParents(name string) error {
	parent := path.Dir(name)
	if parent == "." {
		return nil
	}
	components := strings.Split(parent, "/")
	for i := range components {
		prefix := strings.Join(components[:i+1], "/")
		switch x.kinds[prefix] {
		case kindDirectory:
			continue
		case kindFile, kindSymlink:
			return entryInvalid(x.entries, "entry sits below a file or symbolic link")
		}
		if err := x.root.Mkdir(prefix, 0o700); err != nil {
			return ioError("extract archive", err)
		}
		x.kinds[prefix] = kindDirectory
		x.dirs = append(x.dirs, prefix)
	}
	return nil
}

func (x *extractor) directory(name string) error {
	switch x.kinds[name] {
	case kindDirectory:
		return nil
	case kindFile, kindSymlink:
		return entryInvalid(x.entries, "entry name is used twice")
	}
	if err := x.root.Mkdir(name, 0o700); err != nil {
		return ioError("extract archive", err)
	}
	x.kinds[name] = kindDirectory
	x.dirs = append(x.dirs, name)
	return nil
}

func (x *extractor) file(ctx context.Context, name string, header *tar.Header, reader io.Reader) error {
	if x.kinds[name] != 0 {
		return entryInvalid(x.entries, "entry name is used twice")
	}
	if header.Size < 0 || header.Size > x.limits.maxExpanded-x.expanded {
		return entryInvalid(x.entries, "archive expands beyond the allowed size")
	}
	x.expanded += header.Size
	target, err := x.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return ioError("extract archive", err)
	}
	x.kinds[name] = kindFile
	written, copyErr := copyWithContext(ctx, target, io.LimitReader(reader, header.Size))
	closeErr := target.Close()
	if copyErr != nil {
		return archiveError(ctx, copyErr)
	}
	if written != header.Size {
		return entryInvalid(x.entries, "entry is truncated")
	}
	if closeErr != nil {
		return ioError("extract archive", closeErr)
	}
	perm := regularPerm
	if header.Mode&0o111 != 0 {
		perm = executablePerm
	}
	if err := x.root.Chmod(name, perm); err != nil {
		return ioError("extract archive", err)
	}
	return nil
}

func (x *extractor) symlink(name, target string) error {
	if x.kinds[name] != 0 {
		return entryInvalid(x.entries, "entry name is used twice")
	}
	if target == "" || len(target) > maxSymlinkTarget || strings.HasPrefix(target, "/") || strings.ContainsRune(target, 0) {
		return entryInvalid(x.entries, "symbolic link target is not a relative path")
	}
	if err := x.root.Symlink(target, name); err != nil {
		return ioError("extract archive", err)
	}
	x.kinds[name] = kindSymlink
	x.links[name] = target
	return nil
}

func (x *extractor) checkLinks() error {
	for name, target := range x.links {
		if !resolvesInside(x.links, name, target) {
			return Errorf(ErrorCodeArchiveInvalid, "a symbolic link points outside the archive root or loops")
		}
	}
	return nil
}

func resolvesInside(links map[string]string, name, target string) bool {
	stack := strings.Split(path.Dir(name), "/")
	if path.Dir(name) == "." {
		stack = stack[:0]
	}
	pending := splitTarget(target)
	hops := 0
	for len(pending) > 0 {
		component := pending[0]
		pending = pending[1:]
		switch component {
		case "", ".":
			continue
		case "..":
			if len(stack) == 0 {
				return false
			}
			stack = stack[:len(stack)-1]
			continue
		}
		stack = append(stack, component)
		next, isLink := links[strings.Join(stack, "/")]
		if !isLink {
			continue
		}
		hops++
		if hops > maxSymlinkHops {
			return false
		}
		stack = stack[:len(stack)-1]
		pending = append(splitTarget(next), pending...)
	}
	return true
}

func splitTarget(target string) []string {
	return strings.Split(target, "/")
}

func (x *extractor) seal() error {
	for _, dir := range x.dirs {
		if err := x.root.Chmod(dir, directoryPermission); err != nil {
			return ioError("extract archive", err)
		}
	}
	return nil
}
