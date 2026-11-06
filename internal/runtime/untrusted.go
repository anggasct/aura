package runtime

import "regexp"

var untrustedMarker = regexp.MustCompile(`(?i)\[\s*/?\s*untrusted`)

func NeutralizeUntrustedMarkers(text string) string {
	return untrustedMarker.ReplaceAllString(text, "(untrusted")
}
