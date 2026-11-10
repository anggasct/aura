package email

import (
	"bytes"
	"crypto/sha256"
	"encoding/base32"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/emersion/go-message"
	"github.com/emersion/go-message/mail"

	"github.com/anggasct/aura/internal/platform/principal"
)

const (
	maxMessageIDBytes = 998
	maxSubjectBytes   = 998
	digestChars       = 26
	maxReferenceIDs   = 20
	maxHeaderIDBytes  = 255
	headerIDSymbols   = "!#$%&'*+-/=?^_`{|}~.@"
)

var digestEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

func readHeader(raw []byte) (*mail.Header, bool) {
	entity, err := message.Read(bytes.NewReader(raw))
	if entity == nil {
		return nil, false
	}
	if err != nil && !message.IsUnknownCharset(err) && !message.IsUnknownEncoding(err) {
		return nil, false
	}
	return &mail.Header{Header: entity.Header}, true
}

func isAutomated(header *mail.Header) bool {
	for _, value := range header.Values("Auto-Submitted") {
		keyword, _, _ := strings.Cut(value, ";")
		if !strings.EqualFold(strings.TrimSpace(keyword), "no") {
			return true
		}
	}
	if header.Has("List-Id") {
		return true
	}
	for _, value := range header.Values("Precedence") {
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "bulk", "list", "junk":
			return true
		}
	}
	return false
}

func senderOf(header *mail.Header) (string, bool) {
	if header.FieldsByKey("From").Len() != 1 || hasGroupSyntax(header.Get("From")) {
		return "", false
	}
	addresses, err := header.AddressList("From")
	if err != nil || len(addresses) != 1 {
		return "", false
	}
	sender := strings.ToLower(addresses[0].Address)
	if !principal.IsEmailAddress(sender) {
		return "", false
	}
	return sender, true
}

func domainOf(address string) string {
	at := strings.LastIndexByte(address, '@')
	return address[at+1:]
}

func hasGroupSyntax(value string) bool {
	quoted := false
	escaped := false
	for _, r := range value {
		switch {
		case escaped:
			escaped = false
		case r == '\\':
			escaped = quoted
		case r == '"':
			quoted = !quoted
		case r == ':' && !quoted:
			return true
		}
	}
	return false
}

func messageIDOf(header *mail.Header) (string, bool) {
	raw := header.Get("Message-Id")
	if header.FieldsByKey("Message-Id").Len() != 1 || strings.Count(raw, "<") != 1 || strings.Count(raw, ">") != 1 {
		return "", false
	}
	id, err := header.MessageID()
	if err != nil || !plainIdentifier(id) {
		return "", false
	}
	return id, true
}

func plainIdentifier(id string) bool {
	if id == "" || len(id) > maxMessageIDBytes || !utf8.ValidString(id) {
		return false
	}
	return strings.IndexFunc(id, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) < 0
}

func threadRoot(header *mail.Header, messageID string) string {
	for _, key := range []string{"References", "In-Reply-To"} {
		ids, err := header.MsgIDList(key)
		if err == nil && len(ids) > 0 && plainIdentifier(ids[0]) {
			return ids[0]
		}
	}
	return messageID
}

func subjectOf(header *mail.Header) string {
	subject, err := header.Subject()
	if err != nil {
		subject = header.Get("Subject")
	}
	return truncateUTF8(singleLine(subject), maxSubjectBytes)
}

func singleLine(text string) string {
	return strings.Join(strings.FieldsFunc(strings.ToValidUTF8(text, ""), func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	}), " ")
}

func referenceIDs(header *mail.Header) []string {
	ids, err := header.MsgIDList("References")
	if err != nil {
		return nil
	}
	return boundedReferences(ids)
}

func boundedReferences(ids []string) []string {
	safe := make([]string, 0, len(ids))
	for _, id := range ids {
		if headerSafeID(id) {
			safe = append(safe, id)
		}
	}
	if len(safe) > maxReferenceIDs {
		safe = safe[len(safe)-maxReferenceIDs:]
	}
	return safe
}

func headerSafeID(id string) bool {
	if id == "" || len(id) > maxHeaderIDBytes {
		return false
	}
	for i := range len(id) {
		c := id[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && !strings.ContainsRune(headerIDSymbols, rune(c)) {
			return false
		}
	}
	return true
}

func digestOf(text string) string {
	sum := sha256.Sum256([]byte(text))
	return strings.ToLower(digestEncoding.EncodeToString(sum[:]))[:digestChars]
}

func truncateUTF8(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut]
}
