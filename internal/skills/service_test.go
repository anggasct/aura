package skills

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeRegistry struct {
	records map[string]*Record
	err     error
}

func (f *fakeRegistry) UpsertScan(_ context.Context, record *Record) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	if f.records == nil {
		f.records = make(map[string]*Record)
	}
	if _, exists := f.records[record.ID]; exists {
		return false, nil
	}
	f.records[record.ID] = record
	return true, nil
}

func (f *fakeRegistry) Get(_ context.Context, id string) (Record, error) {
	if f.err != nil {
		return Record{}, f.err
	}
	record, exists := f.records[id]
	if !exists {
		return Record{}, Errorf(ErrorCodeSkillNotFound, "skill is not registered")
	}
	return *record, nil
}

func (f *fakeRegistry) ListByState(_ context.Context, state string, limit int) ([]Record, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := make([]Record, 0)
	for _, record := range f.records {
		if record.State == state {
			out = append(out, *record)
		}
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (f *fakeRegistry) AcceptSkill(_ context.Context, id, digest, granted string) error {
	if f.err != nil {
		return f.err
	}
	record, exists := f.records[id]
	if !exists {
		return Errorf(ErrorCodeSkillNotFound, "skill is not registered")
	}
	if record.State != StateQuarantined {
		return Errorf(ErrorCodeSkillInvalid, "skill is not reviewable")
	}
	if record.Digest != digest {
		return Errorf(ErrorCodeSkillDigestChanged, "skill content changed since review")
	}
	record.State = StateActive
	record.Granted = granted
	return nil
}

func (f *fakeRegistry) RejectSkill(_ context.Context, id string) error {
	if f.err != nil {
		return f.err
	}
	record, exists := f.records[id]
	if !exists {
		return Errorf(ErrorCodeSkillNotFound, "skill is not registered")
	}
	if record.State != StateQuarantined {
		return Errorf(ErrorCodeSkillInvalid, "skill is not reviewable")
	}
	record.State = StateRejected
	return nil
}

func TestRegisterScanValid(t *testing.T) {
	dir := makeValidPackage(t)
	registry := &fakeRegistry{}
	summary, findings, err := RegisterScan(t.Context(), registry, dir, "local")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("findings = %v", findings)
	}
	if !summary.Valid || !summary.Inserted {
		t.Errorf("summary = %+v", summary)
	}
	if !strings.HasPrefix(summary.ID, "local/pdf-tools@") {
		t.Errorf("id = %q", summary.ID)
	}
	record := registry.records[summary.ID]
	if record == nil {
		t.Fatalf("record %s not stored", summary.ID)
	}
	if record.State != StateQuarantined || record.Granted != "[]" || record.Validation != "[]" {
		t.Errorf("record = %+v", record)
	}
	var origin map[string]string
	if err := json.Unmarshal([]byte(record.Origin), &origin); err != nil || origin["scope"] != "local" {
		t.Errorf("origin = %q", record.Origin)
	}
	summary, _, err = RegisterScan(t.Context(), registry, dir, "local")
	if err != nil {
		t.Fatalf("rescan: %v", err)
	}
	if summary.Inserted {
		t.Errorf("rescan should dedupe, got %+v", summary)
	}
}

func TestRegisterScanInvalidQuarantines(t *testing.T) {
	dir := t.TempDir()
	writePackageFile(t, dir, "SKILL.md", "---\ndescription: no name\n---\n")
	registry := &fakeRegistry{}
	summary, findings, err := RegisterScan(t.Context(), registry, dir, "local")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if summary.Valid {
		t.Errorf("summary should not be valid: %+v", summary)
	}
	if !containsFinding(findings, FindingNameMissing) {
		t.Errorf("findings = %v", findings)
	}
	if len(registry.records) != 1 {
		t.Fatalf("invalid package should leave one quarantined record, got %d", len(registry.records))
	}
	for _, record := range registry.records {
		if record.State != StateQuarantined {
			t.Errorf("state = %q", record.State)
		}
		var codes []string
		if err := json.Unmarshal([]byte(record.Validation), &codes); err != nil || !containsFinding(codes, FindingNameMissing) {
			t.Errorf("validation = %q", record.Validation)
		}
	}
}

func TestRegisterScanRequestedTools(t *testing.T) {
	dir := t.TempDir()
	writePackageFile(t, dir, "SKILL.md", "---\nname: t\ndescription: d\nallowed-tools: read write\n---\nBody.\n")
	registry := &fakeRegistry{}
	summary, _, err := RegisterScan(t.Context(), registry, dir, "local")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	var requested []string
	if err := json.Unmarshal([]byte(registry.records[summary.ID].Requested), &requested); err != nil {
		t.Fatalf("requested: %v", err)
	}
	if len(requested) != 2 || requested[0] != "read" {
		t.Errorf("requested = %v", requested)
	}
}

func TestRegisterScanErrors(t *testing.T) {
	dir := makeValidPackage(t)
	var nilCtx context.Context
	if _, _, err := RegisterScan(nilCtx, &fakeRegistry{}, dir, "local"); err == nil {
		t.Errorf("nil ctx should fail")
	}
	if _, _, err := RegisterScan(t.Context(), nil, dir, "local"); err == nil {
		t.Errorf("nil registry should fail")
	}
	boom := errors.New("store unavailable")
	if _, _, err := RegisterScan(t.Context(), &fakeRegistry{err: boom}, dir, "local"); !errors.Is(err, boom) {
		t.Errorf("registry error should propagate, got %v", err)
	}
}

func TestRegisterScanHostileQuarantines(t *testing.T) {
	setup := func(t *testing.T, prepare func(dir string)) string {
		t.Helper()
		dir := t.TempDir()
		writePackageFile(t, dir, "SKILL.md", validSkillDocument)
		prepare(dir)
		return dir
	}
	assertQuarantined := func(t *testing.T, dir, want string) {
		t.Helper()
		registry := &fakeRegistry{}
		summary, findings, err := RegisterScan(t.Context(), registry, dir, "local")
		if err != nil {
			t.Fatalf("register: %v", err)
		}
		if summary.Valid {
			t.Errorf("summary should not be valid: %+v", summary)
		}
		if !containsFinding(findings, want) {
			t.Fatalf("findings = %v, want %s", findings, want)
		}
		if len(registry.records) != 1 {
			t.Fatalf("hostile package should leave one quarantined record, got %d", len(registry.records))
		}
		record := registry.records[summary.ID]
		if record == nil {
			t.Fatalf("record %s not stored", summary.ID)
		}
		if record.State != StateQuarantined {
			t.Errorf("state = %q, want %q", record.State, StateQuarantined)
		}
		var codes []string
		if err := json.Unmarshal([]byte(record.Validation), &codes); err != nil || !containsFinding(codes, want) {
			t.Errorf("validation = %q, want %s", record.Validation, want)
		}
	}
	t.Run("symlink escape", func(t *testing.T) {
		dir := setup(t, func(dir string) {
			if err := os.Symlink("/etc/hostname", filepath.Join(dir, "evil")); err != nil {
				t.Skipf("symlink unsupported: %v", err)
			}
		})
		assertQuarantined(t, dir, FindingSymlinkEscape)
	})
	t.Run("too many files", func(t *testing.T) {
		dir := setup(t, func(dir string) {
			for index := range maxPackageFiles {
				writePackageFile(t, dir, fmt.Sprintf("extra-%03d.txt", index), "x")
			}
		})
		assertQuarantined(t, dir, FindingTooManyFiles)
	})
	t.Run("unresolvable link", func(t *testing.T) {
		dir := setup(t, func(dir string) {
			if err := os.Symlink("no-such-target", filepath.Join(dir, "dangling")); err != nil {
				t.Skipf("symlink unsupported: %v", err)
			}
		})
		assertQuarantined(t, dir, FindingSymlinkUnresolvable)
	})
}
