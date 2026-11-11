package toolbroker

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
)

const (
	redactionMarker = "[REDACTED]"
	redactedJSON    = `"` + redactionMarker + `"`
)

func redact(data []byte, secrets []string) []byte {
	return []byte(redactText(string(data), secretForms(secrets)))
}

func redactOutput(output json.RawMessage, secrets []string) json.RawMessage {
	if len(output) == 0 {
		return output
	}
	forms := secretForms(secrets)
	if !json.Valid(output) {
		return json.RawMessage(redactText(string(output), forms))
	}
	redacted := redactStrings(output, forms)
	if containsForm(redacted, forms) {
		return json.RawMessage(redactedJSON)
	}
	return redacted
}

func secretForms(secrets []string) []string {
	var forms []string
	for _, secret := range secrets {
		for _, form := range formsOf(secret) {
			if !slices.Contains(forms, form) {
				forms = append(forms, form)
			}
		}
	}
	return forms
}

func formsOf(secret string) []string {
	if secret == "" {
		return nil
	}
	forms := []string{secret}
	if minimal, err := minimalJSONBody(secret); err == nil {
		forms = append(forms, minimal, strings.ReplaceAll(minimal, "/", `\/`))
	}
	if goDefault, err := json.Marshal(secret); err == nil {
		forms = append(forms, string(goDefault[1:len(goDefault)-1]))
	}
	return forms
}

func minimalJSONBody(value string) (string, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return "", err
	}
	encoded := bytes.TrimSuffix(buffer.Bytes(), []byte("\n"))
	return string(encoded[1 : len(encoded)-1]), nil
}

func redactText(text string, forms []string) string {
	covered := coveredBytes(text, forms)
	if covered == nil {
		return text
	}
	var redacted strings.Builder
	for i := range len(text) {
		if !covered[i] {
			redacted.WriteByte(text[i])
		} else if i == 0 || !covered[i-1] {
			redacted.WriteString(redactionMarker)
		}
	}
	return redacted.String()
}

func coveredBytes(text string, forms []string) []bool {
	var covered []bool
	for _, form := range forms {
		for from := 0; ; {
			index := strings.Index(text[from:], form)
			if index < 0 {
				break
			}
			if covered == nil {
				covered = make([]bool, len(text))
			}
			start := from + index
			for k := range len(form) {
				covered[start+k] = true
			}
			from = start + 1
		}
	}
	return covered
}

func redactStrings(output []byte, forms []string) []byte {
	var redacted []byte
	copied, position := 0, 0
	for {
		index := bytes.IndexByte(output[position:], '"')
		if index < 0 {
			break
		}
		start := position + index
		end := stringTokenEnd(output, start)
		if replacement, changed := redactToken(output[start:end], forms); changed {
			redacted = append(append(redacted, output[copied:start]...), replacement...)
			copied = end
		}
		position = end
	}
	if redacted == nil {
		return output
	}
	return append(redacted, output[copied:]...)
}

func stringTokenEnd(output []byte, start int) int {
	end := start + 1
	for end < len(output) && output[end] != '"' {
		if output[end] == '\\' {
			end++
		}
		end++
	}
	return min(end+1, len(output))
}

func redactToken(token []byte, forms []string) ([]byte, bool) {
	var value string
	if err := json.Unmarshal(token, &value); err != nil {
		return []byte(redactedJSON), true
	}
	redacted := redactText(value, forms)
	if redacted == value {
		return token, false
	}
	body, err := minimalJSONBody(redacted)
	if err != nil {
		return []byte(redactedJSON), true
	}
	return []byte(`"` + body + `"`), true
}

func containsForm(data []byte, forms []string) bool {
	for _, form := range forms {
		if bytes.Contains(data, []byte(form)) {
			return true
		}
	}
	return false
}
