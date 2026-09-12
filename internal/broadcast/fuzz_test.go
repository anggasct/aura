package broadcast

import (
	"strings"
	"testing"
)

func FuzzParseRoute(f *testing.F) {
	seeds := []string{"", "discord", "discord:owner", "a:b:c", "https://evil.example/hook", "UPPER", "has space", "trailing:", ":leading"}
	for _, seed := range seeds {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		source, instance, err := ParseRoute(raw)
		if err != nil {
			return
		}
		if !ValidAlias(source) {
			t.Errorf("accepted invalid source %q from %q", source, raw)
		}
		if strings.Contains(raw, "://") || strings.ContainsAny(raw, " \t\n/") {
			t.Errorf("accepted unsafe route %q", raw)
		}
		if strings.Count(raw, ":") > 1 {
			t.Errorf("accepted multi-segment route %q", raw)
		}
		_ = instance
	})
}

func FuzzRenderText(f *testing.F) {
	seeds := []string{"", "plain", `"quoted"`, `{"text":"hi"}`, `{"a":1}`, "[1,2", "\xff\xfe invalid", "@everyone ping"}
	for _, seed := range seeds {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, content string) {
		rendered := RenderText(content)
		if content == "" && rendered != "" {
			t.Errorf("empty content rendered as %q", rendered)
		}
	})
}

func FuzzParseHourMinute(f *testing.F) {
	seeds := []string{"", "00:00", "23:59", "24:00", "12:60", "1:2", "ab:cd", "12:3x", " 09:00"}
	for _, seed := range seeds {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		minutes, err := ParseHourMinute(raw)
		if err != nil {
			return
		}
		if minutes < 0 || minutes >= 24*60 {
			t.Errorf("minutes %d out of range for %q", minutes, raw)
		}
	})
}
