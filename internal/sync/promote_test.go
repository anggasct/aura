package sync

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func promoteSnapshot() (snapshot FetchSnapshot, contents map[string][]byte) {
	body := "version: 1\n"
	entries := []FetchEntry{{Path: "config-templates/aura.yaml", Size: int64(len(body)), Digest: digestContent([]byte(body))}}
	return FetchSnapshot{
		Ref:     "ref-abc",
		Branch:  "main",
		Entries: entries,
		Digest:  digestSnapshot([]ExportEntry{{Path: entries[0].Path, Size: entries[0].Size, Digest: entries[0].Digest}}),
	}, map[string][]byte{"config-templates/aura.yaml": []byte(body)}
}

func TestPlanPromotionSkipsSkillReview(t *testing.T) {
	t.Parallel()
	body := "---\nname: deploy\n---\n# Deploy\n"
	entries := []FetchEntry{
		{Path: "skills/reviewed/deploy/SKILL.md", Size: int64(len(body)), Digest: digestContent([]byte(body))},
		{Path: "config-templates/aura.yaml", Size: 11, Digest: digestContent([]byte("version: 1\n"))},
	}
	snapshot := FetchSnapshot{Ref: "ref-abc", Branch: "main", Entries: entries, Digest: "digest-plan"}
	plan, rollback, err := PlanPromotion(snapshot, AdvanceFastForward, func(path string) bool {
		return path == "skills/reviewed/deploy/SKILL.md"
	})
	if err != nil {
		t.Fatalf("PlanPromotion: %v", err)
	}
	if len(plan.Entries) != 1 || plan.Entries[0].Path != "config-templates/aura.yaml" {
		t.Fatalf("plan = %+v, want only the template", plan)
	}
	if len(rollback.Entries) != 0 {
		t.Fatalf("rollback = %+v, want no pre-seeded entries", rollback)
	}
	if _, _, err := PlanPromotion(snapshot, AdvanceConflict, nil); err == nil {
		t.Fatal("conflict decision must fail")
	} else if code, ok := CodeOf(err); !ok || code != ErrorCodeConflict {
		t.Fatalf("code = %v,%v want sync_conflict", code, ok)
	}
}

func TestPlanPromotionNilReviewHoldsSkills(t *testing.T) {
	t.Parallel()
	skillBody := "---\nname: deploy\n---\n# Deploy\n"
	tpl := "version: 1\n"
	entries := []FetchEntry{
		{Path: "skills/reviewed/deploy/SKILL.md", Size: int64(len(skillBody)), Digest: digestContent([]byte(skillBody))},
		{Path: "config-templates/aura.yaml", Size: int64(len(tpl)), Digest: digestContent([]byte(tpl))},
	}
	snapshot := FetchSnapshot{Ref: "ref-abc", Branch: "main", Entries: entries, Digest: "digest-plan"}
	plan, _, err := PlanPromotion(snapshot, AdvanceFastForward, nil)
	if err != nil {
		t.Fatalf("PlanPromotion: %v", err)
	}
	for _, entry := range plan.Entries {
		if entry.Path == "skills/reviewed/deploy/SKILL.md" {
			t.Fatalf("nil review must hold skill paths: plan=%+v", plan)
		}
	}
	if len(plan.Entries) != 1 || plan.Entries[0].Path != "config-templates/aura.yaml" {
		t.Fatalf("plan = %+v, want only the template", plan)
	}
}

