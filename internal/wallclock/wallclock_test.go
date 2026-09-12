package wallclock

import (
	"testing"
	"time"
)

func TestResolveLocalInstantNormalDay(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	got := ResolveLocalInstant(loc, 2026, time.September, 12, 10, 30)
	want := time.Date(2026, time.September, 12, 14, 30, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestResolveLocalInstantGapAdvances(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	got := ResolveLocalInstant(loc, 2026, time.March, 8, 2, 30)
	want := time.Date(2026, time.March, 8, 7, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("gap resolved to %v, want first valid instant %v", got, want)
	}
}

func TestResolveLocalInstantFoldTakesFirst(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	got := ResolveLocalInstant(loc, 2026, time.November, 1, 1, 30)
	want := time.Date(2026, time.November, 1, 5, 30, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("fold resolved to %v, want first occurrence %v", got, want)
	}
}

func TestResolveLocalInstantValidAfterGap(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	got := ResolveLocalInstant(loc, 2026, time.March, 8, 3, 30)
	want := time.Date(2026, time.March, 8, 7, 30, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestResolveLocalInstantSouthernHemisphere(t *testing.T) {
	loc, err := time.LoadLocation("Australia/Sydney")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	got := ResolveLocalInstant(loc, 2026, time.October, 4, 2, 30)
	wall := got.In(loc)
	if wall.Hour() != 3 || wall.Minute() != 0 {
		t.Errorf("Sydney gap resolved to wall %v, want 03:00", wall.Format("15:04"))
	}
	fold := ResolveLocalInstant(loc, 2026, time.April, 5, 2, 30).In(loc)
	_, offset := fold.Zone()
	if offset != 11*3600 {
		t.Errorf("Sydney fold offset = %d, want AEDT (+11) first occurrence", offset)
	}
}

func TestResolveLocalInstantNoDSTZone(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Jakarta")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	got := ResolveLocalInstant(loc, 2026, time.March, 8, 2, 30)
	want := time.Date(2026, time.March, 7, 19, 30, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("got %v, want %v", got, want)
	}
}
