package skills

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func lifecycleEngine(t *testing.T, registry Registry, root string) *Engine {
	t.Helper()
	engine, err := NewEngine(registry, &EngineConfig{
		Dirs:                 []string{root},
		MaxIndexed:           16,
		MaxInstructionRunes:  8192,
		MaxResourceBytes:     65536,
		ScriptToolName:       "exec",
		ScriptToolCapability: "shell.execute",
		QuarantineRetention:  time.Hour,
		PolicyVersion:        "1",
	})
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	return engine
}

func TestStageDraft(t *testing.T) {
	root := t.TempDir()
	registry := &fakeRegistry{}
	engine := lifecycleEngine(t, registry, root)
	summary, err := engine.StageDraft(t.Context(), "fresh", "A fresh skill.", root)
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	if !summary.Valid || !summary.Inserted || summary.Digest == "" {
		t.Errorf("summary = %+v", summary)
	}
	scope := filepath.Base(root)
	if summary.ID != scope+"/fresh@"+summary.Digest[:12] {
		t.Errorf("id = %q", summary.ID)
	}
	record, err := registry.Get(t.Context(), summary.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if record.State != StateQuarantined {
		t.Errorf("state = %q", record.State)
	}
	var origin map[string]string
	if err := json.Unmarshal([]byte(record.Origin), &origin); err != nil || origin["kind"] != OriginPending {
		t.Errorf("origin = %q", record.Origin)
	}
	content, err := os.ReadFile(filepath.Join(root, "pending", "fresh", "SKILL.md"))
	if err != nil {
		t.Fatalf("draft bytes: %v", err)
	}
	if !strings.Contains(string(content), "name: fresh") {
		t.Errorf("draft = %q", content)
	}
	if _, err := engine.StageDraft(t.Context(), "fresh", "Again.", root); !isCode(err, ErrorCodeInvalidArgument) {
		t.Errorf("duplicate err = %v", err)
	}
	for name, tc := range map[string]struct {
		name, description, root string
	}{
		"bad name":     {name: "Bad Name", description: "d", root: root},
		"empty desc":   {name: "ok-name", description: "", root: root},
		"unknown root": {name: "ok-name", description: "d", root: t.TempDir()},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := engine.StageDraft(t.Context(), tc.name, tc.description, tc.root); !isCode(err, ErrorCodeInvalidArgument) {
				t.Errorf("err = %v", err)
			}
		})
	}
	if _, err := engine.StageDraft(nilCtxForTest(), "x", "y", root); !isCode(err, ErrorCodeInvalidArgument) {
		t.Errorf("nil ctx err = %v", err)
	}
}

func TestStageDraftDescriptionRoundTrip(t *testing.T) {
	cases := []string{
		"line1\nfoo: bar",
		"line1\nlicense: MIT",
		"a\n continued",
		"line1\n---\nfoo: bar",
		"line1: value\nsecond: line",
	}
	for i, description := range cases {
		root := t.TempDir()
		registry := &fakeRegistry{}
		engine := lifecycleEngine(t, registry, root)
		name := strings.Repeat("a", 1) + string(rune('a'+i)) + "probe"
		summary, err := engine.StageDraft(t.Context(), name, description, root)
		if err != nil {
			t.Fatalf("case %d stage: %v", i, err)
		}
		if !summary.Valid {
			t.Fatalf("case %d should stage valid, got %+v", i, summary)
		}
		reviewed, err := engine.Review(t.Context(), summary.ID)
		if err != nil {
			t.Fatalf("case %d review: %v", i, err)
		}
		if reviewed.Description != description {
			t.Errorf("case %d stored = %q, want %q", i, reviewed.Description, description)
		}
		content, err := os.ReadFile(filepath.Join(root, "pending", name, "SKILL.md"))
		if err != nil {
			t.Fatalf("case %d bytes: %v", i, err)
		}
		manifest, findings := ParseSkillFile(content)
		if len(findings) != 0 {
			t.Errorf("case %d findings = %v", i, findings)
		}
		if manifest == nil {
			t.Fatalf("case %d manifest is nil", i)
		}
		if manifest.Description != description {
			t.Errorf("case %d manifest = %q, want %q", i, manifest.Description, description)
		}
		if len(manifest.UnknownFields) != 0 {
			t.Errorf("case %d unknown fields = %v", i, manifest.UnknownFields)
		}
		if manifest.License == "MIT" {
			t.Errorf("case %d description leaked into license", i)
		}
	}
}

