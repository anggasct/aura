package operation

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"unicode/utf16"

	"github.com/anggasct/aura/internal/platform/tools"
)

const (
	tableRowsKey = "rows"
	textKey      = "text"
	nullCell     = "-"
)

func (d *Descriptor) renderOn(surface Surface, out io.Writer, data json.RawMessage) error {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil {
		return wrapError(ErrorCodeFailed, "the result is not a JSON object", err)
	}
	fields := d.OutputFieldsOn(surface)
	escape := d.UntrustedOutput && surface != SurfaceAgent
	switch d.Output.Kind {
	case OutputTable:
		if err := renderRecord(out, object, d.OutputHeaderOn(surface), escape); err != nil {
			return err
		}
		return renderTable(out, object, fields, escape)
	case OutputText:
		return renderText(out, object, escape)
	case OutputRecord:
		return renderRecord(out, object, fields, escape)
	}
	return renderRecord(out, object, fields, escape)
}

func displayText(text string, escape bool) string {
	if !escape {
		return text
	}
	return tools.EscapeDisplayText(text)
}

func jsonDisplaySafe(data json.RawMessage) json.RawMessage {
	var safe bytes.Buffer
	for _, r := range string(data) {
		if r == '\t' || r == '\n' || r == '\r' || tools.DisplaySafe(string(r)) {
			safe.WriteRune(r)
			continue
		}
		for _, unit := range utf16.Encode([]rune{r}) {
			fmt.Fprintf(&safe, `\u%04x`, unit)
		}
	}
	return safe.Bytes()
}

func writeIndented(out io.Writer, data json.RawMessage) error {
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, jsonDisplaySafe(data), "", "  "); err != nil {
		return wrapError(ErrorCodeFailed, "the result cannot be indented", err)
	}
	pretty.WriteByte('\n')
	_, err := out.Write(pretty.Bytes())
	return err
}

func cellText(raw json.RawMessage, escape bool) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nullCell
	}
	var text string
	if json.Unmarshal(trimmed, &text) == nil {
		if escape {
			return displayText(text, true)
		}
		return strings.NewReplacer("\t", " ", "\n", " ", "\r", " ").Replace(text)
	}
	return displayText(string(trimmed), escape)
}

func orderedKeys(object map[string]json.RawMessage, fields []string) []string {
	if len(fields) > 0 {
		return fields
	}
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

func renderRecord(out io.Writer, object map[string]json.RawMessage, fields []string, escape bool) error {
	for _, key := range orderedKeys(object, fields) {
		raw, present := object[key]
		if !present {
			continue
		}
		if items, isList := listItems(raw); isList {
			if err := renderList(out, key, items, escape); err != nil {
				return err
			}
			continue
		}
		if _, err := fmt.Fprintf(out, "%s: %s\n", key, cellText(raw, escape)); err != nil {
			return err
		}
	}
	return nil
}

func listItems(raw json.RawMessage) ([]json.RawMessage, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return nil, false
	}
	var items []json.RawMessage
	if err := json.Unmarshal(trimmed, &items); err != nil {
		return nil, false
	}
	return items, true
}

func renderList(out io.Writer, key string, items []json.RawMessage, escape bool) error {
	if len(items) == 0 {
		_, err := fmt.Fprintf(out, "%s: %s\n", key, nullCell)
		return err
	}
	if _, err := fmt.Fprintf(out, "%s:\n", key); err != nil {
		return err
	}
	for _, item := range items {
		if _, err := fmt.Fprintf(out, "  - %s\n", cellText(item, escape)); err != nil {
			return err
		}
	}
	return nil
}

func renderTable(out io.Writer, object map[string]json.RawMessage, fields []string, escape bool) error {
	var rows []map[string]json.RawMessage
	if raw, present := object[tableRowsKey]; present {
		if err := json.Unmarshal(raw, &rows); err != nil {
			return wrapError(ErrorCodeFailed, "the table rows are not objects", err)
		}
	}
	if len(fields) == 0 && len(rows) > 0 {
		fields = orderedKeys(rows[0], nil)
	}
	header := make([]string, len(fields))
	for i, field := range fields {
		header[i] = strings.ToUpper(field)
	}
	if _, err := fmt.Fprintln(out, strings.Join(header, "\t")); err != nil {
		return err
	}
	for _, row := range rows {
		cells := make([]string, len(fields))
		for i, field := range fields {
			if raw, present := row[field]; present {
				cells[i] = cellText(raw, escape)
			}
		}
		if _, err := fmt.Fprintln(out, strings.Join(cells, "\t")); err != nil {
			return err
		}
	}
	return nil
}

func renderText(out io.Writer, object map[string]json.RawMessage, escape bool) error {
	raw, present := object[textKey]
	if !present {
		return NewError(ErrorCodeFailed, "operation: a text result has no text")
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return wrapError(ErrorCodeFailed, "the text result is not a string", err)
	}
	if escape {
		lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
		for i, line := range lines {
			lines[i] = displayText(line, true)
		}
		text = strings.Join(lines, "\n")
	}
	if !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	_, err := io.WriteString(out, text)
	return err
}

func renderPreview(out io.Writer, preview *Preview, asJSON bool) error {
	if asJSON {
		lines := preview.Lines
		if lines == nil {
			lines = []string{}
		}
		encoded, err := encodeJSON(map[string]any{"lines": lines, "empty": preview.Empty})
		if err != nil {
			return err
		}
		return writeIndented(out, encoded)
	}
	for _, line := range preview.Lines {
		if _, err := fmt.Fprintln(out, displayText(line, true)); err != nil {
			return err
		}
	}
	return nil
}
