package workflow

import (
	"errors"
	"fmt"
	"strings"
)

type ErrorCode string

const (
	ErrorCodeSpecInvalid        ErrorCode = "workflow_spec_invalid"
	ErrorCodeDuplicateStep      ErrorCode = "workflow_duplicate_step"
	ErrorCodeUnknownDependency  ErrorCode = "workflow_unknown_dependency"
	ErrorCodeCycleDetected      ErrorCode = "workflow_cycle_detected"
	ErrorCodeExecutorInvalid    ErrorCode = "workflow_executor_invalid"
	ErrorCodeConditionInvalid   ErrorCode = "workflow_condition_invalid"
	ErrorCodeDangerousUncovered ErrorCode = "workflow_dangerous_operation_uncovered"
	ErrorCodeRunNotFound        ErrorCode = "workflow_run_not_found"
	ErrorCodeDefinitionNotFound ErrorCode = "workflow_definition_not_found"
	ErrorCodeStepTimeout        ErrorCode = "workflow_step_timeout"
	ErrorCodeBudgetExhausted    ErrorCode = "workflow_budget_exhausted"
	ErrorCodeConcurrencyBounded ErrorCode = "workflow_concurrency_bounded"
)

type Error struct {
	Code   ErrorCode
	Detail string
}

func (e *Error) Error() string {
	if e.Detail == "" {
		return string(e.Code)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Detail)
}

func codedError(code ErrorCode, detail string) *Error {
	return &Error{Code: code, Detail: detail}
}

func CodeOf(err error) (ErrorCode, bool) {
	var coded *Error
	if errors.As(err, &coded) {
		return coded.Code, true
	}
	return "", false
}

// operand is one side of a condition comparison.
type operand struct {
	// ref is set for steps.<id>.status and steps.<id>.output.<path> forms.
	ref  *refOperand
	text string
	// num and flag carry integer and boolean literals; nilText marks null.
	num     *int64
	flag    *bool
	nilText bool
}

type refOperand struct {
	stepID  string
	field   string // "status" or "output"
	jsonKey []string
	index   []int
}

// comparison is one equality or inequality test.
type comparison struct {
	left  operand
	op    string
	right operand
}

// condition is a conjunction of comparisons.
type condition struct {
	comparisons []comparison
}

// parseCondition parses the frozen grammar:
//
//	condition   := comparison { "&&" comparison }
//	comparison  := operand op operand
//	op          := "==" | "!="
//	operand     := status_ref | output_ref | string | integer | "true" | "false" | "null"
func parseCondition(source string) (*condition, error) {
	tokens, err := lexCondition(source)
	if err != nil {
		return nil, err
	}
	parsed := &condition{}
	for {
		comparisonToken, rest, err := parseComparison(tokens)
		if err != nil {
			return nil, err
		}
		parsed.comparisons = append(parsed.comparisons, *comparisonToken)
		tokens = rest
		if len(tokens) == 0 {
			return parsed, nil
		}
		if tokens[0] != "&&" {
			return nil, codedError(ErrorCodeConditionInvalid, fmt.Sprintf("unexpected token %q; only && joins comparisons", tokens[0]))
		}
		tokens = tokens[1:]
		if len(tokens) == 0 {
			return nil, codedError(ErrorCodeConditionInvalid, "dangling && at end of condition")
		}
	}
}

// lexCondition splits the source into tokens: operators, &&, quoted strings,
// bracketed indices, and bare words.
func lexCondition(source string) ([]string, error) {
	var tokens []string
	var current strings.Builder
	flush := func() {
		if current.Len() > 0 {
			tokens = append(tokens, current.String())
			current.Reset()
		}
	}
	for i := 0; i < len(source); {
		c := source[i]
		switch {
		case c == ' ' || c == '\t':
			flush()
			i++
		case c == '&' && i+1 < len(source) && source[i+1] == '&':
			flush()
			tokens = append(tokens, "&&")
			i += 2
		case c == '=' && i+1 < len(source) && source[i+1] == '=':
			flush()
			tokens = append(tokens, "==")
			i += 2
		case c == '!' && i+1 < len(source) && source[i+1] == '=':
			flush()
			tokens = append(tokens, "!=")
			i += 2
		case c == '!':
			return nil, codedError(ErrorCodeConditionInvalid, "lone '!' is not part of the grammar")
		case c == '"':
			flush()
			j := i + 1
			var literal strings.Builder
			closed := false
			for j < len(source) {
				if source[j] == '\\' {
					if j+1 < len(source) && source[j+1] == '"' {
						literal.WriteByte('"')
						j += 2
						continue
					}
					return nil, codedError(ErrorCodeConditionInvalid, "only \\\" escapes are allowed in string literals")
				}
				if source[j] == '"' {
					closed = true
					j++
					break
				}
				literal.WriteByte(source[j])
				j++
			}
			if !closed {
				return nil, codedError(ErrorCodeConditionInvalid, "unterminated string literal")
			}
			tokens = append(tokens, "\""+literal.String()+"\"")
			i = j
		default:
			current.WriteByte(c)
			i++
		}
	}
	flush()
	if len(tokens) == 0 {
		return nil, codedError(ErrorCodeConditionInvalid, "empty condition")
	}
	return tokens, nil
}

