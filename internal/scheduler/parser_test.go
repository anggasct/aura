package scheduler

import (
	"testing"
)

func TestParseExpressionFields(t *testing.T) {
	schedule, err := ParseExpression("*/15 9-17 * * 1-5")
	if err != nil {
		t.Fatalf("ParseExpression(): %v", err)
	}
	for _, minute := range []int{0, 15, 30, 45} {
		if !schedule.Minute.contains(minute) {
			t.Errorf("minute %d missing", minute)
		}
	}
	if schedule.Minute.contains(10) {
		t.Error("minute 10 wrongly matched")
	}
	if !schedule.Hour.contains(9) || !schedule.Hour.contains(17) || schedule.Hour.contains(8) {
		t.Errorf("hour range wrong: %+v", schedule.Hour)
	}
	if !schedule.DayOfMonth.starred || !schedule.Month.starred {
		t.Error("day-of-month and month must be starred")
	}
	if schedule.DayOfWeek.starred {
		t.Error("restricted weekday must not be starred")
	}
	if !schedule.DayOfWeek.contains(1) || !schedule.DayOfWeek.contains(5) || schedule.DayOfWeek.contains(0) {
		t.Errorf("weekday range wrong: %+v", schedule.DayOfWeek)
	}
}

func TestParseExpressionNamesAndSunday(t *testing.T) {
	schedule, err := ParseExpression("0 9 * JAN MON")
	if err != nil {
		t.Fatalf("ParseExpression(): %v", err)
	}
	if !schedule.Month.contains(1) || schedule.Month.contains(2) {
		t.Errorf("month name wrong: %+v", schedule.Month)
	}
	if !schedule.DayOfWeek.contains(1) {
		t.Errorf("weekday name wrong: %+v", schedule.DayOfWeek)
	}
	sunday, err := ParseExpression("0 0 * * 7")
	if err != nil {
		t.Fatalf("ParseExpression(): %v", err)
	}
	if !sunday.DayOfWeek.contains(0) || sunday.DayOfWeek.contains(7) {
		t.Errorf("day 7 must normalize to Sunday: %+v", sunday.DayOfWeek)
	}
}

func TestParseExpressionRejectsInvalid(t *testing.T) {
	for _, raw := range []string{
		"",
		"* * * *",
		"* * * * * *",
		"60 * * * *",
		"* 24 * * *",
		"* * 0 * *",
		"* * * 13 *",
		"* * * * 8",
		"*/0 * * * *",
		"5-2 * * * *",
		"*, * * * *",
		"NOPE * * * *",
		"* * * FOO *",
		"1,2, three * * *",
	} {
		if _, err := ParseExpression(raw); err == nil {
			t.Errorf("expression %q accepted", raw)
		} else if code, ok := CodeOf(err); !ok || code != ErrorCodeExpressionInvalid {
			t.Errorf("expression %q code = %v, %v", raw, code, ok)
		}
	}
}

func TestDayMatchesDomDowSemantics(t *testing.T) {
	bothStar, err := ParseExpression("* * * * *")
	if err != nil {
		t.Fatalf("ParseExpression(): %v", err)
	}
	if !dayMatches(&bothStar, 15, 6, 3) {
		t.Error("unrestricted schedule must match any day")
	}
	weekdayOnly, err := ParseExpression("0 0 * * 1")
	if err != nil {
		t.Fatalf("ParseExpression(): %v", err)
	}
	if !dayMatches(&weekdayOnly, 15, 6, 1) || dayMatches(&weekdayOnly, 15, 6, 2) {
		t.Error("weekday-only schedule matches wrong days")
	}
	domOnly, err := ParseExpression("0 0 15 * *")
	if err != nil {
		t.Fatalf("ParseExpression(): %v", err)
	}
	if !dayMatches(&domOnly, 15, 6, 2) || dayMatches(&domOnly, 16, 6, 2) {
		t.Error("day-only schedule matches wrong days")
	}
	either, err := ParseExpression("0 0 15 * 1")
	if err != nil {
		t.Fatalf("ParseExpression(): %v", err)
	}
	if !dayMatches(&either, 15, 6, 2) || !dayMatches(&either, 16, 6, 1) || dayMatches(&either, 16, 6, 2) {
		t.Error("restricted dom+dow must combine with OR")
	}
}

func TestParseSteppedDayFieldsRestricted(t *testing.T) {
	schedule, err := ParseExpression("0 0 */2 * *")
	if err != nil {
		t.Fatalf("ParseExpression(): %v", err)
	}
	if schedule.DayOfMonth.starred {
		t.Error("stepped day-of-month must not be starred")
	}
	if !schedule.DayOfWeek.starred {
		t.Error("unrestricted day-of-week must stay starred")
	}
	if !schedule.DayOfMonth.contains(5) || schedule.DayOfMonth.contains(4) {
		t.Errorf("stepped days wrong: %+v", schedule.DayOfMonth)
	}
}
