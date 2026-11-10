package profile

import (
	"strings"
	"testing"
)

type stubScanner struct {
	secrets []string
}

func (s *stubScanner) Contains(text string) bool {
	for _, secret := range s.secrets {
		if strings.Contains(text, secret) {
			return true
		}
	}
	return false
}

func TestSensitiveReasonCorpus(t *testing.T) {
	scanner := &stubScanner{secrets: []string{"sk-live-abc123"}}
	cases := map[string]struct {
		category, key, value string
		want                 string
	}{
		"clean preference":  {"preference", "backend", "Go", ""},
		"clean tool":        {"tool", "editor", "neovim", ""},
		"health trait":      {"preference", "doctor", "prefers drumchapel health centre", "sensitive trait"},
		"religion trait":    {"habit", "sunday", "goes to church weekly", "sensitive trait"},
		"politics trait":    {"preference", "news", "votes in every election", "sensitive trait"},
		"password key":      {"tool", "db password", "rotated", "sensitive trait"},
		"token value":       {"project", "deploy", "uses token abc for deploys", "sensitive trait"},
		"home address":      {"habit", "evening", "home address is 12 Rose Street", "sensitive trait"},
		"coordinates":       {"preference", "running", "gps coordinates 55.8612,-4.2502", "sensitive trait"},
		"whereabouts":       {"habit", "noon", "current whereabouts are the Glasgow office", "sensitive trait"},
		"location category": {"location", "base", "works downtown", "sensitive trait"},
		"secret value":      {"preference", "backend", "key is sk-live-abc123", "secret-like value"},
		"case folded":       {"habit", "morning", "PASSWORD reset daily", "sensitive trait"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := SensitiveReason(tc.category, tc.key, tc.value, scanner); got != tc.want {
				t.Errorf("reason = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSensitiveReasonWithoutScanner(t *testing.T) {
	if got := SensitiveReason("tool", "editor", "neovim", nil); got != "" {
		t.Errorf("reason = %q", got)
	}
	if got := SensitiveReason("habit", "clinic", "visits weekly", nil); got == "" {
		t.Errorf("trait missed without scanner")
	}
}

func TestParseCandidates(t *testing.T) {
	text := `[{"category":"language","key":"backend","value":"Go","sources":[4,7]},{"category":"tool","key":"editor","value":"neovim","sources":[9]}]`
	candidates, err := ParseCandidates(text)
	if err != nil {
		t.Fatalf("ParseCandidates: %v", err)
	}
	if len(candidates) != 2 || candidates[0].Sources[1] != 7 {
		t.Errorf("candidates = %+v", candidates)
	}
}

func TestParseCandidatesSkipsIncomplete(t *testing.T) {
	text := `[{"category":"language","key":"backend","value":"Go","sources":[1]},{"category":"","key":"x","value":"y","sources":[1]},{"category":"tool","key":"e","value":"","sources":[]}]`
	candidates, err := ParseCandidates(text)
	if err != nil {
		t.Fatalf("ParseCandidates: %v", err)
	}
	if len(candidates) != 1 {
		t.Errorf("candidates = %+v", candidates)
	}
}

func TestParseCandidatesRejects(t *testing.T) {
	for name, text := range map[string]string{
		"empty":    "",
		"prose":    "no facts here",
		"object":   `{"category":"language"}`,
		"bad json": `[{broken]`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseCandidates(text); err == nil {
				t.Errorf("expected rejection")
			} else if code, ok := CodeOf(err); !ok || code != ErrorCodeProfileInvalid {
				t.Errorf("code = %v, %v (%v)", code, ok, err)
			}
		})
	}
}

func TestExtractPrompt(t *testing.T) {
	first, err := ExtractPrompt("v1", 2048)
	if err != nil {
		t.Fatalf("ExtractPrompt: %v", err)
	}
	second, err := ExtractPrompt("v1", 2048)
	if err != nil {
		t.Fatalf("ExtractPrompt: %v", err)
	}
	if first != second {
		t.Errorf("prompt is not deterministic")
	}
	if _, err := ExtractPrompt("v9", 2048); err == nil {
		t.Errorf("expected version rejection")
	}
	if _, err := ExtractPrompt("v1", 0); err == nil {
		t.Errorf("expected bound rejection")
	}
}
