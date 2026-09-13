package skills

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"io/fs"
	"os"
	"path"
	"slices"
	"strings"
	"syscall"
	"unicode"
	"unicode/utf8"
)

const (
	maxPackageFiles      = 256
	maxPackageDepth      = 8
	maxPackageTotalBytes = 8 << 20
	maxScopeRunes        = 128
)

const (
	FindingSkillFileMissing    = "skill_file_missing"
	FindingTooManyFiles        = "too_many_files"
	FindingTreeTooDeep         = "tree_too_deep"
	FindingPackageTooLarge     = "package_too_large"
	FindingPathInvalid         = "path_invalid"
	FindingCaseCollision       = "case_collision"
	FindingSymlinkEscape       = "symlink_escape"
	FindingSymlinkUnresolvable = "symlink_unresolvable"
	FindingSymlinkDirectory    = "symlink_directory"
	FindingHardLink            = "hard_link"
	FindingSpecialFile         = "special_file"
)

type FileEntry struct {
	Path   string
	Size   int64
	Digest string
}

type ScannedDir struct {
	Scope     string
	Name      string
	Manifest  *Manifest
	Digest    string
	Files     []FileEntry
	SizeBytes int64
}

type ScanResult struct {
	Scanned  *ScannedDir
	Findings []string
	Err      error
}

func ScanDir(ctx context.Context, dir, scope string) ScanResult {
	if ctx == nil {
		return ScanResult{Err: errNilArgument("ctx")}
	}
	if !validScope(scope) {
		return ScanResult{Err: Errorf(ErrorCodeInvalidArgument, "scope is not valid")}
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return ScanResult{Err: Errorf(ErrorCodeInvalidArgument, "skill directory is not accessible")}
	}
	defer func() { _ = root.Close() }()
	walker := &dirWalker{root: root, seen: make(map[string]string)}
	if err := walker.walk(ctx, ".", 0); err != nil {
		return ScanResult{Err: err}
	}
	if len(walker.findings) > 0 {
		return ScanResult{Findings: walker.findings}
	}
	scanned := &ScannedDir{
		Scope:     scope,
		Files:     walker.files,
		SizeBytes: walker.total,
	}
	scanned.Digest = digestPackage(walker.files, walker.contents)
	skillBytes, ok := walker.contents["SKILL.md"]
	if !ok {
		scanned.Name = dirBase(dir)
		return ScanResult{Scanned: scanned, Findings: []string{FindingSkillFileMissing}}
	}
	manifest, findings := ParseSkillFile(skillBytes)
	if len(findings) > 0 {
		scanned.Name = dirBase(dir)
		return ScanResult{Scanned: scanned, Findings: findings}
	}
	scanned.Name = manifest.Name
	scanned.Manifest = manifest
	return ScanResult{Scanned: scanned}
}

func dirBase(dir string) string {
	base := path.Base(strings.TrimSuffix(dir, "/"))
	if base == "" || base == "." || base == "/" {
		return "unknown"
	}
	return base
}

func validScope(scope string) bool {
	if scope == "" || len([]rune(scope)) > maxScopeRunes {
		return false
	}
	for _, r := range scope {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return false
		}
	}
	return true
}

type dirWalker struct {
	root     *os.Root
	seen     map[string]string
	files    []FileEntry
	contents map[string][]byte
	total    int64
	count    int
	findings []string
}

func (w *dirWalker) openDir(rel string) (*os.File, bool) {
	handle, err := w.root.Open(rel)
	if err != nil {
		return nil, false
	}
	return handle, true
}

func readDirEntries(handle *os.File) ([]fs.DirEntry, bool) {
	entries, err := handle.ReadDir(-1)
	if err != nil {
		return nil, false
	}
	return entries, true
}

func (w *dirWalker) fail(finding string) {
	w.findings = append(w.findings, finding)
}

func (w *dirWalker) lstat(child string) (fs.FileInfo, bool) {
	info, err := w.root.Lstat(child)
	if err != nil {
		return nil, false
	}
	return info, true
}

func (w *dirWalker) openFile(child string) (*os.File, bool) {
	handle, err := w.root.Open(child)
	if err != nil {
		return nil, false
	}
	return handle, true
}

func readCapped(handle *os.File) ([]byte, bool) {
	content, err := io.ReadAll(io.LimitReader(handle, maxSkillFileBytes+1))
	if err != nil {
		return nil, false
	}
	return content, true
}

