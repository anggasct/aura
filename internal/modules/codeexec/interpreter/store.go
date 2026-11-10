package interpreter

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const (
	runtimesDirName = "runtimes"
	bundleDirName   = "bundle"
	digestFileName  = "tree.sha256"
	archiveCopyName = "archive.tar.gz"
	tempPrefix      = ".install-"
	digestFileBytes = 65
	runtimeDirMode  = fs.FileMode(0o700)
	sealedDirMode   = fs.FileMode(0o555)
	sealedFileMode  = fs.FileMode(0o444)
	writableDirMode = fs.FileMode(0o700)
	archiveCopyMode = fs.FileMode(0o600)
)

var errNotRegular = errors.New("not a regular file")

type Store struct {
	dataRoot string
	manifest *Manifest
	limits   extractLimits
}

type InstallResult struct {
	Runtime string
	Version string
}

type Removal struct {
	Runtime   string
	Versions  []string
	Temporary int
	Applied   bool
}

func NewStore(dataRoot string, manifest *Manifest) (*Store, error) {
	if manifest == nil {
		return nil, Errorf(ErrorCodeInvalidArgument, "manifest is required")
	}
	if dataRoot == "" {
		return nil, Errorf(ErrorCodeInvalidArgument, "data directory is required")
	}
	absolute, err := filepath.Abs(dataRoot)
	if err != nil {
		return nil, ioError("resolve data directory", err)
	}
	return &Store{dataRoot: absolute, manifest: manifest, limits: defaultExtractLimits()}, nil
}

func (s *Store) Manifest() *Manifest {
	return s.manifest
}

type Bundle struct {
	Runtime    Runtime
	Dir        string
	Executable string
}

func (s *Store) Bundle(id string) (Bundle, error) {
	runtime, err := s.lookup(id)
	if err != nil {
		return Bundle{}, err
	}
	dir := filepath.Join(s.runtimesPath(), runtime.ID, runtime.Version, bundleDirName)
	return Bundle{Runtime: runtime, Dir: dir, Executable: filepath.Join(dir, filepath.FromSlash(runtime.Executable))}, nil
}

func (s *Store) runtimesPath() string {
	return filepath.Join(s.dataRoot, runtimesDirName)
}

func (s *Store) lookup(id string) (Runtime, error) {
	runtime, ok := s.manifest.Lookup(id)
	if !ok {
		return Runtime{}, Errorf(ErrorCodeRuntimeUnknown, "no runtime named %q is pinned in this build", truncate(id))
	}
	return runtime, nil
}

func truncate(text string) string {
	const limit = 32
	if len(text) > limit {
		return text[:limit]
	}
	return text
}

func (s *Store) List(ctx context.Context) ([]Verification, error) {
	var out []Verification
	for _, id := range s.manifest.IDs() {
		verification, err := s.Verify(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, verification)
	}
	return out, nil
}

func (s *Store) Verify(ctx context.Context, id string) (Verification, error) {
	runtime, err := s.lookup(id)
	if err != nil {
		return Verification{}, err
	}
	result := Verification{Runtime: runtime.ID, Version: runtime.Version}
	versionRoot, state, reason := s.openVersion(&runtime)
	if versionRoot == nil {
		result.State, result.Reason = state, reason
		return result, nil
	}
	defer func() { _ = versionRoot.Close() }()
	reason, err = verifyVersion(ctx, versionRoot, &runtime, s.limits)
	if err != nil {
		return Verification{}, err
	}
	result.State, result.Reason = StateInstalled, reason
	if reason != ReasonNone {
		result.State = StateInvalid
	}
	return result, nil
}

func (s *Store) openVersion(runtime *Runtime) (*os.Root, State, Reason) {
	runtimesRoot, err := os.OpenRoot(s.runtimesPath())
	if err != nil {
		state, reason := openFailure(err)
		return nil, state, reason
	}
	defer func() { _ = runtimesRoot.Close() }()
	idRoot, err := runtimesRoot.OpenRoot(runtime.ID)
	if err != nil {
		state, reason := openFailure(err)
		return nil, state, reason
	}
	defer func() { _ = idRoot.Close() }()
	versionRoot, err := idRoot.OpenRoot(runtime.Version)
	if err != nil {
		state, reason := openFailure(err)
		return nil, state, reason
	}
	return versionRoot, StateInstalled, ReasonNone
}

