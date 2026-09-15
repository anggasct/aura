package sync

import (
	"context"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

type PromotePlan struct {
	Entries []FetchEntry
	Digest  string
}

type RollbackManifest struct {
	Digest  string
	Entries []RollbackEntry
}

type RollbackEntry struct {
	Path    string
	Existed bool
	Size    int64
	Digest  string
}

func PlanPromotion(snapshot FetchSnapshot, decision AdvanceDecision, skillReview func(path string) bool) (PromotePlan, RollbackManifest, error) {
	if decision != AdvanceFastForward {
		return PromotePlan{}, RollbackManifest{}, Errorf(ErrorCodeConflict, "promotion requires a fast-forward decision")
	}
	if strings.TrimSpace(snapshot.Ref) == "" {
		return PromotePlan{}, RollbackManifest{}, Errorf(ErrorCodeInvalidArgument, "fetch ref must not be empty")
	}
	plan := PromotePlan{Digest: snapshot.Digest}
	rollback := RollbackManifest{Digest: snapshot.Digest}
	ordered := slices.Clone(snapshot.Entries)
	slices.SortFunc(ordered, func(a, b FetchEntry) int { return strings.Compare(a.Path, b.Path) })
	for _, entry := range ordered {
		if strings.HasPrefix(entry.Path, "skills/") && skillReview != nil && skillReview(entry.Path) {
			continue
		}
		plan.Entries = append(plan.Entries, entry)
		rollback.Entries = append(rollback.Entries, RollbackEntry{Path: entry.Path})
	}
	return plan, rollback, nil
}

func ApplyPromotion(ctx context.Context, target string, plan PromotePlan, contents map[string][]byte, rollback *RollbackManifest) error {
	if ctx == nil {
		return errNilArgument("ctx")
	}
	if strings.TrimSpace(target) == "" {
		return Errorf(ErrorCodeInvalidArgument, "promotion target must not be empty")
	}
	if len(plan.Entries) == 0 {
		return nil
	}
	handle, err := os.OpenRoot(target)
	if err != nil {
		return Errorf(ErrorCodeManifestInvalid, "promotion target is not accessible")
	}
	defer func() { _ = handle.Close() }()
	staged := make([]stagedFile, 0, len(plan.Entries))
	for _, entry := range plan.Entries {
		if err := ctx.Err(); err != nil {
			removeStaged(handle, staged)
			return err
		}
		content, ok := contents[entry.Path]
		if !ok || int64(len(content)) != entry.Size {
			removeStaged(handle, staged)
			return Errorf(ErrorCodeConflict, "promotion content does not match its plan")
		}
		if DeniedPath(entry.Path) {
			removeStaged(handle, staged)
			return Errorf(ErrorCodePathDenied, "entry is not exportable")
		}
		recorded := recordPrior(handle, entry.Path)
		if rollback != nil {
			rollback.Entries = append(rollback.Entries, recorded)
		}
		stagedPath, err := stageFile(handle, entry.Path, content)
		if err != nil {
			removeStaged(handle, staged)
			return err
		}
		staged = append(staged, stagedFile{path: entry.Path, staged: stagedPath})
	}
	for _, file := range staged {
		if err := promoteStaged(handle, file); err != nil {
			return err
		}
	}
	if err := syncRoot(handle); err != nil {
		return err
	}
	return nil
}

type stagedFile struct {
	path   string
	staged string
}

func recordPrior(handle *os.Root, rel string) RollbackEntry {
	record := RollbackEntry{Path: rel}
	info, err := handle.Stat(rel)
	if err != nil || !info.Mode().IsRegular() {
		return record
	}
	record.Existed = true
	record.Size = info.Size()
	return record
}

func stageFile(handle *os.Root, rel string, content []byte) (string, error) {
	dir := path.Dir(rel)
	if dir != "." {
		if err := handle.MkdirAll(dir, 0o700); err != nil {
			return "", Errorf(ErrorCodeManifestInvalid, "promotion target is not writable")
		}
	}
	tmp, err := os.CreateTemp(os.TempDir(), "sync-promote-*")
	if err != nil {
		return "", Errorf(ErrorCodeManifestInvalid, "promotion staging failed")
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(normalizeContent(content)); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return "", Errorf(ErrorCodeManifestInvalid, "promotion staging failed")
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return "", Errorf(ErrorCodeManifestInvalid, "promotion staging failed")
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return "", Errorf(ErrorCodeManifestInvalid, "promotion staging failed")
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		_ = os.Remove(tmpName)
		return "", Errorf(ErrorCodeManifestInvalid, "promotion staging failed")
	}
	return tmpName, nil
}

func promoteStaged(handle *os.Root, file stagedFile) error {
	data, err := readStaged(file.staged)
	_ = os.Remove(file.staged)
	if err != nil {
		return err
	}
	if err := writeAtomic(handle, file.path, data); err != nil {
		return err
	}
	return nil
}

func readStaged(staged string) ([]byte, error) {
	cleaned := filepath.Clean(staged)
	if !strings.HasPrefix(filepath.Base(cleaned), "sync-promote-") {
		return nil, Errorf(ErrorCodeManifestInvalid, "promotion staging failed")
	}
	data, err := os.ReadFile(cleaned)
	if err != nil {
		return nil, Errorf(ErrorCodeManifestInvalid, "promotion staging failed")
	}
	return data, nil
}

func writeAtomic(handle *os.Root, rel string, content []byte) error {
	dir := path.Dir(rel)
	if dir != "." {
		if err := handle.MkdirAll(dir, 0o700); err != nil {
			return Errorf(ErrorCodeManifestInvalid, "promotion target is not writable")
		}
	}
	staged, err := handle.Create(rel + ".tmp-sync")
	if err != nil {
		return Errorf(ErrorCodeManifestInvalid, "promotion target is not writable")
	}
	stagedName := rel + ".tmp-sync"
	if _, err := staged.Write(content); err != nil {
		_ = staged.Close()
		_ = handle.Remove(stagedName)
		return Errorf(ErrorCodeManifestInvalid, "promotion target is not writable")
	}
	if err := staged.Sync(); err != nil {
		_ = staged.Close()
		_ = handle.Remove(stagedName)
		return Errorf(ErrorCodeManifestInvalid, "promotion target is not writable")
	}
	if err := staged.Close(); err != nil {
		_ = handle.Remove(stagedName)
		return Errorf(ErrorCodeManifestInvalid, "promotion target is not writable")
	}
	if err := handle.Chmod(stagedName, 0o600); err != nil {
		_ = handle.Remove(stagedName)
		return Errorf(ErrorCodeManifestInvalid, "promotion target is not writable")
	}
	if err := handle.Rename(stagedName, rel); err != nil {
		_ = handle.Remove(stagedName)
		return Errorf(ErrorCodeManifestInvalid, "promotion target is not writable")
	}
	return nil
}

func removeStaged(handle *os.Root, staged []stagedFile) {
	for _, file := range staged {
		_ = os.Remove(file.staged)
	}
	_ = handle
}

func syncRoot(handle *os.Root) error {
	dir, err := handle.Open(".")
	if err != nil {
		return Errorf(ErrorCodeManifestInvalid, "promotion target is not accessible")
	}
	defer func() { _ = dir.Close() }()
	if err := dir.Sync(); err != nil {
		return Errorf(ErrorCodeManifestInvalid, "promotion target is not durable")
	}
	return nil
}
