package broadcast

import (
	"fmt"
	"strings"
	"time"
)

type QuietConfig struct {
	Enabled  bool
	Location *time.Location
	StartMin int
	EndMin   int
}

func ParseHourMinute(value string) (int, error) {
	if len(value) != 5 || value[2] != ':' {
		return 0, Errorf(ErrorCodeInvalidArgument, "time is not HH:MM")
	}
	var hour, minute int
	if _, err := fmt.Sscanf(value, "%d:%d", &hour, &minute); err != nil {
		return 0, Errorf(ErrorCodeInvalidArgument, "time is not HH:MM")
	}
	if fmt.Sprintf("%02d:%02d", hour, minute) != value {
		return 0, Errorf(ErrorCodeInvalidArgument, "time is not HH:MM")
	}
	if strings.ContainsAny(value, " ") || hour < 0 || hour > 23 || minute < 0 || minute > 59 {
		return 0, Errorf(ErrorCodeInvalidArgument, "time is not HH:MM")
	}
	return hour*60 + minute, nil
}

func inQuietWindow(wallMin, startMin, endMin int) bool {
	if startMin == endMin {
		return false
	}
	if startMin < endMin {
		return wallMin >= startMin && wallMin < endMin
	}
	return wallMin >= startMin || wallMin < endMin
}

func resolveLocalInstant(loc *time.Location, year int, month time.Month, day, hour, minute int) time.Time {
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

func quietRelease(now time.Time, quiet QuietConfig) time.Time {
	wall := now.In(quiet.Location)
	wallMin := wall.Hour()*60 + wall.Minute()
	if !inQuietWindow(wallMin, quiet.StartMin, quiet.EndMin) {
		return now
	}
	endHour, endMinute := quiet.EndMin/60, quiet.EndMin%60
	release := resolveLocalInstant(quiet.Location, wall.Year(), wall.Month(), wall.Day(), endHour, endMinute)
	if !release.After(now) {
		next := now.In(quiet.Location).AddDate(0, 0, 1)
		release = resolveLocalInstant(quiet.Location, next.Year(), next.Month(), next.Day(), endHour, endMinute)
	}
	return release
}

func (b *Broadcaster) releaseFor(priority string, now time.Time) (state string, notBefore time.Time) {
	if priority == PriorityUrgent || !b.quiet.Enabled || b.quiet.Location == nil {
		return StateScheduled, now
	}
	release := quietRelease(now, b.quiet)
	if release.Equal(now) {
		return StateScheduled, now
	}
	return StateHeld, release
}