func openFailure(err error) (State, Reason) {
	if errors.Is(err, fs.ErrNotExist) {
		return StateAbsent, ReasonNone
	}
	return StateInvalid, ReasonTreeUnreadable
}

func verifyVersion(ctx context.Context, versionRoot *os.Root, runtime *Runtime, limits extractLimits) (Reason, error) {
	bundle, err := versionRoot.OpenRoot(bundleDirName)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return ReasonBundleMissing, nil
		}
		return ReasonTreeUnreadable, nil
	}
	defer func() { _ = bundle.Close() }()
	recorded, reason := readRecordedDigest(versionRoot)
	if reason != ReasonNone {
		return reason, nil
	}
	actual, err := treeDigest(ctx, bundle, runtime, limits)
	if err != nil {
		if reason, ok := asTreeReason(err); ok {
			return reason, nil
		}
		return ReasonNone, err
	}
	if subtle.ConstantTimeCompare([]byte(recorded), []byte(actual)) != 1 {
		return ReasonDigestMismatch, nil
	}
	if !executableReady(bundle, runtime) {
		return ReasonExecutableMissing, nil
	}
	return ReasonNone, nil
}

func readRecordedDigest(versionRoot *os.Root) (string, Reason) {
	file, err := versionRoot.Open(digestFileName)
	if err != nil {
		return "", ReasonDigestMissing
	}
	defer func() { _ = file.Close() }()
	content, err := io.ReadAll(io.LimitReader(file, digestFileBytes+1))
	if err != nil || len(content) != digestFileBytes || content[digestFileBytes-1] != '\n' {
		return "", ReasonDigestMissing
	}
	recorded := string(content[:digestFileBytes-1])
	if !sha256HexPattern.MatchString(recorded) {
		return "", ReasonDigestMissing
	}
	return recorded, ReasonNone
}

func executableReady(bundle *os.Root, runtime *Runtime) bool {
	info, err := bundle.Stat(runtime.Executable)
	return err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0
}

func (s *Store) Install(ctx context.Context, id, archivePath string) (InstallResult, error) {
	runtime, err := s.lookup(id)
	if err != nil {
		return InstallResult{}, err
	}
	if archivePath == "" {
		return InstallResult{}, Errorf(ErrorCodeInvalidArgument, "an archive path is required")
	}
	runtimesRoot, err := s.openRuntimesRoot()
	if err != nil {
		return InstallResult{}, err
	}
	defer func() { _ = runtimesRoot.Close() }()
	lock, err := acquireInstallLock(runtimesRoot)
	if err != nil {
		return InstallResult{}, err
	}
	defer lock.release()
	idRoot, err := openRuntimeDir(runtimesRoot, runtime.ID)
	if err != nil {
		return InstallResult{}, err
	}
	defer func() { _ = idRoot.Close() }()
	if err := sweepTemporary(idRoot); err != nil {
		return InstallResult{}, err
	}
	if _, err := idRoot.Lstat(runtime.Version); err == nil {
		return InstallResult{}, Errorf(ErrorCodeAlreadyInstalled, "%s %s is already installed; remove it first", runtime.ID, runtime.Version)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return InstallResult{}, ioError("inspect installed runtimes", err)
	}
	staged, err := s.stage(ctx, idRoot, &runtime, archivePath)
	if err != nil {
		return InstallResult{}, err
	}
	if err := commitStaged(idRoot, staged, runtime.Version); err != nil {
		return InstallResult{}, err
	}
	return InstallResult{Runtime: runtime.ID, Version: runtime.Version}, nil
}

func (s *Store) openRuntimesRoot() (*os.Root, error) {
	if err := os.MkdirAll(s.runtimesPath(), runtimeDirMode); err != nil {
		return nil, ioError("create runtimes directory", err)
	}
	root, err := os.OpenRoot(s.runtimesPath())
	if err != nil {
		return nil, ioError("open runtimes directory", err)
	}
	return root, nil
}

func openRuntimeDir(runtimesRoot *os.Root, id string) (*os.Root, error) {
	if err := runtimesRoot.Mkdir(id, runtimeDirMode); err != nil && !errors.Is(err, fs.ErrExist) {
		return nil, ioError("create runtime directory", err)
	}
	root, err := runtimesRoot.OpenRoot(id)
	if err != nil {
		return nil, ioError("open runtime directory", err)
	}
	return root, nil
}

