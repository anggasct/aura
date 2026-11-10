package wallclock

import "time"

func ResolveLocalInstant(loc *time.Location, year int, month time.Month, day, hour, minute int) time.Time {
	roundTrips := func(instant time.Time) bool {
		wall := instant.In(loc)
		return wall.Year() == year && wall.Month() == month && wall.Day() == day &&
			wall.Hour() == hour && wall.Minute() == minute
	}
	midnight := time.Date(year, month, day, 0, 0, 0, 0, loc)
	_, midnightOffset := midnight.Zone()
	first := time.Date(year, month, day, hour, minute, 0, 0, time.UTC).Add(-time.Duration(midnightOffset) * time.Second)
	if roundTrips(first) {
		return first
	}
	_, guessOffset := first.Zone()
	second := time.Date(year, month, day, hour, minute, 0, 0, time.UTC).Add(-time.Duration(guessOffset) * time.Second)
	if roundTrips(second) {
		return second
	}
	target := hour*60 + minute
	start := time.Date(year, month, day, 0, 0, 0, 0, loc)
	for step := range 24 * 60 {
		forward := start.Add(time.Duration(step+1) * time.Minute)
		wall := forward.In(loc)
		if wall.Year() != year || wall.Month() != month || wall.Day() != day {
			return forward
		}
		if wall.Hour()*60+wall.Minute() >= target {
			return forward
		}
	}
	return first
}
