package tools

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

const ErrorCodeDisplayRedacted ErrorCode = "approval_display_redacted"

const (
	ApprovalDisplayLimit = 1200
	MaxDisplayFields     = 16

	maxJSONDepth         = 10000
	truncatedMarker      = "...[truncated]"
	jsonUnicodeEscapeLen = len(`\u0000`)
	maxRawMarks          = 2
)

var displayNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]{0,63}$`)

var markupEscapes = map[string]byte{`\u003c`: '<', `\u003e`: '>', `\u0026`: '&'}

func (d *Definition) FieldApproval() bool {
	return d.Plugin != nil && d.Effectful
}

func (d *Definition) StandingGrantAllowed() bool {
	return !d.FieldApproval() && !d.NoStandingGrant
}

func (d *Definition) FieldDisplay() bool {
	return d.FieldApproval() || d.ExactDisplay || len(d.Display) > 0
}

func ValidateDisplayNames(display []string) error {
	if len(display) > MaxDisplayFields {
		return errorf(ErrorCodeDisplayInvalid, "display lists more than %d fields", MaxDisplayFields)
	}
	seen := make(map[string]bool, len(display))
	for _, name := range display {
		if !displayNamePattern.MatchString(name) {
			return errorf(ErrorCodeDisplayInvalid, "display entries must match [A-Za-z_][A-Za-z0-9_-]{0,63}")
		}
		if seen[name] {
			return errorf(ErrorCodeDisplayInvalid, "display lists an entry twice")
		}
		seen[name] = true
	}
	return nil
}

func CheckDisplaySchema(display []string, schema json.RawMessage) error {
	if len(display) == 0 {
		return nil
	}
	if err := ValidateDisplayNames(display); err != nil {
		return err
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(schema, &root); err != nil || root == nil {
		return errorf(ErrorCodeDisplayInvalid, "the input schema is not an object")
	}
	if !bytes.Equal(bytes.TrimSpace(root["additionalProperties"]), []byte("false")) {
		return errorf(ErrorCodeDisplayInvalid, "the input schema must set additionalProperties to false")
	}
	var properties map[string]json.RawMessage
	if err := json.Unmarshal(root["properties"], &properties); err != nil {
		return errorf(ErrorCodeDisplayInvalid, "the input schema declares no properties")
	}
	for _, name := range display {
		if _, declared := properties[name]; !declared {
			return errorf(ErrorCodeDisplayInvalid, "display names a field the input schema does not declare")
		}
	}
	return nil
}

func (d *Definition) ApprovalText(arguments json.RawMessage, redact func(string) string) (string, error) {
	if redact == nil {
		redact = func(text string) string { return text }
	}
	if d.FieldDisplay() {
		return d.fieldText(arguments, redact)
	}
	return jsonText(arguments, redact), nil
}

func jsonText(arguments json.RawMessage, redact func(string) string) string {
	text := strings.TrimSpace(string(arguments))
	var compact bytes.Buffer
	if json.Compact(&compact, []byte(text)) == nil {
		text = readableMarkup(compact.String())
	}
	if text == "" {
		text = "{}"
	}
	text = strings.ToValidUTF8(redact(text), "�")
	escaped, consumed := escapeWithin(text, ApprovalDisplayLimit, false)
	if consumed == len(text) {
		return escaped
	}
	escaped, _ = escapeWithin(text, ApprovalDisplayLimit-len(truncatedMarker), false)
	return escaped + truncatedMarker
}

func readableMarkup(compact string) string {
	var text strings.Builder
	text.Grow(len(compact))
	for i := 0; i < len(compact); {
		if compact[i] != '\\' || i+1 == len(compact) {
			text.WriteByte(compact[i])
			i++
			continue
		}
		if i+jsonUnicodeEscapeLen <= len(compact) {
			if char, ok := markupEscapes[strings.ToLower(compact[i:i+jsonUnicodeEscapeLen])]; ok {
				text.WriteByte(char)
				i += jsonUnicodeEscapeLen
				continue
			}
		}
		text.WriteString(compact[i : i+2])
		i += 2
	}
	return text.String()
}

func (d *Definition) fieldText(arguments json.RawMessage, redact func(string) string) (string, error) {
	var object map[string]json.RawMessage
	if len(bytes.TrimSpace(arguments)) == 0 {
		arguments = json.RawMessage("{}")
	}
	if err := json.Unmarshal(arguments, &object); err != nil || object == nil {
		return "", errorf(ErrorCodeDisplayInvalid, "the arguments are not a json object")
	}
	if len(object) == 0 {
		return "{}", nil
	}
	order, tail := displayOrder(object, d.Display)
	lines := make([]string, 0, len(order)+1)
	for _, name := range order {
		value, _, err := fieldValue(object[name], redact, d.ExactDisplay)
		if err != nil {
			return "", err
		}
		lines = append(lines, escapeText(redact(name))+": "+escapeText(value))
	}
	if tail == "" {
		return fitLines(lines)
	}
	value, isString, err := fieldValue(object[tail], redact, d.ExactDisplay)
	if err != nil {
		return "", err
	}
	label := escapeText(redact(tail)) + ": "
	full := append(slices.Clone(lines), label+escapeText(value))
	if joined := strings.Join(full, "\n"); len(joined) <= ApprovalDisplayLimit {
		return joined, nil
	}
	if !isString || d.ExactDisplay {
		return "", tooLarge()
	}
	return truncateTail(lines, label, value)
}

func truncateTail(lines []string, label, value string) (string, error) {
	used := len(label)
	if len(lines) > 0 {
		used += len(strings.Join(lines, "\n")) + 1
	}
	available := ApprovalDisplayLimit - used - len(hiddenMarker(len(value)))
	if available < 0 {
		return "", tooLarge()
	}
	escaped, consumed := escapeWithin(value, available, true)
	tail := label + escaped + hiddenMarker(len(value)-consumed)
	if len(lines) == 0 {
		return tail, nil
	}
	return strings.Join(lines, "\n") + "\n" + tail, nil
}

func hiddenMarker(hidden int) string {
	return fmt.Sprintf("...[%d bytes hidden]", hidden)
}

func fitLines(lines []string) (string, error) {
	joined := strings.Join(lines, "\n")
	if len(joined) > ApprovalDisplayLimit {
		return "", tooLarge()
	}
	return joined, nil
}

func tooLarge() error {
	return errorf(ErrorCodeDisplayTooLarge, "the arguments do not fit the %d byte approval display", ApprovalDisplayLimit)
}

func displayOrder(object map[string]json.RawMessage, display []string) (order []string, tail string) {
	if len(display) > 0 {
		if last := display[len(display)-1]; object[last] != nil {
			tail = last
		}
	}
	declared := make(map[string]bool, len(display))
	for _, name := range display {
		declared[name] = true
		if _, present := object[name]; present && name != tail {
			order = append(order, name)
		}
	}
	undeclared := make([]string, 0, len(object))
	for name := range object {
		if !declared[name] {
			undeclared = append(undeclared, name)
		}
	}
	slices.Sort(undeclared)
	return append(order, undeclared...), tail
}

func fieldValue(raw json.RawMessage, redact func(string) string, exact bool) (text string, isString bool, err error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) > 0 && trimmed[0] == '"' {
		var decoded string
		if err := json.Unmarshal(trimmed, &decoded); err != nil {
			return "", false, errorf(ErrorCodeDisplayInvalid, "a string argument cannot be decoded")
		}
		text, err = redactedValue(decoded, redact, exact)
		return text, true, err
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, trimmed); err != nil {
		return "", false, errorf(ErrorCodeDisplayInvalid, "an argument cannot be decoded")
	}
	text, err = redactedValue(compact.String(), redact, exact)
	return text, false, err
}

func redactedValue(value string, redact func(string) string, exact bool) (string, error) {
	shown := redact(value)
	if exact && shown != value {
		return "", errorf(ErrorCodeDisplayRedacted, "an argument holds a configured secret")
	}
	return strings.ToValidUTF8(shown, "�"), nil
}

func displayEscaped(r rune) bool {
	switch {
	case unicode.IsControl(r):
		return true
	case r == 0x061C, r == 0x200E, r == 0x200F, r == 0x2028, r == 0x2029:
		return true
	case r >= 0x202A && r <= 0x202E, r >= 0x2066 && r <= 0x2069:
		return true
	case unicode.In(r, unicode.Cf, unicode.Co, unicode.Cs, unicode.Variation_Selector, unicode.Other_Default_Ignorable_Code_Point):
		return true
	}
	return !unicode.In(r, unicode.L, unicode.M, unicode.N, unicode.P, unicode.S, unicode.Z)
}

type displayEscaper struct {
	field bool
	marks int
}

func (e *displayEscaper) escape(r rune) string {
	if unicode.Is(unicode.M, r) && !displayEscaped(r) {
		e.marks++
		if e.marks > maxRawMarks {
			return fmt.Sprintf(`\u{%04x}`, r)
		}
		return string(r)
	}
	e.marks = 0
	switch {
	case r == '\n':
		return `\n`
	case r == '\r':
		return `\r`
	case r == '\t':
		return `\t`
	case displayEscaped(r):
		return fmt.Sprintf(`\u{%04x}`, r)
	case e.field && r == '\\':
		return `\\`
	}
	return string(r)
}

func EscapeDisplayText(text string) string {
	return escapeText(text)
}

func escapeText(text string) string {
	escaper := displayEscaper{field: true}
	var out strings.Builder
	for _, r := range strings.ToValidUTF8(text, "�") {
		out.WriteString(escaper.escape(r))
	}
	return out.String()
}

func escapeWithin(text string, budget int, field bool) (escaped string, consumed int) {
	escaper := displayEscaper{field: field}
	var out strings.Builder
	for consumed < len(text) {
		r, size := utf8.DecodeRuneInString(text[consumed:])
		piece := escaper.escape(r)
		if out.Len()+len(piece) > budget {
			break
		}
		out.WriteString(piece)
		consumed += size
	}
	return out.String(), consumed
}

func rejectAmbiguousKeys(raw json.RawMessage) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	type frame struct {
		object    bool
		expectKey bool
		keys      map[string]bool
		folded    map[string]bool
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var stack []frame
	valueDone := func() {
		if n := len(stack); n > 0 && stack[n-1].object {
			stack[n-1].expectKey = true
		}
	}
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return errorf(ErrorCodeArgumentsAmbiguous, "the arguments are not valid json")
		}
		if delim, isDelim := token.(json.Delim); isDelim {
			switch delim {
			case '{', '[':
				if len(stack) >= maxJSONDepth {
					return errorf(ErrorCodeArgumentsAmbiguous, "the arguments are nested too deeply")
				}
				next := frame{object: delim == '{', expectKey: delim == '{'}
				if next.object {
					next.keys, next.folded = map[string]bool{}, map[string]bool{}
				}
				stack = append(stack, next)
			default:
				stack = stack[:len(stack)-1]
				valueDone()
			}
			continue
		}
		n := len(stack)
		if n == 0 || !stack[n-1].object || !stack[n-1].expectKey {
			valueDone()
			continue
		}
		key, _ := token.(string)
		top := &stack[n-1]
		if top.keys[key] {
			return errorf(ErrorCodeArgumentsAmbiguous, "the arguments repeat a key")
		}
		top.keys[key] = true
		if n == 1 {
			fold := strings.ToLower(strings.ToUpper(key))
			if top.folded[fold] {
				return errorf(ErrorCodeArgumentsAmbiguous, "the arguments repeat a key with different letter case")
			}
			top.folded[fold] = true
		}
		top.expectKey = false
	}
}
