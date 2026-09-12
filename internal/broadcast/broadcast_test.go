package broadcast

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeItemStore struct {
	mu      sync.Mutex
	records map[string]ItemRecord
	byKey   map[string]string
}

func newFakeItemStore() *fakeItemStore {
	return &fakeItemStore{records: map[string]ItemRecord{}, byKey: map[string]string{}}
}

func (s *fakeItemStore) Insert(_ context.Context, record *ItemRecord) (ItemRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := record.Producer + "|" + record.IdempotencyKey
	if id, ok := s.byKey[key]; ok {
		existing := s.records[id]
		if existing.ContentDigest != record.ContentDigest {
			return ItemRecord{}, false, Errorf(ErrorCodeConflict, "idempotency key is already claimed with different content")
		}
		return existing, true, nil
	}
	s.records[record.ID] = *record
	s.byKey[key] = record.ID
	return *record, false, nil
}

type fakeRegistry struct {
	registered map[string]bool
}

func (r *fakeRegistry) Registered(source string) bool {
	return r.registered[source]
}

func testBroadcaster() *Broadcaster {
	broadcaster, err := New(
		map[string]string{"default": "discord:owner", "plain": "discord"},
		&fakeRegistry{registered: map[string]bool{"discord": true}},
		newFakeItemStore(),
	)
	if err != nil {
		panic(err)
	}
	return broadcaster
}

func testNotification() *Notification {
	return &Notification{
		Producer:         "cron",
		IdempotencyKey:   "key-1",
		Priority:         PriorityWarning,
		DestinationAlias: "default",
		ContentJSON:      `{"text":"backup finished"}`,
		CreatedAt:        time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC),
	}
}

func TestSubmit_PersistsScheduledItem(t *testing.T) {
	broadcaster := testBroadcaster()
	item, replayed, err := broadcaster.Submit(t.Context(), testNotification())
	if err != nil {
		t.Fatalf("Submit(): %v", err)
	}
	if replayed {
		t.Error("first submit reported replay")
	}
	if item.ID == "" || !strings.HasPrefix(item.ID, "bcst_") {
		t.Errorf("item ID = %q, want bcst_ identity", item.ID)
	}
	if item.State != StateScheduled {
		t.Errorf("item state = %q, want scheduled", item.State)
	}
	if item.ContentDigest == "" {
		t.Error("item carries no content digest")
	}
	if !item.NotBefore.Equal(testNotification().CreatedAt) {
		t.Errorf("not_before = %v, want creation time", item.NotBefore)
	}
	if item.Priority != PriorityWarning || item.DestinationAlias != "default" {
		t.Errorf("item mismatch: %+v", item)
	}
}

func TestSubmit_IdenticalReplayReturnsID(t *testing.T) {
	broadcaster := testBroadcaster()
	first, _, err := broadcaster.Submit(t.Context(), testNotification())
	if err != nil {
		t.Fatalf("first submit: %v", err)
	}
	second, replayed, err := broadcaster.Submit(t.Context(), testNotification())
	if err != nil {
		t.Fatalf("replay submit: %v", err)
	}
	if !replayed {
		t.Error("identical replay did not report replay")
	}
	if second.ID != first.ID {
		t.Errorf("replay ID = %q, want %q", second.ID, first.ID)
	}
}

func TestSubmit_MismatchedReuseConflicts(t *testing.T) {
	broadcaster := testBroadcaster()
	if _, _, err := broadcaster.Submit(t.Context(), testNotification()); err != nil {
		t.Fatalf("first submit: %v", err)
	}
	changed := testNotification()
	changed.ContentJSON = `{"text":"different"}`
	if _, _, err := broadcaster.Submit(t.Context(), changed); err == nil {
		t.Fatal("mismatched reuse accepted")
	} else if code, ok := CodeOf(err); !ok || code != ErrorCodeConflict {
		t.Errorf("mismatch code = %v, %v; want conflict", code, ok)
	}
}

func TestResolve_AcceptsConfiguredRoutes(t *testing.T) {
	broadcaster := testBroadcaster()
	route, err := broadcaster.Resolve("default")
	if err != nil {
		t.Fatalf("Resolve(): %v", err)
	}
	if route.Source != "discord" || route.Instance != "owner" {
		t.Errorf("route = %+v, want discord:owner", route)
	}
	route, err = broadcaster.Resolve("plain")
	if err != nil {
		t.Fatalf("Resolve(): %v", err)
	}
	if route.Source != "discord" || route.Instance != "" {
		t.Errorf("route = %+v, want bare discord source", route)
	}
}

