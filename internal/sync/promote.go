package sync

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"path"
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

func isPromotedSkill(path string) bool {
	return strings.HasPrefix(path, "skills/")
}

func PlanPromotion(snapshot FetchSnapshot, decision AdvanceDecision, skillReview func(path string) bool) (PromotePlan, RollbackManifest, error) {
	if decision != AdvanceFastForward {
		return PromotePlan{}, RollbackManifest{}, Errorf(ErrorCodeConflict, "promotion requires a fast-forward decision")
	}
	if strings.TrimSpace(snapshot.Ref) == "" {
		return PromotePlan{}, RollbackManifest{}, Errorf(ErrorCodeInvalidArgument, "fetch ref must not be empty")
	}
	plan := PromotePlan{Digest: snapshot.Digest}
	rollback := RollbackManifest{}
	ordered := slices.Clone(snapshot.Entries)
	slices.SortFunc(ordered, func(a, b FetchEntry) int { return strings.Compare(a.Path, b.Path) })
	for _, entry := range ordered {
		if isPromotedSkill(entry.Path) && (skillReview == nil || skillReview(entry.Path)) {
			continue
		}
		plan.Entries = append(plan.Entries, entry)
	}
	return plan, rollback, nil
}

type appliedEntry struct {
	path       string
	existed    bool
	priorBytes []byte
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
	priors := make([]RollbackEntry, 0, len(plan.Entries))
	priorBytesByPath := make(map[string][]byte, len(plan.Entries))
	priorExistedByPath := make(map[string]bool, len(plan.Entries))
	for _, entry := range plan.Entries {
		if err := ctx.Err(); err != nil {
			removeStaged(handle, staged)
			return err
		}
		content, ok := contents[entry.Path]
		if !ok || int64(len(content)) != entry.Size || digestContent(content) != entry.Digest {
			removeStaged(handle, staged)
			return Errorf(ErrorCodeConflict, "promotion content does not match its plan")
		}
		if DeniedPath(entry.Path) {
			removeStaged(handle, staged)
			return Errorf(ErrorCodePathDenied, "entry is not exportable")
		}
		recorded, priorBytes := capturePrior(handle, entry.Path)
		priors = append(priors, recorded)
		if recorded.Existed && priorBytes != nil {
			priorBytesByPath[entry.Path] = priorBytes
		}
		priorExistedByPath[entry.Path] = recorded.Existed
		stagedPath, err := stageFile(handle, entry.Path, content)
		if err != nil {
			removeStaged(handle, staged)
			return err
		}
		staged = append(staged, stagedFile{path: entry.Path, staged: stagedPath})
	}
	if rollback != nil {
		rollback.Entries = append(rollback.Entries, priors...)
		priorExports := make([]ExportEntry, 0, len(priors))
		for _, prior := range priors {
			priorExports = append(priorExports, ExportEntry{Path: prior.Path, Size: prior.Size, Digest: prior.Digest})
		}
		rollback.Digest = digestSnapshot(priorExports)
	}
	var applied []appliedEntry
	for _, file := range staged {
		if err := promoteStaged(handle, file); err != nil {
			restorePromoted(handle, applied)
			removeStaged(handle, staged)
			return err
		}
		applied = append(applied, appliedEntry{
			path:       file.path,
			existed:    priorExistedByPath[file.path],
			priorBytes: priorBytesByPath[file.path],
		})
	}
	if err := syncRoot(handle); err != nil {
		restorePromoted(handle, applied)
		removeStaged(handle, staged)
		return err
	}
	return nil
}

type stagedFile struct {
	path   string
	staged string
}

func capturePrior(handle *os.Root, rel string) (RollbackEntry, []byte) {
	record := RollbackEntry{Path: rel}
	info, err := handle.Stat(rel)
	if err != nil || !info.Mode().IsRegular() {
		return record, nil
	}
	data, err := handle.ReadFile(rel)
	if err != nil {
		record.Existed = true
		record.Size = info.Size()
		return record, nil
	}
	record.Existed = true
	record.Size = int64(len(data))
	record.Digest = digestContent(data)
	kept := slices.Clone(data)
	return record, kept
}

