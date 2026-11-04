package status

import (
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/secret"
)

func TestTruncateWordsKeepsAtMostNWords(t *testing.T) {
	cases := map[string]struct {
		in   string
		want string
	}{
		"longer":       {in: "one two three four five six seven", want: "one two three four five six"},
		"exact":        {in: "one two three four five six", want: "one two three four five six"},
		"collapses ws": {in: "  one\t two\n", want: "one two"},
		"empty":        {in: "", want: ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := TruncateWords(tc.in, MaxWords); got != tc.want {
				t.Fatalf("TruncateWords(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestTruncateRunesCutsOnRuneBoundaries(t *testing.T) {
	if got := TruncateRunes("héllo😀world", 6); got != "héllo😀" {
		t.Fatalf("TruncateRunes = %q", got)
	}
	if got := TruncateRunes("short", 10); got != "short" {
		t.Fatalf("TruncateRunes = %q", got)
	}
}

func TestFallbackCyclesAndClampsNegativeCounts(t *testing.T) {
	all := Fallbacks()
	if len(all) != 3 {
		t.Fatalf("fallbacks = %v", all)
	}
	for i := range 6 {
		if got := Fallback(i); got != all[i%3] {
			t.Fatalf("Fallback(%d) = %q", i, got)
		}
	}
	if Fallback(-4) != all[0] {
		t.Fatal("negative count is not clamped")
	}
	all[0] = "mutated"
	if Fallback(0) == "mutated" {
		t.Fatal("Fallbacks exposes the shared array")
	}
}

func TestFailureLineMarksTheActivity(t *testing.T) {
	cases := map[string]string{
		"":                FailureMarker,
		"  ":              FailureMarker,
		"Reading notes.":  FailureMarker + " Reading notes",
		" Reading notes ": FailureMarker + " Reading notes",
	}
	for in, want := range cases {
		t.Run(in, func(t *testing.T) {
			if got := FailureLine(in); got != want {
				t.Fatalf("FailureLine(%q) = %q, want %q", in, got, want)
			}
		})
	}
}

func TestContainsSecretKeywordIsCaseInsensitive(t *testing.T) {
	for _, in := range []string{"my PASSWORD", "Bearer abc", "api_key=1", "client_secret", "Authorization header"} {
		if !ContainsSecretKeyword(in) {
			t.Errorf("ContainsSecretKeyword(%q) = false", in)
		}
	}
	if ContainsSecretKeyword("reading notes.md") {
		t.Error("plain activity flagged as secret")
	}
}

func TestHumanizeArgsFlattensJSONValues(t *testing.T) {
	human := HumanizeArgs(`{"path":"src/main.go","query":"hello"}`)
	if strings.Contains(human, "{") || strings.Contains(human, `"`) {
		t.Errorf("humanized args leak JSON: %q", human)
	}
	if !strings.Contains(human, "src/main.go") || !strings.Contains(human, "hello") {
		t.Errorf("humanized args = %q, want values preserved", human)
	}
	if got := HumanizeArgs(`{"a":["x",{"b":"y"}],"n":3}`); !strings.Contains(got, "x") || !strings.Contains(got, "y") {
		t.Errorf("nested args = %q", got)
	}
	for _, in := range []string{"", "  ", "not json", `{"n":1}`, `{"a":"x"} {"b":"y"}`} {
		if got := HumanizeArgs(in); got != "" {
			t.Errorf("HumanizeArgs(%q) = %q, want empty", in, got)
		}
	}
	if got := HumanizeArgs(`{"q":"` + strings.Repeat("z", 200) + `"}`); len([]rune(got)) != maxArgRunes {
		t.Errorf("long args = %d runes, want %d", len([]rune(got)), maxArgRunes)
	}
}

func TestRenderScreensSecretsAndFallsBack(t *testing.T) {
	vault := secret.New()
	vault.Set("api", "sk-live-hunter2")
	cases := map[string]struct {
		raw     string
		args    string
		count   int
		want    string
		without []string
	}{
		"blank uses fallback":           {raw: "  ", count: 1, want: Fallback(1)},
		"truncates to six words":        {raw: "one two three four five six seven", want: "one two three four five six"},
		"appends humanized args":        {raw: "Reading", args: `{"path":"notes.md"}`, want: "Reading notes.md"},
		"keyword activity falls back":   {raw: "Looking at secret-token abc123", count: 2, want: Fallback(2)},
		"keyword args omitted":          {raw: "Looking through notes", args: `{"path":"secret-token abc123"}`, want: "Looking through notes"},
		"vault args omitted":            {raw: "Searching", args: `{"q":"sk-live-hunter2"}`, want: "Searching", without: []string{"hunter2"}},
		"vault value redacted in text":  {raw: "Using sk-live-hunter2 now", without: []string{"sk-live-hunter2"}},
		"unparseable parallel args off": {raw: "Mencari catatan", args: `{"pwd":"hunter2"} {"path":"notes.md"}`, want: "Mencari catatan"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := Render(tc.raw, tc.args, tc.count, vault)
			if tc.want != "" && got != tc.want {
				t.Fatalf("Render = %q, want %q", got, tc.want)
			}
			for _, leaked := range tc.without {
				if strings.Contains(got, leaked) {
					t.Fatalf("Render = %q leaked %q", got, leaked)
				}
			}
		})
	}
	if got := Render("Reading", `{"path":"notes.md"}`, 0, nil); got != "Reading notes.md" {
		t.Fatalf("Render without vault = %q", got)
	}
}

func TestRenderFailureScreensTheActivity(t *testing.T) {
	vault := secret.New()
	vault.Set("api", "sk-live-hunter2")
	cases := map[string]struct {
		in   string
		want string
	}{
		"plain":        {in: "Reading the notes file now please quickly", want: FailureMarker + " Reading the notes file now please"},
		"keyword":      {in: "Reading the password file", want: FailureMarker},
		"blank":        {in: "", want: FailureMarker},
		"vault secret": {in: "Calling sk-live-hunter2", want: FailureMarker + " Calling [redacted]"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := RenderFailure(tc.in, vault)
			if strings.Contains(got, "hunter2") {
				t.Fatalf("RenderFailure leaked the secret: %q", got)
			}
			if got != tc.want {
				t.Fatalf("RenderFailure(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
