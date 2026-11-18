package config

import (
	"cmp"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/anggasct/aura/internal/platform/configkit"
	"github.com/anggasct/aura/internal/platform/fault"
)

const (
	rankFile = iota
	rankEnvironment
	rankUnpositioned
)

var rootTypeName = reflect.TypeFor[Config]().String()

type decodeFailure interface {
	error
	Name() string
	Unwrap() error
}

type problemOrder struct {
	rank        int
	line        int
	column      int
	environment string
	path        string
}

func decodeProblems(doc *document, err error) []error {
	if err == nil {
		return nil
	}
	if failure, isFailure := errors.AsType[decodeFailure](err); isFailure && errors.Is(failure, err) {
		return []error{decodeProblem(doc, failure)}
	}
	if joined, isJoined := err.(interface{ Unwrap() []error }); isJoined {
		var problems []error
		for _, child := range joined.Unwrap() {
			problems = append(problems, decodeProblems(doc, child)...)
		}
		return problems
	}
	if inner := errors.Unwrap(err); inner != nil {
		return decodeProblems(doc, inner)
	}
	return []error{configkit.Invalid("", err.Error())}
}

func decodeProblem(doc *document, failure decodeFailure) error {
	path := failure.Name()
	if path == rootTypeName {
		path = ""
	}
	return &configkit.FieldError{
		Path:     path,
		Position: doc.positionOf(path),
		Err:      fault.New(configkit.ErrorCodeInvalid, failure.Unwrap().Error()),
	}
}

func sortProblems(problems []error) {
	slices.SortStableFunc(problems, func(a, b error) int {
		left, right := orderOf(a), orderOf(b)
		return cmp.Or(
			cmp.Compare(left.rank, right.rank),
			cmp.Compare(left.line, right.line),
			cmp.Compare(left.column, right.column),
			strings.Compare(left.environment, right.environment),
			strings.Compare(left.path, right.path),
		)
	})
}

func orderOf(err error) problemOrder {
	field, isField := errors.AsType[*configkit.FieldError](err)
	if !isField {
		return problemOrder{rank: rankUnpositioned}
	}
	order := problemOrder{rank: rankUnpositioned, path: field.Path}
	if name, isEnvironment := strings.CutPrefix(field.Position, "env "); isEnvironment {
		order.rank = rankEnvironment
		order.environment = name
		return order
	}
	if scanned, _ := fmt.Sscanf(field.Position, "line %d, column %d", &order.line, &order.column); scanned == 2 {
		order.rank = rankFile
	}
	return order
}

func positionProblems(doc *document, err error) error {
	if err == nil {
		return nil
	}
	problems := flattenProblems(err)
	for i, problem := range problems {
		field, isField := errors.AsType[*configkit.FieldError](problem)
		if !isField || field.Position != "" {
			continue
		}
		positioned := *field
		positioned.Position = doc.positionOf(field.Path)
		problems[i] = &positioned
	}
	sortProblems(problems)
	return errors.Join(problems...)
}

func flattenProblems(err error) []error {
	if joined, isJoined := err.(interface{ Unwrap() []error }); isJoined {
		children := joined.Unwrap()
		problems := make([]error, 0, len(children))
		for _, child := range children {
			problems = append(problems, flattenProblems(child)...)
		}
		return problems
	}
	return []error{err}
}