func parseComparison(tokens []string) (*comparison, []string, error) {
	if len(tokens) < 3 {
		return nil, nil, codedError(ErrorCodeConditionInvalid, "condition needs operand operator operand")
	}
	if tokens[1] != "==" && tokens[1] != "!=" {
		return nil, nil, codedError(ErrorCodeConditionInvalid, fmt.Sprintf("operator %q is not == or ==", tokens[1]))
	}
	left, err := parseOperand(tokens[0])
	if err != nil {
		return nil, nil, err
	}
	right, err := parseOperand(tokens[2])
	if err != nil {
		return nil, nil, err
	}
	return &comparison{left: left, op: tokens[1], right: right}, tokens[3:], nil
}

func parseOperand(token string) (operand, error) {
	switch {
	case strings.HasPrefix(token, "\"") && strings.HasSuffix(token, "\"") && len(token) >= 2:
		return operand{text: token[1 : len(token)-1]}, nil
	case token == "true" || token == "false":
		flag := token == "true"
		return operand{flag: &flag}, nil
	case token == "null":
		return operand{nilText: true}, nil
	case strings.HasPrefix(token, "steps."):
		return parseRef(token)
	default:
		if value, ok := parseInteger(token); ok {
			return operand{num: &value}, nil
		}
		return operand{}, codedError(ErrorCodeConditionInvalid, fmt.Sprintf("operand %q is not a reference or literal", token))
	}
}

func parseRef(token string) (operand, error) {
	rest := strings.TrimPrefix(token, "steps.")
	parts := strings.Split(rest, ".")
	if len(parts) < 2 {
		return operand{}, codedError(ErrorCodeConditionInvalid, fmt.Sprintf("reference %q must be steps.<id>.status or steps.<id>.output.<path>", token))
	}
	stepID := parts[0]
	field := parts[1]
	if field != "status" && field != "output" {
		return operand{}, codedError(ErrorCodeConditionInvalid, fmt.Sprintf("reference field %q must be status or output", field))
	}
	ref := &refOperand{stepID: stepID, field: field}
	if field == "status" {
		if len(parts) != 2 {
			return operand{}, codedError(ErrorCodeConditionInvalid, "a status reference takes no further path")
		}
		return operand{ref: ref}, nil
	}
	if len(parts) < 3 {
		return operand{}, codedError(ErrorCodeConditionInvalid, "an output reference needs at least one object key")
	}
	for _, segment := range parts[2:] {
		key, index, err := parsePathSegment(segment)
		if err != nil {
			return operand{}, err
		}
		ref.jsonKey = append(ref.jsonKey, key)
		if index != nil {
			ref.index = append(ref.index, int(*index))
		}
	}
	return operand{ref: ref}, nil
}

// parsePathSegment accepts "key" and "key[n]" forms; the index stays
// attached to its key.
func parsePathSegment(segment string) (key string, index *int64, err error) {
	open := strings.IndexByte(segment, '[')
	if open < 0 {
		if segment == "" {
			return "", nil, codedError(ErrorCodeConditionInvalid, "empty output path segment")
		}
		return segment, nil, nil
	}
	if !strings.HasSuffix(segment, "]") {
		return "", nil, codedError(ErrorCodeConditionInvalid, fmt.Sprintf("path segment %q must close its array index", segment))
	}
	key = segment[:open]
	if key == "" {
		return "", nil, codedError(ErrorCodeConditionInvalid, "array index needs a preceding key")
	}
	digits := segment[open+1 : len(segment)-1]
	value, ok := parseInteger(digits)
	if !ok || value < 0 {
		return "", nil, codedError(ErrorCodeConditionInvalid, fmt.Sprintf("array index %q must be a non-negative integer", digits))
	}
	return key, &value, nil
}

func parseInteger(token string) (int64, bool) {
	if token == "" || (token[0] != '-' && (token[0] < '0' || token[0] > '9')) {
		return 0, false
	}
	var value int64
	negate := token[0] == '-'
	digits := token
	if negate {
		digits = token[1:]
	}
	if digits == "" {
		return 0, false
	}
	for i := range len(digits) {
		if digits[i] < '0' || digits[i] > '9' {
			return 0, false
		}
		value = value*10 + int64(digits[i]-'0')
	}
	if negate {
		value = -value
	}
	return value, true
}

// referencedSteps returns every step id a condition references.
func (c *condition) referencedSteps() []string {
	var ids []string
	for _, cmp := range c.comparisons {
		for _, side := range []operand{cmp.left, cmp.right} {
			if side.ref != nil {
				ids = append(ids, side.ref.stepID)
			}
		}
	}
	return ids
}
