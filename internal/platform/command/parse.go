package command

import (
	"regexp"
	"strings"
	"unicode"
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

const (
	MaxCommandBytes = 16 << 10
	MaxTokens       = 64
)

func Tokenize(text string) ([]string, error) {
	if len(text) > MaxCommandBytes {
		return nil, Errorf(ErrorCodeTooLong, "the command is too long")
	}
	var (
		tokens  []string
		current strings.Builder
		started bool
		quote   rune
		escaped bool
	)
	flush := func() error {
		if !started {
			return nil
		}
		if len(tokens) == MaxTokens {
			return Errorf(ErrorCodeTooManyArguments, "the command has too many arguments")
		}
		tokens = append(tokens, current.String())
		current.Reset()
		started = false
		return nil
	}
	for _, r := range text {
		switch {
		case escaped:
			current.WriteRune(r)
			escaped = false
		case r == '\\':
			escaped, started = true, true
		case quote != 0 && r == quote:
			quote = 0
		case quote != 0:
			current.WriteRune(r)
		case r == '"' || r == '\'':
			quote, started = r, true
		case unicode.IsSpace(r):
			if err := flush(); err != nil {
				return nil, err
			}
		default:
			current.WriteRune(r)
			started = true
		}
	}
	switch {
	case escaped:
		return nil, Errorf(ErrorCodeDanglingEscape, "the command ends with a backslash")
	case quote != 0:
		return nil, Errorf(ErrorCodeOpenQuote, "a quote is not closed")
	}
	if err := flush(); err != nil {
		return nil, err
	}
	return tokens, nil
}
