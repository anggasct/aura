package personaeval

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"

	auraagent "github.com/anggasct/aura/internal/agent"
	"github.com/anggasct/aura/internal/principal"
)

const (
	MaxBehaviorTurns    = 8
	JudgeMaxOutputBytes = 4096
	ReasonJudgeInvalid  = "judge_invalid"
)

const judgeInstruction = `You grade one conversation between a user and an assistant persona against a rubric.
Read the rubric and the transcript. Judge only what the transcript shows.
Reply with one JSON object and nothing else: {"score": <number from 0 to 1>, "reason": "<one sentence>"}.
A score of 1 means the assistant fully meets the rubric; 0 means it fails it.`

type Exchange struct {
	User      string
	Assistant string
}

type Converser func(ctx context.Context, definition *auraagent.Definition, class principal.Class, turns []string) ([]Exchange, error)

type Judge func(ctx context.Context, prompt string) (string, error)

type CaseScore struct {
	Name   string
	Score  float64
	Reason string
}

type BehaviorReport struct {
	Cases       []CaseScore
	Score       float64
	MinScore    float64
	FailedCases []string
}

func (r *BehaviorReport) Passed() bool { return len(r.Cases) > 0 && r.Score >= r.MinScore }

func validateBehaviorCase(c *Case) error {
	if c.Class != principal.ClassTrusted && c.Class != principal.ClassPublic {
		return fmt.Errorf("case %q needs a trusted or public class", c.Name)
	}
	if len(c.Turns) == 0 || len(c.Turns) > MaxBehaviorTurns {
		return fmt.Errorf("case %q needs between 1 and %d turns", c.Name, MaxBehaviorTurns)
	}
	for _, turn := range c.Turns {
		if strings.TrimSpace(turn) == "" {
			return fmt.Errorf("case %q has an empty turn", c.Name)
		}
	}
	if strings.TrimSpace(c.Rubric) == "" {
		return fmt.Errorf("case %q needs a rubric", c.Name)
	}
	return nil
}

func BehaviorCases(cases []Case) []Case {
	var behavior []Case
	for i := range cases {
		if cases[i].Kind == KindBehavior {
			behavior = append(behavior, cases[i])
		}
	}
	return behavior
}

func RunBehavior(ctx context.Context, definition *auraagent.Definition, cases []Case, converse Converser, judge Judge) (BehaviorReport, error) {
	if definition.EvalBehavior == nil {
		return BehaviorReport{}, invalid("persona %q does not enable behavior evaluation", definition.ID)
	}
	if converse == nil || judge == nil {
		return BehaviorReport{}, invalid("behavior evaluation needs a converser and a judge")
	}
	behavior := BehaviorCases(cases)
	if len(behavior) == 0 {
		return BehaviorReport{}, invalid("persona %q has no behavior cases", definition.ID)
	}
	report := BehaviorReport{MinScore: definition.EvalBehavior.MinScore}
	total := 0.0
	for i := range behavior {
		c := &behavior[i]
		if !definition.Admits(c.Class) {
			return BehaviorReport{}, invalid("case %q uses class %q, which persona %q does not admit", c.Name, c.Class, definition.ID)
		}
		transcript, err := converse(ctx, definition, c.Class, c.Turns)
		if err != nil {
			return BehaviorReport{}, fmt.Errorf("behavior case %q: %w", c.Name, err)
		}
		raw, err := judge(ctx, JudgePrompt(c.Rubric, transcript))
		if err != nil {
			return BehaviorReport{}, fmt.Errorf("judge case %q: %w", c.Name, err)
		}
		scored := CaseScore{Name: c.Name, Reason: ReasonJudgeInvalid}
		if score, reason, ok := ParseJudgment(raw); ok {
			scored.Score, scored.Reason = score, reason
		}
		if scored.Score < report.MinScore {
			report.FailedCases = append(report.FailedCases, c.Name)
		}
		report.Cases = append(report.Cases, scored)
		total += scored.Score
	}
	report.Score = total / float64(len(report.Cases))
	return report, nil
}

func JudgePrompt(rubric string, transcript []Exchange) string {
	var prompt strings.Builder
	prompt.WriteString(judgeInstruction)
	prompt.WriteString("\n\nRubric:\n")
	prompt.WriteString(rubric)
	prompt.WriteString("\n\nTranscript:\n")
	for _, exchange := range transcript {
		prompt.WriteString("User: ")
		prompt.WriteString(exchange.User)
		prompt.WriteString("\nAssistant: ")
		prompt.WriteString(exchange.Assistant)
		prompt.WriteString("\n")
	}
	return prompt.String()
}

func ParseJudgment(raw string) (score float64, reason string, ok bool) {
	text := strings.TrimSpace(raw)
	text = strings.TrimPrefix(text, "```json")
	text = strings.TrimPrefix(text, "```")
	text = strings.TrimSuffix(strings.TrimSpace(text), "```")
	var judgment struct {
		Score  *float64 `json:"score"`
		Reason string   `json:"reason"`
	}
	decoder := json.NewDecoder(strings.NewReader(strings.TrimSpace(text)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&judgment); err != nil || judgment.Score == nil {
		return 0, "", false
	}
	if decoder.More() {
		return 0, "", false
	}
	value := *judgment.Score
	if math.IsNaN(value) || value < 0 || value > 1 {
		return 0, "", false
	}
	return value, strings.TrimSpace(judgment.Reason), true
}
