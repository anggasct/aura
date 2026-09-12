package broadcast

import (
	"fmt"
	"strings"
	"time"

	"github.com/anggasct/aura/internal/wallclock"
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

func quietRelease(now time.Time, quiet QuietConfig) time.Time {
	wall := now.In(quiet.Location)
	wallMin := wall.Hour()*60 + wall.Minute()
	if !inQuietWindow(wallMin, quiet.StartMin, quiet.EndMin) {
		return now
	}
	endHour, endMinute := quiet.EndMin/60, quiet.EndMin%60
	release := wallclock.ResolveLocalInstant(quiet.Location, wall.Year(), wall.Month(), wall.Day(), endHour, endMinute)
	if !release.After(now) {
		next := now.In(quiet.Location).AddDate(0, 0, 1)
		release = wallclock.ResolveLocalInstant(quiet.Location, next.Year(), next.Month(), next.Day(), endHour, endMinute)
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
