package configkit

import (
	"errors"
	"strings"

	"github.com/anggasct/aura/internal/platform/fault"
)

type ErrorCode string

const (
	ErrorCodeInvalid    ErrorCode = "config_invalid"
	ErrorCodeUnknownKey ErrorCode = "config_unknown_key"
	ErrorCodeMalformed  ErrorCode = "config_malformed"
)

type FieldError struct {
	Path     string
	Position string
	AtKey    bool
	Err      error
}

func (e *FieldError) Error() string {
	var b strings.Builder
	switch {
	case e.Path != "":
		b.WriteString(e.Path)
		if e.Position != "" {
			b.WriteString(" (" + e.Position + ")")
		}
		b.WriteString(": ")
	case e.Position != "":
		b.WriteString("(" + e.Position + ") ")
	}
	b.WriteString(e.Err.Error())
	return b.String()
}

func (e *FieldError) Unwrap() error {
	return e.Err
}

func Invalid(path, detail string) error {
	return Field(path, fault.New(ErrorCodeInvalid, detail))
}

func InvalidKey(path, detail string) error {
	return &FieldError{Path: path, AtKey: true, Err: fault.New(ErrorCodeInvalid, detail)}
}

func Field(path string, err error) error {
	if err == nil {
		return nil
	}
	return &FieldError{Path: path, Err: err}
}

func Under(prefix string, err error) error {
	if err == nil {
		return nil
	}
	return errors.Join(placeUnder(prefix, err)...)
}

func placeUnder(prefix string, err error) []error {
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		placed := make([]error, 0, len(joined.Unwrap()))
		for _, child := range joined.Unwrap() {
			placed = append(placed, placeUnder(prefix, child)...)
		}
		return placed
	}
	node, isField := errors.AsType[*FieldError](err)
	if !isField {
		return []error{Field(prefix, err)}
	}
	path := prefix
	switch {
	case strings.HasPrefix(node.Path, "["):
		path = prefix + node.Path
	case node.Path != "":
		path = prefix + "." + node.Path
	}
	return []error{&FieldError{Path: path, Position: node.Position, AtKey: node.AtKey, Err: node.Err}}
}
