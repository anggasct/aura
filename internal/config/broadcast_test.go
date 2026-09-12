package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	broadcastpkg "github.com/anggasct/aura/internal/broadcast"
)

func writeBroadcastConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

func TestLoad_BroadcastDefaults(t *testing.T) {
	path := writeBroadcastConfig(t, "version: 1\n")
	res, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	broadcast := res.Config.Broadcast
	if broadcast.Timezone != "UTC" {
		t.Errorf("timezone = %q, want UTC", broadcast.Timezone)
	}
	if !broadcast.QuietHours.Enabled || broadcast.QuietHours.Start != "23:00" || broadcast.QuietHours.End != "07:00" {
		t.Errorf("quiet_hours = %+v", broadcast.QuietHours)
	}
	if broadcast.MaxDigestItems != 20 || broadcast.MaxDigestBytes != 12000 {
		t.Errorf("digest bounds = %d/%d", broadcast.MaxDigestItems, broadcast.MaxDigestBytes)
	}
	if broadcast.MaxAttempts != 3 {
		t.Errorf("max_attempts = %d, want 3", broadcast.MaxAttempts)
	}
	if broadcast.MaxDeliveryAge != Duration(24*time.Hour) {
		t.Errorf("max_delivery_age = %v", broadcast.MaxDeliveryAge)
	}
	if broadcast.MinDispatchGap != Duration(5*time.Second) {
		t.Errorf("min_dispatch_gap = %v", broadcast.MinDispatchGap)
	}
	if len(broadcast.Destinations) != 0 || len(broadcast.Fallback) != 0 {
		t.Errorf("routes default = %+v/%+v, want deny-all empty", broadcast.Destinations, broadcast.Fallback)
	}
}

func TestLoad_BroadcastValidRoutes(t *testing.T) {
	path := writeBroadcastConfig(t, `version: 1
broadcast:
  timezone: America/New_York
  destinations:
    default: discord:owner
    plain: discord
    digits: discord2:owner
  fallback:
    default: discord:owner
    empty: ""
`)
	res, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	broadcast := res.Config.Broadcast
	if broadcast.Timezone != "America/New_York" {
		t.Errorf("timezone = %q", broadcast.Timezone)
	}
	if broadcast.Destinations["default"] != "discord:owner" || broadcast.Destinations["plain"] != "discord" {
		t.Errorf("destinations = %+v", broadcast.Destinations)
	}
}

func TestLoad_BroadcastRejectsInvalid(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"bad timezone", "  timezone: Mars/Olympus\n"},
		{"bad quiet start", "  quiet_hours: {enabled: true, start: \"25:00\", end: \"07:00\"}\n"},
		{"bad quiet end", "  quiet_hours: {enabled: true, start: \"23:00\", end: \"7am\"}\n"},
		{"zero digest items", "  max_digest_items: 0\n"},
		{"zero attempts", "  max_attempts: 0\n"},
		{"negative dispatch gap", "  min_dispatch_gap: -5s\n"},
		{"bad alias", "  destinations: {\"BAD ALIAS\": discord}\n"},
		{"credential route", "  destinations: {default: \"https://evil.example/hook\"}\n"},
		{"empty source", "  destinations: {default: \":owner\"}\n"},
		{"slash instance", "  destinations: {default: \"discord:foo/bar\"}\n"},
		{"space instance", "  destinations: {default: \"discord:has space\"}\n"},
		{"long instance", "  destinations: {default: \"discord:" + strings.Repeat("a", 200) + "\"}\n"},
		{"bad fallback", "  fallback: {default: \"a:b:c\"}\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeBroadcastConfig(t, "version: 1\nbroadcast:\n"+tc.body)
			if _, err := Load(path); err == nil {
				t.Errorf("invalid broadcast accepted:\n%s", tc.body)
			}
		})
	}
}

func TestLoad_BroadcastRejectsBadShapes(t *testing.T) {
	path := writeBroadcastConfig(t, "version: 1\nbroadcast:\n  max_attempts: three\n")
	if _, err := Load(path); err == nil {
		t.Error("non-integer max_attempts accepted")
	}
	path = writeBroadcastConfig(t, "version: 1\nbroadcast:\n  quiet_hours: yes\n")
	if _, err := Load(path); err == nil {
		t.Error("non-mapping quiet_hours accepted")
	}
	path = writeBroadcastConfig(t, "version: 1\nbroadcast:\n  destinations: [discord]\n")
	if _, err := Load(path); err == nil {
		t.Error("non-mapping destinations accepted")
	}
	path = writeBroadcastConfig(t, "version: 1\nbroadcast:\n  bogus_key: 1\n")
	if _, err := Load(path); err == nil {
		t.Error("unknown broadcast key accepted")
	}
}

func TestLoad_BroadcastRouteParity(t *testing.T) {
	corpus := []struct {
		route string
		want  bool
	}{
		{"discord:owner", true},
		{"discord", true},
		{"discord2:owner", true},
		{"discord:foo/bar", false},
		{"discord:has space", false},
		{"discord:" + strings.Repeat("a", 200), false},
	}
	for _, tc := range corpus {
		_, _, err := broadcastpkg.ParseRoute(tc.route)
		if got := err == nil; got != tc.want {
			t.Errorf("ParseRoute(%q) = %v, want accept=%v", tc.route, got, tc.want)
		}
		if got := validBroadcastRoute(tc.route); got != tc.want {
			t.Errorf("validBroadcastRoute(%q) = %v, want %v", tc.route, got, tc.want)
		}
		path := writeBroadcastConfig(t, "version: 1\nbroadcast:\n  destinations: {default: \""+tc.route+"\"}\n")
		_, err = Load(path)
		if got := err == nil; got != tc.want {
			t.Errorf("Load(%q) accepted = %v, want %v", tc.route, got, tc.want)
		}
	}
}
