package plugin

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	MaxBundleEntries   = 4096
	MaxBundleBytes     = 128 << 20
	MaxBundleFileBytes = 64 << 20
	maxBundleDepth     = 16
	maxBundlePathBytes = 512
	digestDomain       = "aura.plugin.digest.v1"
	DigestHexLength    = 64
	dirMode            = 0o700
	fileMode           = 0o600
	execFileMode       = 0o700
)

type treeFile struct {
	Path string
	Size int64
	Exec bool
}

type tree struct {
	Dirs  []string
	Files []treeFile
	Bytes int64
}

type Inspection struct {
	Manifest     *Manifest
	BundleSHA256 string
	Digest       string
	Files        int
	Bytes        int64
}

func ReadManifest(root *os.Root) (*Manifest, error) {
	if root == nil {
		return nil, Errorf(ErrInvalidArgument, "bundle root is required")
	}
	info, err := root.Lstat(ManifestFile)
	if err != nil {
		return nil, Errorf(ErrManifestInvalid, "the bundle has no %s", ManifestFile)
	}
	if !info.Mode().IsRegular() {
		return nil, Errorf(ErrPathInvalid, "%s must be a regular file", ManifestFile)
	}
	if info.Size() > MaxManifestBytes {
		return nil, Errorf(ErrManifestTooLarge, "the manifest exceeds %d bytes", MaxManifestBytes)
	}
	file, err := root.Open(ManifestFile)
	if err != nil {
		return nil, Errorf(ErrManifestInvalid, "the manifest cannot be opened")
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, MaxManifestBytes+1))
	if err != nil {
		return nil, Errorf(ErrManifestInvalid, "the manifest cannot be read")
	}
	return ParseManifest(data)
}

func validBundleName(name string) bool {
	if name == "" || !utf8.ValidString(name) || strings.ContainsAny(name, "\\/") {
		return false
	}
	return !strings.ContainsFunc(name, unicode.IsControl)
}

func scanTree(root *os.Root) (*tree, error) {
	found := &tree{}
	entries := 0
	var walk func(dir string, depth int) error
	walk = func(dir string, depth int) error {
		if depth > maxBundleDepth {
			return Errorf(ErrBundleTooLarge, "the bundle nests deeper than %d directories", maxBundleDepth)
		}
		listing, err := fs.ReadDir(root.FS(), dir)
		if err != nil {
			return Errorf(ErrPathInvalid, "a bundle directory cannot be read")
		}
		for _, entry := range listing {
			entries++
			if entries > MaxBundleEntries {
				return Errorf(ErrBundleTooLarge, "the bundle holds more than %d entries", MaxBundleEntries)
			}
			name := entry.Name()
			if !validBundleName(name) {
				return Errorf(ErrPathInvalid, "a bundle entry has an unsupported name")
			}
			rel := path.Join(dir, name)
			if dir == "." {
				rel = name
			}
			if len(rel) > maxBundlePathBytes {
				return Errorf(ErrPathInvalid, "a bundle path is too long")
			}
			switch kind := entry.Type(); {
			case kind&fs.ModeSymlink != 0:
				return Errorf(ErrPathInvalid, "the bundle contains a symbolic link")
			case kind.IsDir():
				found.Dirs = append(found.Dirs, rel)
				if err := walk(rel, depth+1); err != nil {
					return err
				}
			case kind.IsRegular():
				info, err := entry.Info()
				if err != nil {
					return Errorf(ErrPathInvalid, "a bundle file cannot be inspected")
				}
				if info.Size() > MaxBundleFileBytes {
					return Errorf(ErrBundleTooLarge, "a bundle file exceeds %d bytes", MaxBundleFileBytes)
				}
				if found.Bytes > MaxBundleBytes-info.Size() {
					return Errorf(ErrBundleTooLarge, "the bundle exceeds %d bytes", MaxBundleBytes)
				}
				found.Bytes += info.Size()
				found.Files = append(found.Files, treeFile{Path: rel, Size: info.Size(), Exec: info.Mode().Perm()&0o111 != 0})
			default:
				return Errorf(ErrPathInvalid, "the bundle contains a special file")
			}
		}
		return nil
	}
	if err := walk(".", 0); err != nil {
		return nil, err
	}
	return found, nil
}