func TestApplyPromotionAtomic(t *testing.T) {
	t.Parallel()
	snapshot, contents := promoteSnapshot()
	plan, rollback, err := PlanPromotion(snapshot, AdvanceFastForward, nil)
	if err != nil {
		t.Fatalf("PlanPromotion: %v", err)
	}
	target := t.TempDir()
	if err := ApplyPromotion(t.Context(), target, plan, contents, &rollback); err != nil {
		t.Fatalf("ApplyPromotion: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(target, "config-templates", "aura.yaml"))
	if err != nil {
		t.Fatalf("read promoted: %v", err)
	}
	if string(raw) != "version: 1\n" {
		t.Fatalf("promoted = %q, want template content", raw)
	}
	if len(rollback.Entries) != len(plan.Entries) {
		t.Fatalf("rollback len = %d, want %d", len(rollback.Entries), len(plan.Entries))
	}
}

func TestApplyPromotionFailureLeavesTargetUnchanged(t *testing.T) {
	t.Parallel()
	snapshot, _ := promoteSnapshot()
	plan, rollback, err := PlanPromotion(snapshot, AdvanceFastForward, nil)
	if err != nil {
		t.Fatalf("PlanPromotion: %v", err)
	}
	target := t.TempDir()
	if err := ApplyPromotion(t.Context(), target, plan, map[string][]byte{}, &rollback); err == nil {
		t.Fatal("missing content must fail")
	}
	entries, err := os.ReadDir(filepath.Join(target, "config-templates"))
	if err == nil && len(entries) != 0 {
		t.Fatalf("target holds partial files: %v", entries)
	}
}

func TestApplyPromotionMidFailureAtomic(t *testing.T) {
	t.Parallel()
	first := []byte("first: 1\n")
	second := []byte("second: 2\n")
	entries := []FetchEntry{
		{Path: "config-templates/a.yaml", Size: int64(len(first)), Digest: digestContent(first)},
		{Path: "config-templates/b", Size: int64(len(second)), Digest: digestContent(second)},
	}
	snapshot := FetchSnapshot{
		Ref:     "ref-abc",
		Branch:  "main",
		Entries: entries,
		Digest:  digestSnapshot([]ExportEntry{{Path: entries[0].Path, Size: entries[0].Size, Digest: entries[0].Digest}, {Path: entries[1].Path, Size: entries[1].Size, Digest: entries[1].Digest}}),
	}
	plan, rollback, err := PlanPromotion(snapshot, AdvanceFastForward, nil)
	if err != nil {
		t.Fatalf("PlanPromotion: %v", err)
	}
	target := t.TempDir()
	if err := os.MkdirAll(filepath.Join(target, "config-templates", "b"), 0o755); err != nil {
		t.Fatalf("pre-create blocking dir: %v", err)
	}
	contents := map[string][]byte{"config-templates/a.yaml": first, "config-templates/b": second}
	if err := ApplyPromotion(t.Context(), target, plan, contents, &rollback); err == nil {
		t.Fatal("blocked promotion must fail")
	}
	if _, err := os.Stat(filepath.Join(target, "config-templates", "a.yaml")); !os.IsNotExist(err) {
		t.Fatalf("first file must be rolled back, stat err = %v", err)
	}
}

func TestApplyPromotionMidFailureRestoresPrior(t *testing.T) {
	t.Parallel()
	old := []byte("old: 1\n")
	next := []byte("new: 1\n")
	second := []byte("second: 2\n")
	entries := []FetchEntry{
		{Path: "config-templates/a.yaml", Size: int64(len(next)), Digest: digestContent(next)},
		{Path: "config-templates/b", Size: int64(len(second)), Digest: digestContent(second)},
	}
	snapshot := FetchSnapshot{
		Ref:     "ref-abc",
		Branch:  "main",
		Entries: entries,
		Digest:  digestSnapshot([]ExportEntry{{Path: entries[0].Path, Size: entries[0].Size, Digest: entries[0].Digest}, {Path: entries[1].Path, Size: entries[1].Size, Digest: entries[1].Digest}}),
	}
	plan, rollback, err := PlanPromotion(snapshot, AdvanceFastForward, nil)
	if err != nil {
		t.Fatalf("PlanPromotion: %v", err)
	}
	target := t.TempDir()
	if err := os.MkdirAll(filepath.Join(target, "config-templates"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "config-templates", "a.yaml"), old, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(target, "config-templates", "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	contents := map[string][]byte{"config-templates/a.yaml": next, "config-templates/b": second}
	if err := ApplyPromotion(t.Context(), target, plan, contents, &rollback); err == nil {
		t.Fatal("blocked promotion must fail")
	}
	raw, err := os.ReadFile(filepath.Join(target, "config-templates", "a.yaml"))
	if err != nil {
		t.Fatalf("read restored: %v", err)
	}
	if !bytes.Equal(raw, old) {
		t.Fatalf("restored = %q, want %q", raw, old)
	}
}

func TestApplyPromotionStaysInsideTarget(t *testing.T) {
	snapshot, contents := promoteSnapshot()
	plan, rollback, err := PlanPromotion(snapshot, AdvanceFastForward, nil)
	if err != nil {
		t.Fatalf("PlanPromotion: %v", err)
	}
	target := t.TempDir()
	isolated := t.TempDir()
	t.Setenv("TMPDIR", isolated)
	if err := ApplyPromotion(t.Context(), target, plan, contents, &rollback); err != nil {
		t.Fatalf("ApplyPromotion: %v", err)
	}
	leftovers, err := os.ReadDir(isolated)
	if err != nil {
		t.Fatalf("read isolated TMPDIR: %v", err)
	}
	if len(leftovers) != 0 {
		t.Fatalf("promotion leaked %d files into TMPDIR", len(leftovers))
	}
	info, err := os.Stat(filepath.Join(target, "config-templates", "aura.yaml"))
	if err != nil {
		t.Fatalf("stat promoted: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("promoted mode = %o, want 600", info.Mode().Perm())
	}
	dirInfo, err := os.Stat(filepath.Join(target, "config-templates"))
	if err != nil {
		t.Fatalf("stat dest dir: %v", err)
	}
	if dirInfo.Mode().Perm()&0o077 != 0 {
		t.Fatalf("dest dir mode = %o, want owner-only", dirInfo.Mode().Perm())
	}
}

func TestRollbackManifestRestores(t *testing.T) {
	t.Parallel()
	old := []byte("old: 1\n")
	next := []byte("new: 1\n")
	entries := []FetchEntry{{Path: "config-templates/aura.yaml", Size: int64(len(next)), Digest: digestContent(next)}}
	snapshot := FetchSnapshot{
		Ref:     "ref-abc",
		Branch:  "main",
		Entries: entries,
		Digest:  digestSnapshot([]ExportEntry{{Path: entries[0].Path, Size: entries[0].Size, Digest: entries[0].Digest}}),
	}
	plan, rollback, err := PlanPromotion(snapshot, AdvanceFastForward, nil)
	if err != nil {
		t.Fatalf("PlanPromotion: %v", err)
	}
	target := t.TempDir()
	if err := os.MkdirAll(filepath.Join(target, "config-templates"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "config-templates", "aura.yaml"), old, 0o600); err != nil {
		t.Fatal(err)
	}
	contents := map[string][]byte{"config-templates/aura.yaml": next}
	if err := ApplyPromotion(t.Context(), target, plan, contents, &rollback); err != nil {
		t.Fatalf("ApplyPromotion: %v", err)
	}
	if len(rollback.Entries) != len(plan.Entries) {
		t.Fatalf("rollback len = %d, want %d", len(rollback.Entries), len(plan.Entries))
	}
	recorded := rollback.Entries[0]
	if !recorded.Existed || recorded.Size != int64(len(old)) || recorded.Digest != digestContent(old) {
		t.Fatalf("rollback entry = %+v, want prior digest of old content", recorded)
	}
	if rollback.Digest == plan.Digest {
		t.Fatalf("rollback digest must differ from plan digest")
	}
	wantPrior := digestSnapshot([]ExportEntry{{Path: recorded.Path, Size: recorded.Size, Digest: recorded.Digest}})
	if rollback.Digest != wantPrior {
		t.Fatalf("rollback digest = %q, want prior digest %q", rollback.Digest, wantPrior)
	}
	raw, err := os.ReadFile(filepath.Join(target, "config-templates", "aura.yaml"))
	if err != nil || !bytes.Equal(raw, next) {
		t.Fatalf("promoted = %q, want new content", raw)
	}
	if got := digestContent(old); got != recorded.Digest {
		t.Fatalf("prior bytes do not verify against manifest")
	}
}

func TestApplyPromotionRejectsDenied(t *testing.T) {
	t.Parallel()
	content := []byte("data")
	snapshot := FetchSnapshot{
		Ref:     "ref-abc",
		Branch:  "main",
		Entries: []FetchEntry{{Path: "skills/state.db", Size: int64(len(content)), Digest: digestContent(content)}},
		Digest:  "digest-plan",
	}
	plan := PromotePlan{Entries: snapshot.Entries, Digest: snapshot.Digest}
	rollback := RollbackManifest{}
	if err := ApplyPromotion(t.Context(), t.TempDir(), plan, map[string][]byte{"skills/state.db": content}, &rollback); err == nil {
		t.Fatal("denied path must fail")
	} else if code, ok := CodeOf(err); !ok || code != ErrorCodePathDenied {
		t.Fatalf("code = %v,%v want sync_path_denied", code, ok)
	}
}