func stageFile(handle *os.Root, rel string, content []byte) (string, error) {
	dir := path.Dir(rel)
	if dir != "." {
		if err := handle.MkdirAll(dir, 0o700); err != nil {
			return "", Errorf(ErrorCodeManifestInvalid, "promotion target is not writable")
		}
	}
	normalized := normalizeContent(content)
	for range 5 {
		var random [8]byte
		if _, err := rand.Read(random[:]); err != nil {
			return "", Errorf(ErrorCodeManifestInvalid, "promotion staging failed")
		}
		name := ".sync-promote-" + hex.EncodeToString(random[:]) + ".tmp"
		tmpRel := name
		if dir != "." {
			tmpRel = dir + "/" + name
		}
		staged, err := handle.OpenFile(tmpRel, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			if os.IsExist(err) {
				continue
			}
			return "", Errorf(ErrorCodeManifestInvalid, "promotion staging failed")
		}
		if _, err := staged.Write(normalized); err != nil {
			_ = staged.Close()
			_ = handle.Remove(tmpRel)
			return "", Errorf(ErrorCodeManifestInvalid, "promotion staging failed")
		}
		if err := staged.Sync(); err != nil {
			_ = staged.Close()
			_ = handle.Remove(tmpRel)
			return "", Errorf(ErrorCodeManifestInvalid, "promotion staging failed")
		}
		if err := staged.Close(); err != nil {
			_ = handle.Remove(tmpRel)
			return "", Errorf(ErrorCodeManifestInvalid, "promotion staging failed")
		}
		if err := handle.Chmod(tmpRel, 0o600); err != nil {
			_ = handle.Remove(tmpRel)
			return "", Errorf(ErrorCodeManifestInvalid, "promotion staging failed")
		}
		return tmpRel, nil
	}
	return "", Errorf(ErrorCodeManifestInvalid, "promotion staging failed")
}

func promoteStaged(handle *os.Root, file stagedFile) error {
	if err := handle.Rename(file.staged, file.path); err != nil {
		_ = handle.Remove(file.staged)
		return Errorf(ErrorCodeManifestInvalid, "promotion target is not writable")
	}
	if err := syncParent(handle, file.path); err != nil {
		return err
	}
	return nil
}

func restorePromoted(handle *os.Root, applied []appliedEntry) {
	for i := len(applied) - 1; i >= 0; i-- {
		entry := applied[i]
		if entry.existed {
			_ = writeAtomic(handle, entry.path, entry.priorBytes)
		} else {
			_ = handle.Remove(entry.path)
		}
	}
}

func writeAtomic(handle *os.Root, rel string, content []byte) error {
	dir := path.Dir(rel)
	if dir != "." {
		if err := handle.MkdirAll(dir, 0o700); err != nil {
			return Errorf(ErrorCodeManifestInvalid, "promotion target is not writable")
		}
	}
	stagedName := rel + ".tmp-sync"
	staged, err := handle.Create(stagedName)
	if err != nil {
		return Errorf(ErrorCodeManifestInvalid, "promotion target is not writable")
	}
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
	if err := syncParent(handle, rel); err != nil {
		return err
	}
	return nil
}

func removeStaged(handle *os.Root, staged []stagedFile) {
	for _, file := range staged {
		_ = handle.Remove(file.staged)
	}
}

func syncParent(handle *os.Root, rel string) error {
	dir := path.Dir(rel)
	if dir == "." {
		return nil
	}
	parent, err := handle.Open(dir)
	if err != nil {
		return Errorf(ErrorCodeManifestInvalid, "promotion target is not durable")
	}
	defer func() { _ = parent.Close() }()
	if err := parent.Sync(); err != nil {
		return Errorf(ErrorCodeManifestInvalid, "promotion target is not durable")
	}
	return nil
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
