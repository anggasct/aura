package sync

import (
	"os"
	"path/filepath"
	"testing"
)

func promoteSnapshot() (snapshot FetchSnapshot, contents map[string][]byte) {
	body := "version: 1\n"
	entries := []FetchEntry{{Path: "config-templates/aura.yaml", Size: int64(len(body)), Digest: "digest-1"}}
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
		{Path: "skills/reviewed/deploy/SKILL.md", Size: int64(len(body)), Digest: "digest-skill"},
		{Path: "config-templates/aura.yaml", Size: 11, Digest: "digest-tpl"},
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
	if len(rollback.Entries) != 1 {
		t.Fatalf("rollback = %+v, want one recorded entry", rollback)
	}
	if _, _, err := PlanPromotion(snapshot, AdvanceConflict, nil); err == nil {
		t.Fatal("conflict decision must fail")
	} else if code, ok := CodeOf(err); !ok || code != ErrorCodeConflict {
		t.Fatalf("code = %v,%v want sync_conflict", code, ok)
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
	if len(rollback.Entries) == 0 {
		t.Fatal("rollback must record the promoted entry")
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

func TestApplyPromotionRejectsDenied(t *testing.T) {
	t.Parallel()
	snapshot := FetchSnapshot{
		Ref:     "ref-abc",
		Branch:  "main",
		Entries: []FetchEntry{{Path: "skills/state.db", Size: 4, Digest: "digest-1"}},
		Digest:  "digest-plan",
	}
	plan := PromotePlan{Entries: snapshot.Entries, Digest: snapshot.Digest}
	rollback := RollbackManifest{}
	if err := ApplyPromotion(t.Context(), t.TempDir(), plan, map[string][]byte{"skills/state.db": []byte("data")}, &rollback); err == nil {
		t.Fatal("denied path must fail")
	} else if code, ok := CodeOf(err); !ok || code != ErrorCodePathDenied {
		t.Fatalf("code = %v,%v want sync_path_denied", code, ok)
	}
}
