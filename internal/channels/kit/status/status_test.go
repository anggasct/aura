package status

import (
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/secret"
	runtimechannelhost "github.com/anggasct/aura/internal/platform/turn/channelhost"
)

func TestRenderListsTheLastThreeStepsWithOutcomes(t *testing.T) {
	cases := map[string]struct {
		steps   []Step
		elapsed time.Duration
		texts   Texts
		want    string
	}{
		"zero steps before sixty seconds": {
			elapsed: 7 * time.Second,
			want:    "Working…\n⏱ 0:07",
		},
		"zero steps at sixty seconds": {
			elapsed: 60 * time.Second,
			want:    "Still working…\n⏱ 1:00",
		},
		"zero steps after sixty seconds": {
			elapsed: 12*time.Minute + 30*time.Second,
			want:    "Still working…\n⏱ 12:30",
		},
		"one hour elapsed format": {
			elapsed: time.Hour + 2*time.Minute + 3*time.Second,
			steps: []Step{
				{Activity: "Checking repository", Outcome: runtimechannelhost.StepRunning},
			},
			want: "⏳ Checking repository\n⏱ 1:02:03",
		},
		"marks and oldest-first order": {
			elapsed: 7 * time.Second,
			steps: []Step{
				{Activity: "Reading notes", Outcome: runtimechannelhost.StepDone},
				{Activity: "Checking calendar", Outcome: runtimechannelhost.StepFailed},
				{Activity: "Drafting summary", Outcome: runtimechannelhost.StepRunning},
			},
			want: strings.Join([]string{
				"✓ Reading notes",
				"✗ Checking calendar",
				"⏳ Drafting summary",
				"⏱ 0:07",
			}, "\n"),
		},
		"fourth step drops the oldest": {
			elapsed: 12*time.Minute + 30*time.Second,
			steps: []Step{
				{Activity: "First step", Outcome: runtimechannelhost.StepDone},
				{Activity: "Second step", Outcome: runtimechannelhost.StepDone},
				{Activity: "Third step", Outcome: runtimechannelhost.StepFailed},
				{Activity: "Fourth step", Outcome: runtimechannelhost.StepRunning},
			},
			want: strings.Join([]string{
				"✓ Second step",
				"✗ Third step",
				"⏳ Fourth step",
				"⏱ 12:30",
			}, "\n"),
		},
		"caller supplied texts": {
			elapsed: 75 * time.Second,
			texts: Texts{
				Working:      "Sedang bekerja…",
				StillWorking: "Masih bekerja…",
			},
			want: "Masih bekerja…\n⏱ 1:15",
		},
		"negative elapsed clamps to zero": {
			elapsed: -5 * time.Second,
			want:    "Working…\n⏱ 0:00",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := Render(tc.steps, tc.elapsed, tc.texts, nil)
			if got != tc.want {
				t.Fatalf("Render() =\n%q\nwant:\n%q", got, tc.want)
			}
		})
	}
}