func sweepTemporary(idRoot *os.Root) error {
	entries, err := fs.ReadDir(idRoot.FS(), ".")
	if err != nil {
		return ioError("inspect installed runtimes", err)
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), tempPrefix) {
			continue
		}
		if err := removeTree(idRoot, entry.Name()); err != nil {
			return ioError("remove an interrupted install", err)
		}
	}
	return nil
}

func temporaryName() string {
	return tempPrefix + strings.ToLower(rand.Text())
}

func (s *Store) stage(ctx context.Context, idRoot *os.Root, runtime *Runtime, archivePath string) (string, error) {
	name := temporaryName()
	if err := idRoot.Mkdir(name, runtimeDirMode); err != nil {
		return "", ioError("create install directory", err)
	}
	if err := s.fill(ctx, idRoot, name, runtime, archivePath); err != nil {
		_ = removeTree(idRoot, name)
		return "", err
	}
	return name, nil
}

func (s *Store) fill(ctx context.Context, idRoot *os.Root, name string, runtime *Runtime, archivePath string) error {
	staging, err := idRoot.OpenRoot(name)
	if err != nil {
		return ioError("open install directory", err)
	}
	defer func() { _ = staging.Close() }()
	if err := copyVerifiedArchive(ctx, staging, runtime, archivePath); err != nil {
		return err
	}
	if err := s.extractStaged(ctx, staging, runtime); err != nil {
		return err
	}
	if err := staging.Remove(archiveCopyName); err != nil {
		return ioError("remove archive copy", err)
	}
	if err := idRoot.Chmod(name, sealedDirMode); err != nil {
		return ioError("seal install directory", err)
	}
	return nil
}

func (s *Store) extractStaged(ctx context.Context, staging *os.Root, runtime *Runtime) error {
	if err := staging.Mkdir(bundleDirName, runtimeDirMode); err != nil {
		return ioError("create bundle directory", err)
	}
	bundle, err := staging.OpenRoot(bundleDirName)
	if err != nil {
		return ioError("open bundle directory", err)
	}
	defer func() { _ = bundle.Close() }()
	archive, err := staging.Open(archiveCopyName)
	if err != nil {
		return ioError("open archive copy", err)
	}
	defer func() { _ = archive.Close() }()
	if err := extractArchive(ctx, archive, bundle, s.limits); err != nil {
		return err
	}
	if err := staging.Chmod(bundleDirName, sealedDirMode); err != nil {
		return ioError("seal bundle directory", err)
	}
	if !executableReady(bundle, runtime) {
		return Errorf(ErrorCodeArchiveInvalid, "archive does not contain the pinned %s executable", runtime.ID)
	}
	digest, err := treeDigest(ctx, bundle, runtime, s.limits)
	if err != nil {
		if _, ok := asTreeReason(err); ok {
			return Errorf(ErrorCodeArchiveInvalid, "archive contents are not acceptable")
		}
		return err
	}
	return writeDigest(staging, digest)
}

func writeDigest(staging *os.Root, digest string) error {
	file, err := staging.OpenFile(digestFileName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, archiveCopyMode)
	if err != nil {
		return ioError("write tree digest", err)
	}
	if _, err := file.WriteString(digest + "\n"); err != nil {
		_ = file.Close()
		return ioError("write tree digest", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return ioError("write tree digest", err)
	}
	if err := file.Close(); err != nil {
		return ioError("write tree digest", err)
	}
	if err := staging.Chmod(digestFileName, sealedFileMode); err != nil {
		return ioError("write tree digest", err)
	}
	return nil
}

func copyVerifiedArchive(ctx context.Context, staging *os.Root, runtime *Runtime, archivePath string) error {
	source, err := openArchive(archivePath)
	if err != nil {
		return Errorf(ErrorCodeArchiveInvalid, "cannot open archive %q", filepath.Base(archivePath))
	}
	defer func() { _ = source.Close() }()
	info, err := source.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return Errorf(ErrorCodeArchiveInvalid, "archive %q is not a regular file", filepath.Base(archivePath))
	}
	if info.Size() > maxArchiveBytes {
		return Errorf(ErrorCodeArchiveInvalid, "archive is larger than %d bytes", maxArchiveBytes)
	}
	target, err := staging.OpenFile(archiveCopyName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, archiveCopyMode)
	if err != nil {
		return ioError("copy archive", err)
	}
	hash := sha256.New()
	written, copyErr := copyWithContext(ctx, io.MultiWriter(target, hash), io.LimitReader(source, maxArchiveBytes+1))
	closeErr := target.Close()
	switch {
	case copyErr != nil:
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return ioError("copy archive", copyErr)
	case closeErr != nil:
		return ioError("copy archive", closeErr)
	case written > maxArchiveBytes:
		return Errorf(ErrorCodeArchiveInvalid, "archive is larger than %d bytes", maxArchiveBytes)
	}
	got := hex.EncodeToString(hash.Sum(nil))
	if subtle.ConstantTimeCompare([]byte(got), []byte(runtime.SHA256)) != 1 {
		return Errorf(ErrorCodeDigestMismatch, "archive SHA-256 %s does not match the pinned %s for %s %s", got, runtime.SHA256, runtime.ID, runtime.Version)
	}
	return nil
}

