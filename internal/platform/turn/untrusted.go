package turn

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"

	"github.com/anggasct/aura/internal/platform/principal"
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

func (r *TurnRequest) Tainted() bool {
	return r == nil || len(r.Knowledge) > 0 || r.UntrustedContext != nil || !authoredInputOrigin(r.Origin)
}

func authoredInputOrigin(origin Origin) bool {
	if origin == OriginTerminal || origin == OriginHTTP || origin == OriginSchedule {
		return true
	}
	channel, account, found := strings.Cut(string(origin), ":")
	if !found || account == "" {
		return false
	}
	switch channel {
	case principal.ChannelDiscord, principal.ChannelTelegram, principal.ChannelEmail:
		return true
	}
	return false
}