func TestRenderCutsOnWordsWithEllipsisAndStripsMarkdown(t *testing.T) {
	vault := secret.New()
	vault.Set("api", "sk-live-hunter2")
	vault.Set("gh", "ghp_under_score_999")

	t.Run("ninety rune sentence ends at a word with ellipsis", func(t *testing.T) {
		sentence := "Looking through the recent workspace documents and checking every pending task before replying"
		if len([]rune(sentence)) < 90 {
			t.Fatalf("fixture length = %d, want at least 90 runes", len([]rune(sentence)))
		}
		got := Render([]Step{{Activity: sentence, Outcome: runtimechannelhost.StepRunning}}, 5*time.Second, DefaultTexts(), vault)
		firstLine, _, _ := strings.Cut(got, "\n")
		activity := strings.TrimPrefix(firstLine, MarkerRunning+" ")
		if !strings.HasSuffix(activity, "…") {
			t.Fatalf("activity = %q, want trailing ellipsis", activity)
		}
		if n := len([]rune(activity)); n > MaxActivityRunes {
			t.Fatalf("activity length = %d runes (%q), want at most %d", n, activity, MaxActivityRunes)
		}
		withoutEllipsis := strings.TrimSuffix(activity, "…")
		if !strings.HasPrefix(sentence, withoutEllipsis+" ") {
			t.Fatalf("activity = %q cut inside a word of %q", activity, sentence)
		}
	})

	t.Run("strips inline markdown markers and keeps first line only", func(t *testing.T) {
		got := Render([]Step{{
			Activity: "Running `cmd` with **bold**, _italic_, and ~~old~~\nsecond line ignored",
			Outcome:  runtimechannelhost.StepRunning,
		}}, 3*time.Second, DefaultTexts(), vault)
		want := "⏳ Running cmd with bold, italic, and old\n⏱ 0:03"
		if got != want {
			t.Fatalf("Render() = %q, want %q", got, want)
		}
	})

	t.Run("secret value or keyword falls back to working text", func(t *testing.T) {
		cases := map[string]string{
			"vault value":               "Checking sk-live-hunter2 now",
			"vault value with scores":   "Checking ghp_under_score_999 now",
			"keyword password":          "Reading the password file",
			"keyword token":             "Looking at secret-token abc123",
			"keyword api_key":           "Inspecting api_key value",
			"markdown only becomes nil": "```***___~~~",
		}
		for name, raw := range cases {
			t.Run(name, func(t *testing.T) {
				got := Render([]Step{{Activity: raw, Outcome: runtimechannelhost.StepRunning}}, 4*time.Second, DefaultTexts(), vault)
				want := "⏳ Working…\n⏱ 0:04"
				if got != want {
					t.Fatalf("Render(%q) = %q, want %q", raw, got, want)
				}
				for _, leaked := range []string{"hunter2", "ghp_under_score_999", "password", "abc123"} {
					if strings.Contains(strings.ToLower(got), leaked) {
						t.Fatalf("Render(%q) leaked %q: %q", raw, leaked, got)
					}
				}
			})
		}
	})

	t.Run("live example renders without goal text", func(t *testing.T) {
		modelLine := "The error says the executor field is missing, checking the latest Go release notes now."
		goalText := "Cari berita terbaru soal Go 1.27 di web, ringkas hasilnya, lalu laporkan ringkas"
		got := Render([]Step{{Activity: modelLine, Outcome: runtimechannelhost.StepRunning}}, 12*time.Second, DefaultTexts(), vault)
		if strings.Contains(got, goalText) || strings.Contains(got, "Cari berita") {
			t.Fatalf("Render() included tool goal text: %q", got)
		}
		firstLine, _, _ := strings.Cut(got, "\n")
		activity := strings.TrimPrefix(firstLine, MarkerRunning+" ")
		if !strings.HasSuffix(activity, "…") || len([]rune(activity)) > MaxActivityRunes {
			t.Fatalf("activity = %q, want word-cut with ellipsis within %d runes", activity, MaxActivityRunes)
		}
	})
}

func TestSilentCallStartsAStepWithTheWorkingText(t *testing.T) {
	got := Render([]Step{{Activity: "", Outcome: runtimechannelhost.StepRunning}}, 9*time.Second, DefaultTexts(), nil)
	want := "⏳ Working…\n⏱ 0:09"
	if got != want {
		t.Fatalf("Render(silent step) = %q, want %q", got, want)
	}
	custom := Render([]Step{{Activity: "   ", Outcome: runtimechannelhost.StepDone}}, 9*time.Second, Texts{Working: "Memproses…"}, nil)
	wantCustom := "✓ Memproses…\n⏱ 0:09"
	if custom != wantCustom {
		t.Fatalf("Render(silent step custom) = %q, want %q", custom, wantCustom)
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

func TestApplyUpdateMatchesByCallID(t *testing.T) {
	var steps []TrackedStep
	steps = ApplyUpdate(steps, runtimechannelhost.PresenceUpdate{
		CallIDs:  []string{"c1", "c2"},
		Activity: "First step",
		Outcome:  runtimechannelhost.StepRunning,
	})
	steps = ApplyUpdate(steps, runtimechannelhost.PresenceUpdate{
		CallIDs:  []string{"c3"},
		Activity: "Second step",
		Outcome:  runtimechannelhost.StepRunning,
	})
	steps = ApplyUpdate(steps, runtimechannelhost.PresenceUpdate{
		CallIDs:  []string{"c1", "c2"},
		Activity: "First step",
		Outcome:  runtimechannelhost.StepFailed,
	})
	if len(steps) != 2 {
		t.Fatalf("steps = %+v, want 2 steps updated in place", steps)
	}
	if steps[0].Step.Outcome != runtimechannelhost.StepFailed || steps[1].Step.Outcome != runtimechannelhost.StepRunning {
		t.Fatalf("outcomes = [%s, %s], want [failed, running]", steps[0].Step.Outcome, steps[1].Step.Outcome)
	}
}
