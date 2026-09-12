package scheduler

import (
	"testing"
	"time"
)

func mustParse(t *testing.T, raw string) *Schedule {
	t.Helper()
	schedule, err := ParseExpression(raw)
	if err != nil {
		t.Fatalf("ParseExpression(%q): %v", raw, err)
	}
	return &schedule
}

func mustLoad(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("LoadLocation(%q): %v", name, err)
	}
	return loc
}

func TestNextFireMinutely(t *testing.T) {
	schedule := mustParse(t, "* * * * *")
	after := time.Date(2026, 9, 12, 10, 0, 30, 0, time.UTC)
	got, err := NextFire(schedule, time.UTC, after)
	if err != nil {
		t.Fatalf("NextFire(): %v", err)
	}
	if !got.Equal(time.Date(2026, 9, 12, 10, 1, 0, 0, time.UTC)) {
		t.Errorf("next = %v", got)
	}
}

func TestNextFireDailyNineAM(t *testing.T) {
	schedule := mustParse(t, "0 9 * * *")
	utc := time.UTC
	morning, err := NextFire(schedule, utc, time.Date(2026, 9, 12, 8, 0, 0, 0, utc))
	if err != nil {
		t.Fatalf("NextFire(): %v", err)
	}
	if !morning.Equal(time.Date(2026, 9, 12, 9, 0, 0, 0, utc)) {
		t.Errorf("morning fire = %v", morning)
	}
	evening, err := NextFire(schedule, utc, time.Date(2026, 9, 12, 9, 0, 0, 0, utc))
	if err != nil {
		t.Fatalf("NextFire(): %v", err)
	}
	if !evening.Equal(time.Date(2026, 9, 13, 9, 0, 0, 0, utc)) {
		t.Errorf("post-fire next = %v", evening)
	}
}

func TestNextFireTimezoneMatrix(t *testing.T) {
	schedule := mustParse(t, "30 7 * * *")
	cases := []struct {
		zone  string
		after string
		want  string
	}{
		{"UTC", "2026-09-12T06:00:00Z", "2026-09-12T07:30:00Z"},
		{"America/New_York", "2026-09-12T10:00:00Z", "2026-09-12T11:30:00Z"},
		{"Asia/Jakarta", "2026-09-11T23:00:00Z", "2026-09-12T00:30:00Z"},
		{"Australia/Sydney", "2026-09-11T20:00:00Z", "2026-09-11T21:30:00Z"},
	}
	for _, tc := range cases {
		t.Run(tc.zone, func(t *testing.T) {
			after, _ := time.Parse(time.RFC3339, tc.after)
			want, _ := time.Parse(time.RFC3339, tc.want)
			got, err := NextFire(schedule, mustLoad(t, tc.zone), after)
			if err != nil {
				t.Fatalf("NextFire(): %v", err)
			}
			if !got.Equal(want) {
				t.Errorf("next = %v, want %v", got, want)
			}
		})
	}
}

func TestNextFireDSTGapSkips(t *testing.T) {
	schedule := mustParse(t, "30 2 * * *")
	loc := mustLoad(t, "America/New_York")
	got, err := NextFire(schedule, loc, time.Date(2026, 3, 8, 6, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("NextFire(): %v", err)
	}
	if !got.Equal(time.Date(2026, 3, 8, 7, 0, 0, 0, time.UTC)) {
		t.Errorf("gap-day fire = %v, want 03:00 EDT", got)
	}
}

func TestNextFireDSTFoldFiresOnce(t *testing.T) {
	schedule := mustParse(t, "30 1 * * *")
	loc := mustLoad(t, "America/New_York")
	first, err := NextFire(schedule, loc, time.Date(2026, 11, 1, 4, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("NextFire(): %v", err)
	}
	if !first.Equal(time.Date(2026, 11, 1, 5, 30, 0, 0, time.UTC)) {
		t.Errorf("first = %v, want first 01:30 occurrence", first)
	}
	second, err := NextFire(schedule, loc, first)
	if err != nil {
		t.Fatalf("NextFire(): %v", err)
	}
	if !second.Equal(time.Date(2026, 11, 2, 6, 30, 0, 0, time.UTC)) {
		t.Errorf("second = %v, want next-day 01:30 EST", second)
	}
}

func TestNextFireFeb29(t *testing.T) {
	schedule := mustParse(t, "0 0 29 2 *")
	got, err := NextFire(schedule, time.UTC, time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("NextFire(): %v", err)
	}
	if !got.Equal(time.Date(2028, 2, 29, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("leap fire = %v", got)
	}
}

func TestNextFireWeekdayOnly(t *testing.T) {
	schedule := mustParse(t, "0 12 * * MON")
	got, err := NextFire(schedule, time.UTC, time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("NextFire(): %v", err)
	}
	if !got.Equal(time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("monday fire = %v", got)
	}
}

func TestNextFireRejectsNilLocation(t *testing.T) {
	if _, err := NextFire(mustParse(t, "* * * * *"), nil, time.Now()); err == nil {
		t.Error("nil location accepted")
	}
}

func TestNextFireSteppedDayOfMonth(t *testing.T) {
	schedule := mustParse(t, "0 0 */2 * *")
	got, err := NextFire(schedule, time.UTC, time.Date(2026, 4, 3, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("NextFire(): %v", err)
	}
	if !got.Equal(time.Date(2026, 4, 5, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("stepped fire = %v, want 2026-04-05", got)
	}
}

func TestNextFireRejectsNilSchedule(t *testing.T) {
	if _, err := NextFire(nil, time.UTC, time.Now()); err == nil {
		t.Error("nil schedule accepted")
	} else if code, ok := CodeOf(err); !ok || code != ErrorCodeInvalidArgument {
		t.Errorf("nil schedule code = %v, %v", code, ok)
	}
}
