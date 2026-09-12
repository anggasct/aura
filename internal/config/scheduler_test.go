package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeSchedulerConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

func TestLoad_SchedulerDefaults(t *testing.T) {
	path := writeSchedulerConfig(t, "version: 1\n")
	res, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	scheduler := res.Config.Scheduler
	if !scheduler.Enabled {
		t.Error("scheduler must be enabled by default")
	}
	if scheduler.DefaultTimezone != "UTC" {
		t.Errorf("default_timezone = %q", scheduler.DefaultTimezone)
	}
	if scheduler.DefaultCatchUpGrace != Duration(15*time.Minute) {
		t.Errorf("default_catch_up_grace = %v", scheduler.DefaultCatchUpGrace)
	}
	if scheduler.OccurrenceRetention != Duration(720*time.Hour) {
		t.Errorf("occurrence_retention = %v", scheduler.OccurrenceRetention)
	}
}

func TestLoad_SchedulerValid(t *testing.T) {
	path := writeSchedulerConfig(t, `version: 1
scheduler:
  enabled: false
  default_timezone: America/New_York
  default_catch_up_grace: 5m
  occurrence_retention: 48h
`)
	res, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	scheduler := res.Config.Scheduler
	if scheduler.Enabled || scheduler.DefaultTimezone != "America/New_York" {
		t.Errorf("scheduler = %+v", scheduler)
	}
}

func TestLoad_SchedulerRejectsInvalid(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"bad timezone", "  default_timezone: Mars/Olympus\n"},
		{"zero retention", "  occurrence_retention: 0s\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeSchedulerConfig(t, "version: 1\nscheduler:\n"+tc.body)
			if _, err := Load(path); err == nil {
				t.Errorf("invalid scheduler accepted:\n%s", tc.body)
			}
		})
	}
}

func TestLoad_SchedulerRejectsBadShapes(t *testing.T) {
	path := writeSchedulerConfig(t, "version: 1\nscheduler:\n  enabled: yes\n")
	if _, err := Load(path); err == nil {
		t.Error("non-boolean enabled accepted")
	}
	path = writeSchedulerConfig(t, "version: 1\nscheduler:\n  bogus_key: 1\n")
	if _, err := Load(path); err == nil {
		t.Error("unknown scheduler key accepted")
	}
}
