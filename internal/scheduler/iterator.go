package scheduler

import (
	"time"

	"github.com/anggasct/aura/internal/wallclock"
)

const maxIteratorJumps = 200000

func NextFire(schedule *Schedule, loc *time.Location, after time.Time) (time.Time, error) {
	if loc == nil {
		return time.Time{}, Errorf(ErrorCodeTimezoneInvalid, "location must not be nil")
	}
	base := after.In(loc).Truncate(time.Minute).Add(time.Minute)
	year, month, day := base.Date()
	hour, minute, _ := base.Clock()
	for range maxIteratorJumps {
		nextMonth, ok := schedule.Month.nextAtOrAfter(int(month))
		if !ok {
			year++
			month = time.Month(schedule.Month.values[0])
			day, hour, minute = 1, 0, 0
			continue
		}
		if nextMonth != int(month) {
			month = time.Month(nextMonth)
			day, hour, minute = 1, 0, 0
			continue
		}
		weekday := time.Date(year, month, day, 12, 0, 0, 0, loc).Weekday()
		if !dayMatches(schedule, day, int(month), int(weekday)) {
			year, month, day = nextDay(loc, year, month, day)
			hour, minute = 0, 0
			continue
		}
		nextHour, ok := schedule.Hour.nextAtOrAfter(hour)
		if !ok {
			year, month, day = nextDay(loc, year, month, day)
			hour, minute = 0, 0
			continue
		}
		if nextHour != hour {
			hour, minute = nextHour, 0
			continue
		}
		nextMinute, ok := schedule.Minute.nextAtOrAfter(minute)
		if !ok {
			hour++
			minute = 0
			if hour > 23 {
				year, month, day = nextDay(loc, year, month, day)
				hour = 0
			}
			continue
		}
		if nextMinute != minute {
			minute = nextMinute
			continue
		}
		resolved := wallclock.ResolveLocalInstant(loc, year, month, day, hour, minute)
		if !resolved.After(after) {
			minute++
			if minute > 59 {
				minute = 0
				hour++
			}
			continue
		}
		return resolved, nil
	}
	return time.Time{}, Errorf(ErrorCodeExpressionInvalid, "expression never fires")
}

func nextDay(loc *time.Location, year int, month time.Month, day int) (nextYear int, nextMonth time.Month, nextDay int) {
	next := time.Date(year, month, day, 12, 0, 0, 0, loc).AddDate(0, 0, 1)
	nextYear, nextMonth, nextDay = next.Date()
	return nextYear, nextMonth, nextDay
}
