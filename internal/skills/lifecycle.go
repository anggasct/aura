package skills

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const (
	pendingDirName   = "pending"
	maxSweepPerCycle = 16
	maxDoctorRows    = 1024
)

type DoctorRow struct {
	ID       string
	Name     string
	State    string
	Digest   string
	Origin   string
	Findings []string
}

type ConflictGroup struct {
	Name string
	IDs  []string
}

type DoctorReport struct {
	Rows      []DoctorRow
	Conflicts []ConflictGroup
}

func (e *Engine) StageDraft(ctx context.Context, name, description, root string) (ScanSummary, error) {
	if ctx == nil {
		return ScanSummary{}, errNilArgument("ctx")
	}
	if !validSkillName(name) {
		return ScanSummary{}, Errorf(ErrorCodeInvalidArgument, "skill name is not valid")
	}
	if !validSkillDescription(description) {
		return ScanSummary{}, Errorf(ErrorCodeInvalidArgument, "skill description is not valid")
	}
	scope, ok := e.scopeForRoot(root)
	if !ok {
		return ScanSummary{}, Errorf(ErrorCodeInvalidArgument, "staging root is not configured")
	}
	pending := filepath.Join(root, pendingDirName)
	if err := os.MkdirAll(pending, 0o700); err != nil {
		return ScanSummary{}, codedError(ErrorCodeSkillUnavailable, "prepare staging area", err)
	}
	pkgdir := filepath.Join(pending, name)
	if _, err := os.Lstat(pkgdir); err == nil {
		return ScanSummary{}, Errorf(ErrorCodeInvalidArgument, "skill draft already exists")
	}
	document := "---\nname: " + name + "\ndescription: " + description + "\n---\n# " + name + "\n\n" + description + "\n"
	if err := atomicWriteFile(pkgdir, "SKILL.md", []byte(document)); err != nil {
		return ScanSummary{}, err
	}
	result := ScanDir(ctx, pkgdir, scope)
	summary, findings, err := registerResult(ctx, e.registry, pkgdir, scope, OriginPending, result)
	if err != nil {
		return ScanSummary{}, err
	}
	if len(findings) > 0 {
		return ScanSummary{}, Errorf(ErrorCodeSkillInvalid, "staged draft is not valid")
	}
	return summary, nil
}

func (e *Engine) scopeForRoot(root string) (string, bool) {
	for _, configured := range e.roots {
		if configured.dir == root {
			return configured.scope, true
		}
	}
	return "", false
}

