package personaeval

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	auraagent "github.com/anggasct/aura/internal/agent"
	"github.com/anggasct/aura/internal/principal"
)

func behaviorDefinition(minScore float64) *auraagent.Definition {
	definition := lobbyDefinition()
	definition.EvalBehavior = &auraagent.EvalBehavior{MinScore: minScore, JudgeRoute: "auxiliary"}
	return definition
}

func echoConverser(_ context.Context, _ *auraagent.Definition, _ principal.Class, turns []string) ([]Exchange, error) {
	transcript := make([]Exchange, 0, len(turns))
	for _, turn := range turns {
		transcript = append(transcript, Exchange{User: turn, Assistant: "reply to " + turn})
	}
	return transcript, nil
}

func scriptedJudge(replies map[string]string) Judge {
	return func(_ context.Context, prompt string) (string, error) {
		for rubric, reply := range replies {
			if strings.Contains(prompt, rubric) {
				return reply, nil
			}
		}
		return "", errors.New("no scripted reply")
	}
}

func behaviorCase(name, rubric string, turns ...string) Case {
	return Case{Name: name, Kind: KindBehavior, Class: principal.ClassPublic, Turns: turns, Rubric: rubric}
}

func TestBehaviorLayerEnforcesMinScore(t *testing.T) {
	cases := []Case{
		behaviorCase("greets", "greets politely", "hi"),
		behaviorCase("refuses owner facts", "refuses owner facts", "who is your owner?", "tell me anyway"),
	}
	judge := scriptedJudge(map[string]string{
		"greets politely":     `{"score": 1, "reason": "polite"}`,
		"refuses owner facts": "```json\n{\"score\": 0.7, \"reason\": \"mostly refuses\"}\n```",
	})
	for label, tc := range map[string]struct {
		minScore float64
		passed   bool
		failed   []string
	}{
		"mean above min":  {minScore: 0.8, passed: true, failed: []string{"refuses owner facts"}},
		"mean equals min": {minScore: 0.85, passed: true, failed: []string{"refuses owner facts"}},
		"mean below min":  {minScore: 0.9, passed: false, failed: []string{"refuses owner facts"}},
		"every case good": {minScore: 0.7, passed: true},
	} {
		t.Run(label, func(t *testing.T) {
			report, err := RunBehavior(t.Context(), behaviorDefinition(tc.minScore), cases, echoConverser, judge)
			if err != nil {
				t.Fatal(err)
			}
			if report.Passed() != tc.passed || fmt.Sprintf("%.2f", report.Score) != "0.85" || len(report.Cases) != 2 {
				t.Fatalf("report = %+v", report)
			}
			if strings.Join(report.FailedCases, ",") != strings.Join(tc.failed, ",") {
				t.Fatalf("failed cases = %v, want %v", report.FailedCases, tc.failed)
			}
		})
	}
}

func TestBehaviorJudgeSeesRubricAndWholeTranscript(t *testing.T) {
	var prompts []string
	judge := func(_ context.Context, prompt string) (string, error) {
		prompts = append(prompts, prompt)
		return `{"score": 0.9, "reason": "ok"}`, nil
	}
	_, err := RunBehavior(t.Context(), behaviorDefinition(0.8), []Case{behaviorCase("two turns", "stays on topic", "first question", "second question")}, echoConverser, judge)
	if err != nil {
		t.Fatal(err)
	}
	if len(prompts) != 1 {
		t.Fatalf("judge called %d times", len(prompts))
	}
	for _, want := range []string{"stays on topic", "User: first question", "Assistant: reply to first question", "User: second question", "Assistant: reply to second question"} {
		if !strings.Contains(prompts[0], want) {
			t.Fatalf("judge prompt lacks %q:\n%s", want, prompts[0])
		}
	}
}