func TestConcurrentDuplicateStage(t *testing.T) {
	root := t.TempDir()
	registry := &fakeRegistry{}
	engine := lifecycleEngine(t, registry, root)
	const workers = 8
	results := make(chan error, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := engine.StageDraft(context.Background(), "racy", "Racy draft.", root)
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	succeeded := 0
	for err := range results {
		if err == nil {
			succeeded++
		} else if !isCode(err, ErrorCodeInvalidArgument) {
			t.Errorf("duplicate err = %v, want invalid_argument", err)
		}
	}
	if succeeded != 1 {
		t.Errorf("concurrent duplicate stage successes = %d, want exactly 1", succeeded)
	}
}

func TestCollisionExcludesBoth(t *testing.T) {
	first := t.TempDir()
	second := t.TempDir()
	writePackageFile(t, filepath.Join(first, "same"), "SKILL.md", "---\nname: same\ndescription: First.\n---\nOne.\n")
	writePackageFile(t, filepath.Join(second, "same"), "SKILL.md", "---\nname: same\ndescription: Second.\n---\nTwo.\n")
	registry := &fakeRegistry{}
	engine, err := NewEngine(registry, &EngineConfig{
		Dirs:                 []string{first, second},
		MaxIndexed:           16,
		MaxInstructionRunes:  8192,
		MaxResourceBytes:     65536,
		ScriptToolName:       "exec",
		ScriptToolCapability: "shell.execute",
		PolicyVersion:        "1",
	})
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	if err := engine.Refresh(t.Context()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	for _, record := range registry.records {
		record.State = StateActive
	}
	if err := engine.Refresh(t.Context()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if catalog := engine.Catalog(); len(catalog) != 0 {
		t.Errorf("colliding packages should not index, got %v", catalog)
	}
	if _, err := engine.Activate(t.Context(), "same", "r"); !isCode(err, ErrorCodeSkillUnavailable) {
		t.Errorf("ambiguous err = %v", err)
	}
	report, err := engine.Doctor(t.Context())
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	if len(report.Conflicts) != 1 || report.Conflicts[0].Name != "same" || len(report.Conflicts[0].IDs) != 2 {
		t.Errorf("conflicts = %+v", report.Conflicts)
	}
	states := make(map[string]int)
	for _, row := range report.Rows {
		states[row.State]++
	}
	if states[StateConflict] != 2 {
		t.Errorf("rows = %+v", report.Rows)
	}
}

func TestDisableTransitions(t *testing.T) {
	root := makeExecRoot(t)
	registry := &fakeRegistry{}
	engine := execEngine(t, registry, root)
	activation := activateOps(t, engine, registry, nil)
	if err := engine.Disable(t.Context(), activation.SkillID); err != nil {
		t.Fatalf("disable: %v", err)
	}
	record, err := registry.Get(t.Context(), activation.SkillID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if record.State != StateDisabled {
		t.Errorf("state = %q", record.State)
	}
	if _, err := engine.Activate(t.Context(), "ops", "r"); !isCode(err, ErrorCodeSkillUnavailable) {
		t.Errorf("disabled err = %v", err)
	}
	if err := engine.Disable(t.Context(), activation.SkillID); !isCode(err, ErrorCodeSkillInvalid) {
		t.Errorf("second disable err = %v", err)
	}
	if err := engine.Disable(t.Context(), "local/missing@0123456789ab"); !isCode(err, ErrorCodeSkillNotFound) {
		t.Errorf("missing err = %v", err)
	}
}

func TestValidateDir(t *testing.T) {
	registry := &fakeRegistry{}
	engine := lifecycleEngine(t, registry, t.TempDir())
	dir := makeValidPackage(t)
	digest, findings, err := engine.ValidateDir(t.Context(), dir)
	if err != nil || len(findings) != 0 || digest == "" {
		t.Errorf("digest = %q findings = %v, %v", digest, findings, err)
	}
	bad := t.TempDir()
	writePackageFile(t, bad, "SKILL.md", "---\ndescription: no name\n---\n")
	_, findings, err = engine.ValidateDir(t.Context(), bad)
	if err != nil || !containsFinding(findings, FindingNameMissing) {
		t.Errorf("findings = %v, %v", findings, err)
	}
	if len(registry.records) != 0 {
		t.Errorf("validate should not register, got %d rows", len(registry.records))
	}
	if _, _, err := engine.ValidateDir(nilCtxForTest(), dir); !isCode(err, ErrorCodeInvalidArgument) {
		t.Errorf("nil ctx err = %v", err)
	}
}

func TestRetentionSweepCollectsExpiredDrafts(t *testing.T) {
	root := t.TempDir()
	registry := &fakeRegistry{}
	engine := lifecycleEngine(t, registry, root)
	summary, err := engine.StageDraft(t.Context(), "stale", "Stale draft.", root)
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	if _, err := engine.Reject(t.Context(), summary.ID, "nope"); err != nil {
		t.Fatalf("reject: %v", err)
	}
	pkgdir := filepath.Join(root, "pending", "stale")
	if err := engine.Refresh(t.Context()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if _, err := os.Stat(pkgdir); err != nil {
		t.Errorf("fresh rejection should retain bytes: %v", err)
	}
	past := time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339Nano)
	for _, record := range registry.records {
		if record.ID == summary.ID {
			record.ReviewedAt = past
		}
	}
	if err := engine.Refresh(t.Context()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if _, err := os.Stat(pkgdir); !os.IsNotExist(err) {
		t.Errorf("expired rejection should collect bytes, stat err = %v", err)
	}
}

func TestConcurrentRefreshAndActivate(t *testing.T) {
	root := makeEngineRoot(t, map[string]string{"alpha": "body one", "beta": "body two"})
	registry := &fakeRegistry{}
	engine := lifecycleEngine(t, registry, root)
	if err := engine.Refresh(t.Context()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	for _, record := range registry.records {
		record.State = StateActive
	}
	if err := engine.Refresh(t.Context()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	done := make(chan error, 16)
	for range 8 {
		go func() {
			_, err := engine.Activate(context.Background(), "alpha", "race")
			done <- err
		}()
		go func() {
			done <- engine.Refresh(context.Background())
		}()
	}
	for range 16 {
		if err := <-done; err != nil {
			t.Fatalf("concurrent use: %v", err)
		}
	}
}

func TestIndexTruncationDeterministic(t *testing.T) {
	packages := make(map[string]string, 4)
	for _, name := range []string{"aaa", "bbb", "ccc", "ddd"} {
		packages[name] = "body"
	}
	root := makeEngineRoot(t, packages)
	registry := &fakeRegistry{}
	engine, err := NewEngine(registry, &EngineConfig{
		Dirs:                 []string{root},
		MaxIndexed:           2,
		MaxInstructionRunes:  8192,
		MaxResourceBytes:     65536,
		ScriptToolName:       "exec",
		ScriptToolCapability: "shell.execute",
		PolicyVersion:        "1",
	})
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	if err := engine.Refresh(t.Context()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	for _, record := range registry.records {
		record.State = StateActive
	}
	first := catalogIDs(t, engine)
	second := catalogIDs(t, engine)
	if len(first) != 2 || len(second) != 2 {
		t.Fatalf("catalog = %v, %v", first, second)
	}
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("nondeterministic truncation: %v vs %v", first, second)
		}
	}
}

func catalogIDs(t *testing.T, engine *Engine) []string {
	t.Helper()
	if err := engine.Refresh(t.Context()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	ids := make([]string, 0)
	for _, entry := range engine.Catalog() {
		ids = append(ids, entry.ID)
	}
	return ids
}

func TestDoctorLeaksNoContent(t *testing.T) {
	root := makeExecRoot(t)
	registry := &fakeRegistry{}
	engine := lifecycleEngine(t, registry, root)
	if err := engine.Refresh(t.Context()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	_ = activateOps(t, engine, registry, []string{"shell.execute"})
	report, err := engine.Doctor(t.Context())
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(encoded), "echo hi") || strings.Contains(string(encoded), "Body.") {
		t.Errorf("doctor leaks content: %s", encoded)
	}
	if len(report.Rows) == 0 {
		t.Errorf("doctor should list rows")
	}
}