func copyTree(src, dst *os.Root) error {
	found, err := scanTree(src)
	if err != nil {
		return err
	}
	for _, dir := range found.Dirs {
		if err := dst.Mkdir(dir, dirMode); err != nil {
			return Wrap(ErrStorage, err, "a bundle directory cannot be created")
		}
	}
	var copied int64
	for _, file := range found.Files {
		written, err := copyFile(src, dst, file)
		if err != nil {
			return err
		}
		copied += written
		if copied > MaxBundleBytes {
			return Errorf(ErrBundleTooLarge, "the bundle exceeds %d bytes", MaxBundleBytes)
		}
	}
	return nil
}

func copyFile(src, dst *os.Root, file treeFile) (int64, error) {
	in, err := src.Open(file.Path)
	if err != nil {
		return 0, Errorf(ErrPathInvalid, "a bundle file cannot be opened")
	}
	defer func() { _ = in.Close() }()
	info, err := in.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return 0, Errorf(ErrPathInvalid, "a bundle file is not a regular file")
	}
	mode := os.FileMode(fileMode)
	if file.Exec {
		mode = execFileMode
	}
	out, err := dst.OpenFile(file.Path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return 0, Wrap(ErrStorage, err, "a bundle file cannot be created")
	}
	written, copyErr := io.Copy(out, io.LimitReader(in, MaxBundleFileBytes+1))
	if copyErr == nil && written > MaxBundleFileBytes {
		copyErr = Errorf(ErrBundleTooLarge, "a bundle file exceeds %d bytes", MaxBundleFileBytes)
	}
	if copyErr == nil {
		copyErr = out.Sync()
	}
	closeErr := out.Close()
	if copyErr != nil {
		var coded *Error
		if errors.As(copyErr, &coded) {
			return 0, copyErr
		}
		return 0, Wrap(ErrStorage, copyErr, "a bundle file cannot be written")
	}
	if closeErr != nil {
		return 0, Wrap(ErrStorage, closeErr, "a bundle file cannot be written")
	}
	return written, nil
}

func hashFile(root *os.Root, file treeFile) (string, error) {
	in, err := root.Open(file.Path)
	if err != nil {
		return "", Errorf(ErrPathInvalid, "a bundle file cannot be opened")
	}
	defer func() { _ = in.Close() }()
	info, err := in.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return "", Errorf(ErrPathInvalid, "a bundle file is not a regular file")
	}
	hasher := sha256.New()
	read, err := io.Copy(hasher, io.LimitReader(in, MaxBundleFileBytes+1))
	if err != nil {
		return "", Errorf(ErrPathInvalid, "a bundle file cannot be read")
	}
	if read != file.Size {
		return "", Errorf(ErrPathInvalid, "a bundle file changed while it was read")
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

func Inspect(root *os.Root) (*Inspection, error) {
	if root == nil {
		return nil, Errorf(ErrInvalidArgument, "bundle root is required")
	}
	manifest, err := ReadManifest(root)
	if err != nil {
		return nil, err
	}
	found, err := scanTree(root)
	if err != nil {
		return nil, err
	}
	files := make([]treeFile, 0, len(found.Files))
	for _, file := range found.Files {
		if file.Path != ManifestFile {
			files = append(files, file)
		}
	}
	slices.SortFunc(files, func(a, b treeFile) int { return strings.Compare(a.Path, b.Path) })
	bundle := sha256.New()
	var bytes int64
	for _, file := range files {
		sum, err := hashFile(root, file)
		if err != nil {
			return nil, err
		}
		flag := '-'
		if file.Exec {
			flag = 'x'
		}
		_, _ = fmt.Fprintf(bundle, "%s\x00%d\x00%c\x00%s\n", file.Path, file.Size, flag, sum)
		bytes += file.Size
	}
	bundleSum := hex.EncodeToString(bundle.Sum(nil))
	digest, err := computeDigest(manifest, bundleSum)
	if err != nil {
		return nil, err
	}
	return &Inspection{Manifest: manifest, BundleSHA256: bundleSum, Digest: digest, Files: len(files), Bytes: bytes}, nil
}

func computeDigest(manifest *Manifest, bundleSum string) (string, error) {
	canonical, err := manifest.Canonical()
	if err != nil {
		return "", err
	}
	hasher := sha256.New()
	_, _ = hasher.Write([]byte(digestDomain + "\n"))
	_, _ = hasher.Write(canonical)
	_, _ = hasher.Write([]byte("\n" + bundleSum))
	return hex.EncodeToString(hasher.Sum(nil)), nil
}
