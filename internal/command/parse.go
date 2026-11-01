package command

import (
	"regexp"
	"strings"
)

var tokenPattern = regexp.MustCompile(`^/[A-Za-z][A-Za-z0-9-]{0,31}$`)

func Parse(text string) (Invocation, bool) {
	fields := strings.Fields(text)
	if len(fields) == 0 || !tokenPattern.MatchString(fields[0]) {
		return Invocation{}, false
	}
	return Invocation{
		Name:  strings.ToLower(fields[0][1:]),
		Token: fields[0],
		Args:  fields[1:],
		Text:  text,
	}, true
}
