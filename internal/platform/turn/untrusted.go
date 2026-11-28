package turn

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strconv"
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

func QuoteUntrustedName(text string) string {
	return strconv.Quote(NeutralizeUntrustedMarkers(text))
}

func (r *TurnRequest) Tainted() bool {
	_, tainted := r.AdmissionTaint()
	return tainted
}

func authoredInputOrigin(origin Origin) bool {
	if origin == OriginTerminal || origin == OriginHTTP || origin == OriginSchedule || origin == OriginWorkflow {
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