func openArchive(archivePath string) (*os.File, error) {
	resolved, err := filepath.EvalSymlinks(archivePath)
	if err != nil {
		return nil, err
	}
	parent, err := os.OpenRoot(filepath.Dir(resolved))
	if err != nil {
		return nil, err
	}
	defer func() { _ = parent.Close() }()
	info, err := parent.Stat(filepath.Base(resolved))
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errNotRegular
	}
	return parent.Open(filepath.Base(resolved))
}

func commitStaged(idRoot *os.Root, name, version string) error {
	if err := idRoot.Rename(name, version); err != nil {
		_ = removeTree(idRoot, name)
		return ioError("move runtime into place", err)
	}
	directory, err := idRoot.Open(".")
	if err != nil {
		return ioError("sync runtime directory", err)
	}
	defer func() { _ = directory.Close() }()
	if err := directory.Sync(); err != nil {
		return ioError("sync runtime directory", err)
	}
	return nil
}

func (s *Store) Remove(ctx context.Context, id string, apply bool) (Removal, error) {
	runtime, err := s.lookup(id)
	if err != nil {
		return Removal{}, err
	}
	plan := Removal{Runtime: runtime.ID}
	runtimesRoot, err := os.OpenRoot(s.runtimesPath())
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return plan, nil
		}
		return Removal{}, ioError("open runtimes directory", err)
	}
	defer func() { _ = runtimesRoot.Close() }()
	if apply {
		lock, err := acquireInstallLock(runtimesRoot)
		if err != nil {
			return Removal{}, err
		}
		defer lock.release()
	}
	idRoot, err := runtimesRoot.OpenRoot(runtime.ID)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return plan, nil
		}
		return Removal{}, ioError("open runtime directory", err)
	}
	defer func() { _ = idRoot.Close() }()
	entries, err := fs.ReadDir(idRoot.FS(), ".")
	if err != nil {
		return Removal{}, ioError("inspect installed runtimes", err)
	}
	var doomed []string
	for _, entry := range entries {
		doomed = append(doomed, entry.Name())
		if strings.HasPrefix(entry.Name(), tempPrefix) {
			plan.Temporary++
			continue
		}
		plan.Versions = append(plan.Versions, entry.Name())
	}
	if !apply {
		return plan, nil
	}
	for _, name := range doomed {
		if err := ctx.Err(); err != nil {
			return plan, err
		}
		if err := discard(idRoot, name); err != nil {
			return plan, ioError("remove runtime", err)
		}
	}
	plan.Applied = true
	return plan, nil
}

func discard(idRoot *os.Root, name string) error {
	if strings.HasPrefix(name, tempPrefix) {
		return removeTree(idRoot, name)
	}
	holding := temporaryName()
	if err := idRoot.Rename(name, holding); err != nil {
		return err
	}
	return removeTree(idRoot, holding)
}

func removeTree(parent *os.Root, name string) error {
	info, err := parent.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.IsDir() {
		if err := makeWritable(parent, name); err != nil {
			return err
		}
	}
	return parent.RemoveAll(name)
}

func makeWritable(parent *os.Root, name string) error {
	if err := parent.Chmod(name, writableDirMode); err != nil {
		return err
	}
	tree, err := parent.OpenRoot(name)
	if err != nil {
		return err
	}
	defer func() { _ = tree.Close() }()
	return fs.WalkDir(tree.FS(), ".", func(entryPath string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() && entryPath != "." {
			return tree.Chmod(entryPath, writableDirMode)
		}
		return nil
	})
}
