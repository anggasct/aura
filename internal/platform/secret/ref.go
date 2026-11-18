package secret

import (
	"path/filepath"
	"regexp"
	"strings"
)

const (
	envScheme  = "env://"
	fileScheme = "file://"
)

var envNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

type Ref struct {
	text string
}

func ParseRef(text string) (Ref, error) {
	switch {
	case text == "":
		return Ref{}, nil
	case strings.HasPrefix(text, envScheme):
		if !envNamePattern.MatchString(strings.TrimPrefix(text, envScheme)) {
			return Ref{}, Errorf(ErrorCodeScopeViolation, "env:// secret reference must be a valid environment variable name")
		}
	case strings.HasPrefix(text, fileScheme):
		path := strings.TrimPrefix(text, fileScheme)
		if !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return Ref{}, Errorf(ErrorCodeScopeViolation, "file:// secret reference must be an absolute, clean path")
		}
	default:
		return Ref{}, Errorf(ErrorCodeScopeViolation, "secret reference must be empty or use the env:// or file:// scheme")
	}
	return Ref{text: text}, nil
}

func (r Ref) String() string {
	return r.text
}

func (r Ref) IsZero() bool {
	return r.text == ""
}

func (r Ref) Reference() Reference {
	if name, isEnv := strings.CutPrefix(r.text, envScheme); isEnv {
		return Reference{Env: name}
	}
	if path, isFile := strings.CutPrefix(r.text, fileScheme); isFile {
		return Reference{File: path}
	}
	return Reference{}
}

func (r Ref) MarshalText() ([]byte, error) {
	return []byte(r.text), nil
}

func (r *Ref) UnmarshalText(text []byte) error {
	parsed, err := ParseRef(string(text))
	if err != nil {
		return err
	}
	*r = parsed
	return nil
}
