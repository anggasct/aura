package turn

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
)

var untrustedMarker = regexp.MustCompile(`(?i)\[\s*/?\s*untrusted`)

func NeutralizeUntrustedMarkers(text string) string {
	return untrustedMarker.ReplaceAllString(text, "(untrusted")
}

func QuoteUntrusted(text string) string {
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(NeutralizeUntrustedMarkers(text)); err != nil {
		return `""`
	}
	return strings.TrimSuffix(encoded.String(), "\n")
}
