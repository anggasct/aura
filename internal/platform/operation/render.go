package operation

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
)

const (
	tableRowsKey = "rows"
	textKey      = "text"
	nullCell     = "-"
)

func renderOutput(out io.Writer, descriptor *Descriptor, data json.RawMessage) error {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil {
		return wrapError(ErrorCodeFailed, "the result is not a JSON object", err)
	}
	fields := descriptor.OutputFieldsOn(SurfaceCLI)
	switch descriptor.Output.Kind {
	case OutputTable:
		return renderTable(out, object, fields)
	case OutputText:
		return renderText(out, object)
	case OutputRecord:
		return renderRecord(out, object, fields)
	}
	return renderRecord(out, object, fields)
}

func writeIndented(out io.Writer, data json.RawMessage) error {
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, data, "", "  "); err != nil {
		return wrapError(ErrorCodeFailed, "the result cannot be indented", err)
	}
	pretty.WriteByte('\n')
	_, err := out.Write(pretty.Bytes())
	return err
}

func cellText(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nullCell
	}
	var text string
	if json.Unmarshal(trimmed, &text) == nil {
		return strings.NewReplacer("\t", " ", "\n", " ", "\r", " ").Replace(text)
	}
	return string(trimmed)
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

func renderRecord(out io.Writer, object map[string]json.RawMessage, fields []string) error {
	for _, key := range orderedKeys(object, fields) {
		raw, present := object[key]
		if !present {
			continue
		}
		if _, err := fmt.Fprintf(out, "%s: %s\n", key, cellText(raw)); err != nil {
			return err
		}
	}
	return nil
}

func renderTable(out io.Writer, object map[string]json.RawMessage, fields []string) error {
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
				cells[i] = cellText(raw)
			}
		}
		if _, err := fmt.Fprintln(out, strings.Join(cells, "\t")); err != nil {
			return err
		}
	}
	return nil
}

func renderText(out io.Writer, object map[string]json.RawMessage) error {
	raw, present := object[textKey]
	if !present {
		return NewError(ErrorCodeFailed, "operation: a text result has no text")
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return wrapError(ErrorCodeFailed, "the text result is not a string", err)
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
		if _, err := fmt.Fprintln(out, line); err != nil {
			return err
		}
	}
	return nil
}
