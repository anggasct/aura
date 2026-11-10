package connector

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"time"
)

const (
	messageIDDomain = "aura.invalid"
	bodyLineWidth   = 76
)

type outgoing struct {
	to         []string
	cc         []string
	subject    string
	body       string
	messageID  string
	inReplyTo  string
	references []string
}

func deriveMessageID(idempotencyKey string) string {
	digest := sha256.Sum256([]byte("aura-gmail-message-id\x00" + idempotencyKey))
	return "<" + hex.EncodeToString(digest[:16]) + "@" + messageIDDomain + ">"
}

func (m *outgoing) encode() ([]byte, error) {
	if !m.headersValid() {
		return nil, fail(CodeInvalidArgument)
	}
	var out strings.Builder
	out.WriteString(foldList("To", m.to, ",") + crlf)
	if len(m.cc) > 0 {
		out.WriteString(foldList("Cc", m.cc, ",") + crlf)
	}
	out.WriteString(subjectHeader(m.subject) + crlf)
	out.WriteString("Date: " + time.Now().UTC().Format(time.RFC1123Z) + crlf)
	out.WriteString("Message-ID: " + m.messageID + crlf)
	if m.inReplyTo != "" {
		out.WriteString("In-Reply-To: " + m.inReplyTo + crlf)
		out.WriteString(foldList("References", m.references, "") + crlf)
	}
	out.WriteString("MIME-Version: 1.0" + crlf)
	out.WriteString("Content-Type: text/plain; charset=UTF-8" + crlf)
	out.WriteString("Content-Transfer-Encoding: base64" + crlf + crlf)
	out.WriteString(wrapBase64(normalizeNewlines(m.body)))
	return []byte(out.String()), nil
}

func (m *outgoing) headersValid() bool {
	if len(m.to) == 0 || len(m.to)+len(m.cc) > maxRecipients || !validSubject(m.subject) || !validMessageID(m.messageID) {
		return false
	}
	for _, address := range append(append([]string(nil), m.to...), m.cc...) {
		if !validAddress(address) {
			return false
		}
	}
	if (m.inReplyTo == "") != (len(m.references) == 0) {
		return false
	}
	if m.inReplyTo != "" && !validMessageID(m.inReplyTo) {
		return false
	}
	for _, reference := range m.references {
		if !validMessageID(reference) {
			return false
		}
	}
	return true
}

func normalizeNewlines(text string) string {
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	return strings.ReplaceAll(text, "\n", crlf)
}

func wrapBase64(text string) string {
	encoded := base64.StdEncoding.EncodeToString([]byte(text))
	var out strings.Builder
	for len(encoded) > bodyLineWidth {
		out.WriteString(encoded[:bodyLineWidth] + crlf)
		encoded = encoded[bodyLineWidth:]
	}
	out.WriteString(encoded + crlf)
	return out.String()
}