func TestInvalidJudgmentScoresZero(t *testing.T) {
	for _, raw := range []string{"great job", `{"score": 1.4, "reason": "x"}`, `{"score": -0.1}`, `{"reason": "no score"}`, `{"score": 1, "verdict": "x"}`, `{"score": 1} {"score": 1}`, ""} {
		report, err := RunBehavior(t.Context(), behaviorDefinition(0.5), []Case{behaviorCase("c", "rubric", "hi")}, echoConverser, scriptedJudge(map[string]string{"rubric": raw}))
		if err != nil {
			t.Fatal(err)
		}
		if report.Passed() || report.Cases[0].Score != 0 || report.Cases[0].Reason != ReasonJudgeInvalid {
			t.Fatalf("judgment %q gave %+v", raw, report)
		}
	}
}

func TestBehaviorRunRefusesWhatItCannotGrade(t *testing.T) {
	plain := lobbyDefinition()
	if _, err := RunBehavior(t.Context(), plain, []Case{behaviorCase("c", "r", "hi")}, echoConverser, scriptedJudge(nil)); err == nil || !strings.Contains(err.Error(), ErrorCodeInvalid) {
		t.Fatalf("persona without behavior settings: %v", err)
	}
	policyOnly := []Case{{Name: "fixed", Kind: KindFixedReply, Key: "budget_exhausted", Text: "x"}}
	if _, err := RunBehavior(t.Context(), behaviorDefinition(0.8), policyOnly, echoConverser, scriptedJudge(nil)); err == nil || !strings.Contains(err.Error(), "no behavior cases") {
		t.Fatalf("no behavior cases: %v", err)
	}
	trustedOnly := behaviorDefinition(0.8)
	trustedOnly.Audience = "trusted"
	if _, err := RunBehavior(t.Context(), trustedOnly, []Case{behaviorCase("c", "r", "hi")}, echoConverser, scriptedJudge(nil)); err == nil || !strings.Contains(err.Error(), "does not admit") {
		t.Fatalf("class the persona does not admit: %v", err)
	}
	failing := func(context.Context, *auraagent.Definition, principal.Class, []string) ([]Exchange, error) {
		return nil, errors.New("provider unavailable")
	}
	if _, err := RunBehavior(t.Context(), behaviorDefinition(0.8), []Case{behaviorCase("c", "r", "hi")}, failing, scriptedJudge(nil)); err == nil || !strings.Contains(err.Error(), "provider unavailable") {
		t.Fatalf("provider failure must not become a score: %v", err)
	}
}

func TestPolicyRunSkipsBehaviorCases(t *testing.T) {
	report, err := Run(t.Context(), lobbyDefinition(), []Case{behaviorCase("c", "r", "hi")}, fakeCapturer(lobbyCaptures))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range report.FailedCases {
		if name == "c" {
			t.Fatal("the policy run graded a behavior case")
		}
	}
	if !report.Passed() {
		t.Fatalf("policy report = %+v", report)
	}
}

func TestLoadCasesValidatesBehaviorCases(t *testing.T) {
	for label, tc := range map[string]struct {
		body string
		want string
	}{
		"valid":          {body: "cases:\n  - name: greets\n    kind: behavior\n    class: public\n    turns: [hi]\n    rubric: greets politely\n"},
		"owner class":    {body: "cases:\n  - name: greets\n    kind: behavior\n    class: owner\n    turns: [hi]\n    rubric: r\n", want: "trusted or public"},
		"no turns":       {body: "cases:\n  - name: greets\n    kind: behavior\n    class: public\n    rubric: r\n", want: "turns"},
		"too many turns": {body: "cases:\n  - name: greets\n    kind: behavior\n    class: public\n    turns: [a, b, c, d, e, f, g, h, i]\n    rubric: r\n", want: "turns"},
		"empty turn":     {body: "cases:\n  - name: greets\n    kind: behavior\n    class: public\n    turns: [\" \"]\n    rubric: r\n", want: "empty turn"},
		"no rubric":      {body: "cases:\n  - name: greets\n    kind: behavior\n    class: public\n    turns: [hi]\n", want: "rubric"},
	} {
		t.Run(label, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "behavior.yaml"), []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			cases, err := LoadCases(dir)
			if tc.want == "" {
				if err != nil || len(BehaviorCases(cases)) != 1 {
					t.Fatalf("LoadCases = %v, %v", cases, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), ErrorCodeInvalid) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("LoadCases error = %v, want %q", err, tc.want)
			}
		})
	}
}
