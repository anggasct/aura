package sqlite

import "time"

func FormatTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

func ParseTime(text string) (time.Time, error) {
	return time.Parse(time.RFC3339Nano, text)
}