func atomicWriteFile(dir, name string, content []byte) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return codedError(ErrorCodeSkillUnavailable, "prepare skill directory", err)
	}
	tmp, err := os.CreateTemp(dir, ".draft-*")
	if err != nil {
		return codedError(ErrorCodeSkillUnavailable, "stage skill draft", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return codedError(ErrorCodeSkillUnavailable, "stage skill draft", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return codedError(ErrorCodeSkillUnavailable, "stage skill draft", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return codedError(ErrorCodeSkillUnavailable, "stage skill draft", err)
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		_ = os.Remove(tmpName)
		return codedError(ErrorCodeSkillUnavailable, "stage skill draft", err)
	}
	if err := os.Rename(tmpName, filepath.Join(dir, name)); err != nil {
		_ = os.Remove(tmpName)
		return codedError(ErrorCodeSkillUnavailable, "stage skill draft", err)
	}
	return nil
}

func (e *Engine) Disable(ctx context.Context, id string) error {
	if ctx == nil {
		return errNilArgument("ctx")
	}
	if strings.TrimSpace(id) == "" {
		return Errorf(ErrorCodeInvalidArgument, "skill id must not be empty")
	}
	record, err := e.registry.Get(ctx, id)
	if err != nil {
		return err
	}
	if record.State != StateActive {
		return Errorf(ErrorCodeSkillInvalid, "skill state does not allow the transition")
	}
	if err := e.registry.DisableSkill(ctx, id); err != nil {
		return err
	}
	e.evict(id)
	e.logger.InfoContext(ctx, "skill disabled",
		"component", "skills",
		"skill_id", id)
	return nil
}

func (e *Engine) ValidateDir(ctx context.Context, dir string) (digest string, findings []string, err error) {
	if ctx == nil {
		return "", nil, errNilArgument("ctx")
	}
	result := ScanDir(ctx, dir, "validate")
	if result.Err != nil {
		return "", nil, result.Err
	}
	if result.Scanned == nil {
		return "", result.Findings, nil
	}
	return result.Scanned.Digest, result.Findings, nil
}

func (e *Engine) Doctor(ctx context.Context) (DoctorReport, error) {
	if ctx == nil {
		return DoctorReport{}, errNilArgument("ctx")
	}
	records, err := e.registry.ListAll(ctx, maxDoctorRows)
	if err != nil {
		return DoctorReport{}, err
	}
	conflicts := findConflicts(records)
	conflicted := make(map[string]bool, len(conflicts))
	for _, group := range conflicts {
		for _, id := range group.IDs {
			conflicted[id] = true
		}
	}
	report := DoctorReport{Conflicts: conflicts}
	for i := range records {
		record := &records[i]
		row := DoctorRow{
			ID:     record.ID,
			Name:   record.Name,
			State:  record.State,
			Digest: record.Digest,
			Origin: record.Origin,
		}
		if conflicted[record.ID] {
			row.State = StateConflict
		}
		var findings []string
		if record.Validation != "" && record.Validation != "[]" {
			if err := json.Unmarshal([]byte(record.Validation), &findings); err != nil {
				findings = nil
			}
		}
		row.Findings = findings
		report.Rows = append(report.Rows, row)
	}
	return report, nil
}

func findConflicts(records []Record) []ConflictGroup {
	byName := make(map[string]map[string][]string)
	for i := range records {
		record := &records[i]
		if record.State != StateActive {
			continue
		}
		origins, ok := byName[record.Name]
		if !ok {
			origins = make(map[string][]string)
			byName[record.Name] = origins
		}
		origins[record.Origin] = append(origins[record.Origin], record.ID)
	}
	groups := make([]ConflictGroup, 0)
	for name, origins := range byName {
		if len(origins) < 2 {
			continue
		}
		ids := make([]string, 0)
		for _, group := range origins {
			ids = append(ids, group...)
		}
		slices.Sort(ids)
		groups = append(groups, ConflictGroup{Name: name, IDs: ids})
	}
	slices.SortFunc(groups, func(a, b ConflictGroup) int { return strings.Compare(a.Name, b.Name) })
	return groups
}

func (e *Engine) sweepRetention(ctx context.Context) {
	if e.retention <= 0 {
		return
	}
	records, err := e.registry.ListByState(ctx, StateRejected, maxSweepPerCycle)
	if err != nil {
		e.logger.WarnContext(ctx, "skill retention sweep failed", "component", "skills")
		return
	}
	cutoff := time.Now().UTC().Add(-e.retention)
	for i := range records {
		record := &records[i]
		reviewed, err := time.Parse(time.RFC3339Nano, record.ReviewedAt)
		if err != nil || !reviewed.Before(cutoff) {
			continue
		}
		origin, err := parseLocalOrigin(record.Origin)
		if err != nil || origin.Kind != OriginPending {
			continue
		}
		if err := removePackageDir(origin.Root); err != nil {
			e.logger.WarnContext(ctx, "skill retention sweep failed",
				"component", "skills",
				"skill_id", record.ID)
			continue
		}
		e.logger.InfoContext(ctx, "skill draft collected",
			"component", "skills",
			"skill_id", record.ID)
	}
}

func removePackageDir(dir string) error {
	if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
		return Errorf(ErrorCodeInvalidArgument, "package directory must be absolute and clean")
	}
	parent := filepath.Dir(dir)
	if filepath.Base(parent) != pendingDirName {
		return Errorf(ErrorCodeInvalidArgument, "package directory is not staged content")
	}
	return os.RemoveAll(dir)
}
