package broadcast

import (
	"context"
	"strings"
	"testing"
	"time"
)

func digestItem(id, dest, priority string, createdAt time.Time, size int) Item {
	return Item{
		ID:               id,
		Producer:         "cron",
		IdempotencyKey:   "key-" + id,
		Priority:         priority,
		DestinationAlias: dest,
		ContentJSON:      `{"text":"` + strings.Repeat("x", size) + `"}`,
		State:            StateHeld,
		NotBefore:        time.Date(2026, 9, 13, 7, 0, 0, 0, time.UTC),
		CreatedAt:        createdAt,
	}
}

func TestGroupDigests_GroupsByWindowDestinationPriority(t *testing.T) {
	base := time.Date(2026, 9, 12, 23, 30, 0, 0, time.UTC)
	items := []Item{
		digestItem("c2", "default", PriorityInfo, base.Add(time.Minute), 10),
		digestItem("c1", "default", PriorityInfo, base, 10),
		digestItem("w1", "default", PriorityWarning, base, 10),
		digestItem("d1", "plain", PriorityInfo, base, 10),
		digestItem("u1", "default", PriorityUrgent, base, 10),
	}
	groups := GroupDigests(items, 20, 12000)
	if len(groups) != 3 {
		t.Fatalf("groups = %d, want 3 (urgent excluded)", len(groups))
	}
	var info []Item
	for _, group := range groups {
		if len(group) > 0 && group[0].Priority == PriorityInfo && group[0].DestinationAlias == "default" {
			info = group
		}
		for _, item := range group {
			if item.Priority == PriorityUrgent {
				t.Errorf("urgent item grouped: %+v", item)
			}
		}
	}
	if len(info) != 2 || info[0].ID != "c1" || info[1].ID != "c2" {
		t.Errorf("info group order wrong: %+v", info)
	}
	again := GroupDigests(items, 20, 12000)
	if len(again) != len(groups) {
		t.Fatal("grouping is not deterministic")
	}
	for i := range groups {
		if len(again[i]) != len(groups[i]) {
			t.Fatal("grouping is not deterministic")
		}
		for j := range groups[i] {
			if again[i][j].ID != groups[i][j].ID {
				t.Fatal("grouping is not deterministic")
			}
		}
	}
}

func TestGroupDigests_SplitsOnCountAndBytes(t *testing.T) {
	base := time.Date(2026, 9, 12, 23, 30, 0, 0, time.UTC)
	items := []Item{
		digestItem("c1", "default", PriorityInfo, base, 10),
		digestItem("c2", "default", PriorityInfo, base.Add(time.Minute), 10),
		digestItem("c3", "default", PriorityInfo, base.Add(2*time.Minute), 10),
	}
	groups := GroupDigests(items, 2, 12000)
	if len(groups) != 2 || len(groups[0]) != 2 || len(groups[1]) != 1 {
		t.Fatalf("count split = %v, want [2][1]", groups)
	}
	if groups[1][0].ID != "c3" {
		t.Errorf("split order wrong: %+v", groups[1])
	}
	tiny := GroupDigests(items, 20, int64(len(items[0].ContentJSON)+digestContentOverhead+1))
	if len(tiny) != 3 {
		t.Fatalf("byte split groups = %d, want 3", len(tiny))
	}
}

func TestCreateDigest_LinksChildren(t *testing.T) {
	broadcaster := testBroadcaster()
	store := newFakeItemStore()
	broadcaster.items = store
	base := time.Date(2026, 9, 12, 23, 30, 0, 0, time.UTC)
	group := []Item{
		digestItem("c1", "default", PriorityInfo, base, 10),
		digestItem("c2", "default", PriorityInfo, base.Add(time.Minute), 10),
	}
	for _, item := range group {
		record := ItemRecord{
			ID: item.ID, Producer: item.Producer, IdempotencyKey: item.IdempotencyKey,
			ContentDigest: digestContent(item.ContentJSON), Priority: item.Priority,
			DestinationAlias: item.DestinationAlias, ContentJSON: item.ContentJSON,
			State: StateHeld, NotBefore: item.NotBefore, CreatedAt: base, UpdatedAt: base,
		}
		if _, _, err := store.Insert(t.Context(), &record); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	parent, replayed, err := broadcaster.CreateDigest(t.Context(), group, 0)
	if err != nil {
		t.Fatalf("CreateDigest(): %v", err)
	}
	if replayed {
		t.Error("first digest reported replay")
	}
	if parent.State != StateScheduled || !strings.HasPrefix(parent.ID, "bcst_dg_") {
		t.Errorf("parent = %+v", parent)
	}
	if int64(len(parent.ContentJSON)) > 12000 {
		t.Error("parent content exceeds the byte bound")
	}
	for _, id := range []string{"c1", "c2"} {
		child := store.records[id]
		if child.State != StateCancelled || child.DigestParentID != parent.ID {
			t.Errorf("child %s not linked: %+v", id, child)
		}
	}
	same, replayed, err := broadcaster.CreateDigest(t.Context(), group, 0)
	if err != nil || !replayed || same.ID != parent.ID {
		t.Errorf("digest replay = %+v, %v, %v; want same parent replayed", same.ID, replayed, err)
	}
}

func TestCreateDigest_RejectsMixedOrUrgent(t *testing.T) {
	broadcaster := testBroadcaster()
	base := time.Date(2026, 9, 12, 23, 30, 0, 0, time.UTC)
	mixed := []Item{
		digestItem("c1", "default", PriorityInfo, base, 10),
		digestItem("d1", "plain", PriorityInfo, base, 10),
	}
	if _, _, err := broadcaster.CreateDigest(t.Context(), mixed, 0); err == nil {
		t.Error("mixed-destination group accepted")
	}
	urgent := []Item{digestItem("u1", "default", PriorityUrgent, base, 10)}
	if _, _, err := broadcaster.CreateDigest(t.Context(), urgent, 0); err == nil {
		t.Error("urgent group accepted")
	}
	if _, _, err := broadcaster.CreateDigest(t.Context(), nil, 0); err == nil {
		t.Error("empty group accepted")
	}
	var nilCtx context.Context
	if _, _, err := broadcaster.CreateDigest(nilCtx, urgent, 0); err == nil {
		t.Error("nil context accepted")
	}
}
