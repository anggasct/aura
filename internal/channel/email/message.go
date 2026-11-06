package email

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	mimeheader "mime"
	"mime/quotedprintable"
	"strings"
	"time"
	"unicode"

	"github.com/anggasct/aura/internal/principal"
)

const (
	MaxReplyBodyBytes     = 256 << 10
	maxReplySubjectBytes  = 160
	replyTruncationNotice = "\n\n[reply truncated]"
	plainLineLimit        = 76
	base64LineBytes       = 57
	binaryShareDivisor    = 3
	messageIDDigestChars  = 32
	replyPrefix           = "re:"
	noSubjectReply        = "Re:"
)

type replyMessage struct {
	From       string
	To         string
	Subject    string
	MessageID  string
	InReplyTo  string
	References []string
	Body       string
	Date       time.Time
}

func replyMessageID(base, domain string) string {
	sum := sha256.Sum256([]byte(base))
	return "aura." + hex.EncodeToString(sum[:])[:messageIDDigestChars] + "@" + domain
}

func replySubject(original string) string {
	subject := singleLine(original)
	for {
		head := strings.TrimSpace(subject)
		if len(head) < len(replyPrefix) || !strings.EqualFold(head[:len(replyPrefix)], replyPrefix) {
			subject = head
			break
		}
		subject = head[len(replyPrefix):]
	}
	if subject == "" {
		return noSubjectReply
	}
	return "Re: " + truncateUTF8(subject, maxReplySubjectBytes)
}

func replyBody(texts []string) string {
	body := cleanBody(strings.Join(texts, "\n\n"))
	if len(body) <= MaxReplyBodyBytes {
		return body
	}
	return truncateUTF8(body, MaxReplyBodyBytes-len(replyTruncationNotice)) + replyTruncationNotice
}

func cleanBody(text string) string {
	text = strings.ToValidUTF8(text, "�")
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	text = strings.Map(func(r rune) rune {
		if r != '\n' && r != '\t' && unicode.IsControl(r) {
			return -1
		}
		return r
	}, text)
	return strings.TrimSpace(text)
}

func (m *replyMessage) render() ([]byte, error) {
	if !principal.IsPlainEmailAddress(m.From) || !principal.IsPlainEmailAddress(m.To) || !headerSafeID(m.MessageID) {
		return nil, Errorf(ErrorCodeMessageInvalid, "reply envelope or message id is not well formed")
	}
	body := cleanBody(m.Body)
	if body == "" || len(body) > MaxReplyBodyBytes {
		return nil, Errorf(ErrorCodeMessageInvalid, "reply body is empty or over the bound")
	}
	encoding, encoded := encodeBody(body)
	var out bytes.Buffer
	field := func(name, value string) {
		out.WriteString(name + ": " + value + "\r\n")
	}
	field("Date", m.Date.UTC().Format(time.RFC1123Z))
	field("From", m.From)
	field("To", m.To)
	field("Subject", encodeHeaderText(replySubject(m.Subject)))
	field("Message-ID", "<"+m.MessageID+">")
	if headerSafeID(m.InReplyTo) {
		field("In-Reply-To", "<"+m.InReplyTo+">")
	}
	if references := boundedReferences(m.References); len(references) > 0 {
		field("References", "<"+strings.Join(references, ">\r\n <")+">")
	}
	field("Auto-Submitted", "auto-replied")
	field("MIME-Version", "1.0")
	field("Content-Type", "text/plain; charset=utf-8")
	field("Content-Transfer-Encoding", encoding)
	out.WriteString("\r\n")
	out.Write(encoded)
	return out.Bytes(), nil
}

func encodeHeaderText(text string) string {
	encoded := mimeheader.QEncoding.Encode("utf-8", text)
	if encoded == text {
		return text
	}
	return strings.ReplaceAll(encoded, "?= =?", "?=\r\n =?")
}

func encodeBody(body string) (encoding string, encoded []byte) {
	nonASCII, longest, line := 0, 0, 0
	for i := range len(body) {
		switch {
		case body[i] == '\n':
			line = 0
			continue
		case body[i] >= unicode.MaxASCII:
			nonASCII++
		}
		line++
		longest = max(longest, line)
	}
	switch {
	case nonASCII == 0 && longest <= plainLineLimit:
		return "7bit", []byte(strings.ReplaceAll(body, "\n", "\r\n") + "\r\n")
	case nonASCII*binaryShareDivisor <= len(body):
		var out bytes.Buffer
		writer := quotedprintable.NewWriter(&out)
		_, _ = writer.Write([]byte(body + "\n"))
		_ = writer.Close()
		return "quoted-printable", out.Bytes()
	default:
		return "base64", wrapBase64([]byte(strings.ReplaceAll(body, "\n", "\r\n") + "\r\n"))
	}
}

func wrapBase64(data []byte) []byte {
	var out bytes.Buffer
	for len(data) > 0 {
		chunk := data[:min(len(data), base64LineBytes)]
		data = data[len(chunk):]
		out.WriteString(base64.StdEncoding.EncodeToString(chunk))
		out.WriteString("\r\n")
	}
	return out.Bytes()
}
