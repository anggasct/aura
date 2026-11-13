package workflow

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/anggasct/aura/internal/platform/tools"
)

const (
	maxGoalRunes      = 200
	maxEventRunes     = 128
	maxConditionRunes = 256
)

var idPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

func EscapeText(text string) string {
	return tools.EscapeDisplayText(text)
}

func ValidID(id string) bool {
	return idPattern.MatchString(id)
}

func checkID(field, id string) error {
	if !ValidID(id) {
		return codedError(ErrorCodeSpecInvalid, fmt.Sprintf("%s %q must match [a-z][a-z0-9_-]{0,63}", field, tools.EscapeDisplayText(id)))
	}
	return nil
}

func checkDisplaySafe(field, text string) error {
	if !tools.DisplaySafe(text) {
		return codedError(ErrorCodeSpecInvalid, field+" holds a control, formatting or invalid character")
	}
	return nil
}

func checkDisplayText(field, text string, maxRunes int) error {
	if err := checkDisplaySafe(field, text); err != nil {
		return err
	}
	if utf8.RuneCountInString(text) > maxRunes {
		return codedError(ErrorCodeSpecInvalid, fmt.Sprintf("%s is longer than %d characters", field, maxRunes))
	}
	return nil
}

func checkGoal(goal string) error {
	if goal == "" {
		return codedError(ErrorCodeSpecInvalid, "goal must not be empty")
	}
	if err := checkDisplayText("goal", goal, maxGoalRunes); err != nil {
		return err
	}
	if strings.HasPrefix(goal, " ") || strings.HasSuffix(goal, " ") || strings.Contains(goal, "  ") {
		return codedError(ErrorCodeSpecInvalid, "goal must not start or end with a space or repeat one")
	}
	for _, r := range goal {
		if r != ' ' && (unicode.IsSpace(r) || unicode.Is(unicode.Z, r)) {
			return codedError(ErrorCodeSpecInvalid, "goal may use only the plain space character")
		}
	}
	return nil
}