func (w *dirWalker) walk(ctx context.Context, rel string, depth int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	handle, ok := w.openDir(rel)
	if !ok {
		w.fail(FindingSymlinkUnresolvable)
		return nil
	}
	entries, ok := readDirEntries(handle)
	_ = handle.Close()
	if !ok {
		w.fail(FindingSymlinkUnresolvable)
		return nil
	}
	for _, entry := range entries {
		if len(w.findings) > 0 {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		name := entry.Name()
		child := name
		if rel != "." {
			child = rel + "/" + name
		}
		if !validComponent(name) {
			w.fail(FindingPathInvalid)
			return nil
		}
		folded := strings.ToLower(child)
		if first, exists := w.seen[folded]; exists && first != child {
			w.fail(FindingCaseCollision)
			return nil
		}
		w.seen[folded] = child
		w.count++
		if w.count > maxPackageFiles {
			w.fail(FindingTooManyFiles)
			return nil
		}
		info, ok := w.lstat(child)
		if !ok {
			w.fail(FindingSymlinkUnresolvable)
			return nil
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			w.followLink(child)
			continue
		}
		if info.IsDir() {
			if depth+1 > maxPackageDepth {
				w.fail(FindingTreeTooDeep)
				return nil
			}
			if err := w.walk(ctx, child, depth+1); err != nil {
				return err
			}
			continue
		}
		if !info.Mode().IsRegular() {
			w.fail(FindingSpecialFile)
			return nil
		}
		w.readFile(child, info)
	}
	return nil
}

func (w *dirWalker) followLink(child string) {
	target, err := w.root.Readlink(child)
	if err != nil || !utf8.ValidString(target) {
		w.fail(FindingSymlinkUnresolvable)
		return
	}
	resolved := target
	if !path.IsAbs(target) {
		resolved = path.Join(path.Dir(child), target)
	}
	if resolved == ".." || strings.HasPrefix(resolved, "../") || path.IsAbs(resolved) {
		w.fail(FindingSymlinkEscape)
		return
	}
	info, err := w.root.Stat(child)
	if err != nil {
		w.fail(FindingSymlinkUnresolvable)
		return
	}
	if info.IsDir() {
		w.fail(FindingSymlinkDirectory)
		return
	}
	if !info.Mode().IsRegular() {
		w.fail(FindingSpecialFile)
		return
	}
	w.readFile(child, info)
}

func (w *dirWalker) readFile(child string, info fs.FileInfo) {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && stat.Nlink > 1 {
		w.fail(FindingHardLink)
		return
	}
	if w.total+info.Size() > maxPackageTotalBytes {
		w.fail(FindingPackageTooLarge)
		return
	}
	handle, ok := w.openFile(child)
	if !ok {
		w.fail(FindingSymlinkUnresolvable)
		return
	}
	content, ok := readCapped(handle)
	_ = handle.Close()
	if !ok {
		w.fail(FindingSymlinkUnresolvable)
		return
	}
	if len(content) > maxSkillFileBytes {
		w.fail(FindingFileTooLarge)
		return
	}
	if w.total+int64(len(content)) > maxPackageTotalBytes {
		w.fail(FindingPackageTooLarge)
		return
	}
	sum := sha256.Sum256(content)
	w.total += int64(len(content))
	entry := FileEntry{Path: child, Size: int64(len(content)), Digest: hex.EncodeToString(sum[:])}
	w.files = append(w.files, entry)
	if w.contents == nil {
		w.contents = make(map[string][]byte)
	}
	w.contents[child] = content
}

func validComponent(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	if !utf8.ValidString(name) || strings.Contains(name, "/") || strings.Contains(name, "\\") {
		return false
	}
	for _, r := range name {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return false
		}
	}
	return true
}

func digestPackage(files []FileEntry, contents map[string][]byte) string {
	ordered := slices.Clone(files)
	slices.SortFunc(ordered, func(a, b FileEntry) int { return strings.Compare(a.Path, b.Path) })
	writer := sha256.New()
	_, _ = writer.Write([]byte("skills-package/v1\n"))
	for _, entry := range ordered {
		_, _ = writer.Write([]byte(entry.Path))
		_, _ = writer.Write([]byte{0})
		_, _ = writer.Write([]byte(entry.Digest))
		_, _ = writer.Write([]byte{0})
		_, _ = writer.Write(contents[entry.Path])
		_, _ = writer.Write([]byte{0})
	}
	return hex.EncodeToString(writer.Sum(nil))
}
