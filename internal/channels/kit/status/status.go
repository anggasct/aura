package status

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/anggasct/aura/internal/platform/secret"
	runtimechannelhost "github.com/anggasct/aura/internal/platform/turn/channelhost"
)

const (
	MaxSteps          = 3
	MaxActivityRunes  = 60
	StillWorkingAfter = time.Minute

	MarkerRunning = "⏳"
	MarkerDone    = "✓"
	MarkerFailed  = "✗"
	MarkerElapsed = "⏱"

	defaultWorkingText      = "Working…"
	defaultStillWorkingText = "Still working…"
)

var secretKeywords = []string{"password", "secret", "token", "api_key", "apikey", "bearer", "authorization", "private_key", "client_secret"}

var markdownMarkerReplacer = strings.NewReplacer("`", "", "*", "", "_", "", "~", "")

type Texts struct {
	Working      string
	StillWorking string
}

func DefaultTexts() Texts {
	return Texts{
		Working:      defaultWorkingText,
		StillWorking: defaultStillWorkingText,
	}
}

type Step struct {
	Activity string
	Outcome  runtimechannelhost.StepOutcome
}

type TrackedStep struct {
	CallIDs []string
	Step    Step
}

func ApplyUpdate(steps []TrackedStep, update runtimechannelhost.PresenceUpdate) []TrackedStep {
	outcome := cmp.Or(update.Outcome, runtimechannelhost.StepRunning)
	if len(update.CallIDs) > 0 {
		for i := len(steps) - 1; i >= 0; i-- {
			if hasSharedCallID(steps[i].CallIDs, update.CallIDs) {
				steps[i].Step.Outcome = outcome
				if update.Activity != "" {
					steps[i].Step.Activity = update.Activity
				}
				return steps
			}
		}
	}
	return append(steps, TrackedStep{
		CallIDs: slices.Clone(update.CallIDs),
		Step: Step{
			Activity: update.Activity,
			Outcome:  outcome,
		},
	})
}

func StepsOf(tracked []TrackedStep) []Step {
	if len(tracked) == 0 {
		return nil
	}
	out := make([]Step, len(tracked))
	for i := range tracked {
		out[i] = tracked[i].Step
	}
	return out
}

func hasSharedCallID(a, b []string) bool {
	for _, id := range b {
		if id != "" && slices.Contains(a, id) {
			return true
		}
	}
	return false
}

func TruncateRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n])
}

func ContainsSecretKeyword(s string) bool {
	lowered := strings.ToLower(s)
	for _, keyword := range secretKeywords {
		if strings.Contains(lowered, keyword) {
			return true
		}
	}
	return false
}

func FormatElapsed(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	total := int(d / time.Second)
	if total >= 3600 {
		hours := total / 3600
		minutes := (total % 3600) / 60
		seconds := total % 60
		return fmt.Sprintf("%d:%02d:%02d", hours, minutes, seconds)
	}
	minutes := total / 60
	seconds := total % 60
	return fmt.Sprintf("%d:%02d", minutes, seconds)
}

func Render(steps []Step, elapsed time.Duration, texts Texts, vault *secret.Vault) string {
	working := cmp.Or(strings.TrimSpace(texts.Working), defaultWorkingText)
	stillWorking := cmp.Or(strings.TrimSpace(texts.StillWorking), defaultStillWorkingText)
	if elapsed < 0 {
		elapsed = 0
	}
	elapsedLine := MarkerElapsed + " " + FormatElapsed(elapsed)
	if len(steps) == 0 {
		head := working
		if elapsed >= StillWorkingAfter {
			head = stillWorking
		}
		return head + "\n" + elapsedLine
	}
	if len(steps) > MaxSteps {
		steps = steps[len(steps)-MaxSteps:]
	}
	lines := make([]string, 0, len(steps)+1)
	for _, step := range steps {
		lines = append(lines, stepMarker(step.Outcome)+" "+formatActivity(step.Activity, working, vault))
	}
	lines = append(lines, elapsedLine)
	return strings.Join(lines, "\n")
}

func stepMarker(outcome runtimechannelhost.StepOutcome) string {
	switch outcome {
	case runtimechannelhost.StepDone:
		return MarkerDone
	case runtimechannelhost.StepFailed:
		return MarkerFailed
	case runtimechannelhost.StepRunning:
		return MarkerRunning
	default:
		return MarkerRunning
	}
}

func formatActivity(raw, fallback string, vault *secret.Vault) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return fallback
	}
	line, _, _ := strings.Cut(trimmed, "\n")
	line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
	if line == "" {
		return fallback
	}
	if isScreenedSecret(line, vault) {
		return fallback
	}
	cleaned := markdownMarkerReplacer.Replace(line)
	cleaned = strings.Join(strings.Fields(cleaned), " ")
	if cleaned == "" {
		return fallback
	}
	if isScreenedSecret(cleaned, vault) {
		return fallback
	}
	if vault != nil {
		cleaned = vault.Redact(cleaned)
	}
	return truncateWordSafe(cleaned, MaxActivityRunes)
}

func isScreenedSecret(s string, vault *secret.Vault) bool {
	if vault != nil && vault.Contains(s) {
		return true
	}
	return ContainsSecretKeyword(s)
}

func truncateWordSafe(s string, maxRunes int) string {
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	if maxRunes <= 1 {
		return "…"
	}
	budget := maxRunes - 1
	cut := -1
	for i := budget; i >= 0; i-- {
		if unicode.IsSpace(runes[i]) {
			cut = i
			break
		}
	}
	if cut <= 0 {
		return string(runes[:budget]) + "…"
	}
	return strings.TrimRightFunc(string(runes[:cut]), unicode.IsSpace) + "…"
}
