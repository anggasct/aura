package broadcast

import (
	"testing"
	"time"
)

func testQuietBroadcaster(t *testing.T, zone, start, end string) *Broadcaster {
	t.Helper()
	loc, err := time.LoadLocation(zone)
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	startMin, err := ParseHourMinute(start)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	endMin, err := ParseHourMinute(end)
	if err != nil {
		t.Fatalf("end: %v", err)
	}
	policy := testPolicy()
	policy.Quiet = QuietConfig{Enabled: true, Location: loc, StartMin: startMin, EndMin: endMin}
	broadcaster, err := New(
		policy,
		&fakeRegistry{registered: map[string]bool{"discord": true}},
		newFakeItemStore(),
		nil,
	)
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	return broadcaster
}

func submitAt(t *testing.T, broadcaster *Broadcaster, priority, at string) Item {
	t.Helper()
	notification := testNotification()
	notification.Priority = priority
	notification.IdempotencyKey = priority + at
	moment, err := time.Parse(time.RFC3339, at)
	if err != nil {
		t.Fatalf("parse time: %v", err)
	}
	notification.CreatedAt = moment
	item, _, err := broadcaster.Submit(t.Context(), notification)
	if err != nil {
		t.Fatalf("Submit(): %v", err)
	}
	return item
}

func TestQuietHours_SameDayWindow(t *testing.T) {
	broadcaster := testQuietBroadcaster(t, "UTC", "09:00", "17:00")
	held := submitAt(t, broadcaster, PriorityInfo, "2026-09-12T10:00:00Z")
	if held.State != StateHeld {
		t.Errorf("10:00 state = %q, want held", held.State)
	}
	if !held.NotBefore.Equal(time.Date(2026, 9, 12, 17, 0, 0, 0, time.UTC)) {
		t.Errorf("not_before = %v, want 17:00 UTC", held.NotBefore)
	}
	open := submitAt(t, broadcaster, PriorityWarning, "2026-09-12T18:00:00Z")
	if open.State != StateScheduled || !open.NotBefore.Equal(time.Date(2026, 9, 12, 18, 0, 0, 0, time.UTC)) {
		t.Errorf("18:00 = %+v, want scheduled now", open)
	}
	edge := submitAt(t, broadcaster, PriorityInfo, "2026-09-12T17:00:00Z")
	if edge.State != StateScheduled {
		t.Errorf("17:00 end-exclusive state = %q, want scheduled", edge.State)
	}
}

func TestQuietHours_CrossMidnightWindow(t *testing.T) {
	broadcaster := testQuietBroadcaster(t, "UTC", "23:00", "07:00")
	evening := submitAt(t, broadcaster, PriorityInfo, "2026-09-12T23:30:00Z")
	if evening.State != StateHeld {
		t.Fatalf("23:30 state = %q, want held", evening.State)
	}
	if !evening.NotBefore.Equal(time.Date(2026, 9, 13, 7, 0, 0, 0, time.UTC)) {
		t.Errorf("evening release = %v, want next-day 07:00 UTC", evening.NotBefore)
	}
	night := submitAt(t, broadcaster, PriorityWarning, "2026-09-13T02:00:00Z")
	if night.State != StateHeld || !night.NotBefore.Equal(time.Date(2026, 9, 13, 7, 0, 0, 0, time.UTC)) {
		t.Errorf("02:00 = %+v, want held to same 07:00 UTC", night)
	}
	morning := submitAt(t, broadcaster, PriorityInfo, "2026-09-13T07:00:00Z")
	if morning.State != StateScheduled {
		t.Errorf("07:00 state = %q, want scheduled", morning.State)
	}
}

func TestQuietHours_UrgentBypasses(t *testing.T) {
	broadcaster := testQuietBroadcaster(t, "UTC", "23:00", "07:00")
	urgent := submitAt(t, broadcaster, PriorityUrgent, "2026-09-12T23:30:00Z")
	if urgent.State != StateScheduled || !urgent.NotBefore.Equal(time.Date(2026, 9, 12, 23, 30, 0, 0, time.UTC)) {
		t.Errorf("urgent = %+v, want scheduled now", urgent)
	}
}

func TestQuietHours_RespectsTimezone(t *testing.T) {
	broadcaster := testQuietBroadcaster(t, "America/New_York", "23:00", "07:00")
	midnight := submitAt(t, broadcaster, PriorityInfo, "2026-09-12T04:00:00Z")
	if midnight.State != StateHeld {
		t.Fatalf("00:00 EDT state = %q, want held", midnight.State)
	}
	if !midnight.NotBefore.Equal(time.Date(2026, 9, 12, 11, 0, 0, 0, time.UTC)) {
		t.Errorf("release = %v, want 07:00 EDT (11:00 UTC)", midnight.NotBefore)
	}
	evening := submitAt(t, broadcaster, PriorityInfo, "2026-09-12T02:00:00Z")
	if evening.State != StateScheduled {
		t.Errorf("22:00 EDT state = %q, want scheduled", evening.State)
	}
}

func TestQuietHours_DSTGapAdvancesRelease(t *testing.T) {
	broadcaster := testQuietBroadcaster(t, "America/New_York", "00:00", "02:30")
	early := submitAt(t, broadcaster, PriorityInfo, "2026-03-08T05:30:00Z")
	if early.State != StateHeld {
		t.Fatalf("00:30 EST state = %q, want held", early.State)
	}
	if !early.NotBefore.Equal(time.Date(2026, 3, 8, 7, 0, 0, 0, time.UTC)) {
		t.Errorf("gap release = %v, want 03:00 EDT (07:00 UTC)", early.NotBefore)
	}
}

func TestQuietHours_DSTFoldChoosesFirstOccurrence(t *testing.T) {
	broadcaster := testQuietBroadcaster(t, "America/New_York", "00:00", "01:30")
	early := submitAt(t, broadcaster, PriorityInfo, "2026-11-01T04:30:00Z")
	if early.State != StateHeld {
		t.Fatalf("00:30 EDT state = %q, want held", early.State)
	}
	if !early.NotBefore.Equal(time.Date(2026, 11, 1, 5, 30, 0, 0, time.UTC)) {
		t.Errorf("fold release = %v, want first 01:30 EDT (05:30 UTC)", early.NotBefore)
	}
}

func TestQuietHours_ReplayPreservesRelease(t *testing.T) {
	broadcaster := testQuietBroadcaster(t, "UTC", "23:00", "07:00")
	notification := testNotification()
	notification.CreatedAt = time.Date(2026, 9, 12, 23, 30, 0, 0, time.UTC)
	first, replayed, err := broadcaster.Submit(t.Context(), notification)
	if err != nil || replayed {
		t.Fatalf("first submit = %+v, %v, %v", first.ID, replayed, err)
	}
	second, replayed, err := broadcaster.Submit(t.Context(), notification)
	if err != nil || !replayed {
		t.Fatalf("replay submit = %+v, %v, %v", second.ID, replayed, err)
	}
	if second.ID != first.ID || second.State != StateHeld || !second.NotBefore.Equal(first.NotBefore) {
		t.Errorf("replay changed the held release: %+v vs %+v", second, first)
	}
}

func TestParseHourMinute(t *testing.T) {
	if minutes, err := ParseHourMinute("23:00"); err != nil || minutes != 1380 {
		t.Errorf("ParseHourMinute(23:00) = %d, %v", minutes, err)
	}
	for _, raw := range []string{"", "7am", "25:00", "12:60", "1:2", "12:3x", " 09:00"} {
		if _, err := ParseHourMinute(raw); err == nil {
			t.Errorf("time %q accepted", raw)
		}
	}
}