func TestSubmit_RejectsUnconfiguredOrUnknown(t *testing.T) {
	broadcaster := testBroadcaster()
	notification := testNotification()
	notification.DestinationAlias = "missing"
	if _, _, err := broadcaster.Submit(t.Context(), notification); err == nil {
		t.Error("unknown alias accepted")
	} else if code, ok := CodeOf(err); !ok || code != ErrorCodeUnknownAlias {
		t.Errorf("unknown alias code = %v, %v", code, ok)
	}

	foreign, err := New(
		map[string]string{"default": "telegram:owner"},
		&fakeRegistry{registered: map[string]bool{"discord": true}},
		newFakeItemStore(),
	)
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	if _, _, err := foreign.Submit(t.Context(), testNotification()); err == nil {
		t.Error("unregistered channel accepted")
	} else if code, ok := CodeOf(err); !ok || code != ErrorCodeChannelUnknown {
		t.Errorf("unregistered channel code = %v, %v", code, ok)
	}
}

func TestSubmit_RejectsInjection(t *testing.T) {
	broadcaster := testBroadcaster()
	for _, alias := range []string{
		"../escape",
		"https://evil.example/hook",
		"discord:owner:extra",
		"DISCORD",
		"has space",
		"has/slash",
		"",
		strings.Repeat("a", maxAliasRunes+1),
	} {
		notification := testNotification()
		notification.DestinationAlias = alias
		if _, _, err := broadcaster.Submit(t.Context(), notification); err == nil {
			t.Errorf("injected alias %q accepted", alias)
		}
	}
}

func TestSubmit_RejectsInvalidNotifications(t *testing.T) {
	broadcaster := testBroadcaster()
	valid := testNotification
	cases := []struct {
		name   string
		mutate func(*Notification)
	}{
		{"empty producer", func(n *Notification) { n.Producer = "" }},
		{"empty key", func(n *Notification) { n.IdempotencyKey = "" }},
		{"bad priority", func(n *Notification) { n.Priority = "critical" }},
		{"empty content", func(n *Notification) { n.ContentJSON = "" }},
		{"oversize content", func(n *Notification) { n.ContentJSON = strings.Repeat("x", maxContentBytes+1) }},
		{"non-json content", func(n *Notification) { n.ContentJSON = "plain text" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			notification := valid()
			tc.mutate(notification)
			if _, _, err := broadcaster.Submit(t.Context(), notification); err == nil {
				t.Errorf("invalid notification accepted: %+v", notification)
			}
		})
	}
	if _, _, err := broadcaster.Submit(t.Context(), nil); err == nil {
		t.Error("nil notification accepted")
	}
	var nilCtx context.Context
	if _, _, err := broadcaster.Submit(nilCtx, valid()); err == nil {
		t.Error("nil context accepted")
	}
}

func TestNew_RejectsBadWiring(t *testing.T) {
	if _, err := New(map[string]string{"default": "discord:owner"}, nil, newFakeItemStore()); err == nil {
		t.Error("nil registry accepted")
	}
	if _, err := New(map[string]string{"default": "discord:owner"}, &fakeRegistry{}, nil); err == nil {
		t.Error("nil store accepted")
	}
	if _, err := New(map[string]string{"BAD ALIAS": "discord"}, &fakeRegistry{}, newFakeItemStore()); err == nil {
		t.Error("bad alias accepted")
	}
	if _, err := New(map[string]string{"default": "https://evil.example/hook"}, &fakeRegistry{}, newFakeItemStore()); err == nil {
		t.Error("credential-bearing route accepted")
	}
}

func TestParseRoute(t *testing.T) {
	source, instance, err := ParseRoute("discord:owner")
	if err != nil || source != "discord" || instance != "owner" {
		t.Errorf("ParseRoute() = %q, %q, %v", source, instance, err)
	}
	for _, raw := range []string{"", "https://x.example/y", "a:b:c", ":owner", "discord:", "has space", "UPPER"} {
		if _, _, err := ParseRoute(raw); err == nil {
			t.Errorf("route %q accepted", raw)
		}
	}
}
