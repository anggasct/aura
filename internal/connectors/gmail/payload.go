package gmail

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime"
	"strings"

	"golang.org/x/net/html/charset"
)

type header struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type partBody struct {
	AttachmentID string `json:"attachmentId"`
	Size         int64  `json:"size"`
	Data         string `json:"data"`
}

type part struct {
	MimeType string   `json:"mimeType"`
	Filename string   `json:"filename"`
	Headers  []header `json:"headers"`
	Body     partBody `json:"body"`
	Parts    []part   `json:"parts"`
}

type message struct {
	ID       string `json:"id"`
	ThreadID string `json:"threadId"`
	Snippet  string `json:"snippet"`
	Payload  *part  `json:"payload"`
}

type attachment struct {
	Name     string `json:"name"`
	MimeType string `json:"mime_type"`
	Size     int64  `json:"size"`
}

type extraction struct {
	text        string
	attachments []attachment
	truncated   bool
}

type collector struct {
	plain       strings.Builder
	markup      bytes.Buffer
	attachments []attachment
	parts       int
	incomplete  bool
}

func extractBody(root *part) *extraction {
	c := &collector{}
	if root != nil {
		c.walk(root, 0)
	}
	text := cleanBody(c.plain.String())
	if text == "" {
		reduced, complete := htmlText(c.markup.Bytes())
		c.incomplete = c.incomplete || !complete
		text = cleanBody(reduced)
	}
	text, cut := truncateBytes(text, maxReadBody)
	return &extraction{text: text, attachments: c.attachments, truncated: cut || c.incomplete}
}

func (c *collector) walk(p *part, depth int) {
	if depth > maxMIMEDepth || c.parts >= maxMIMEParts {
		c.incomplete = true
		return
	}
	c.parts++
	for i := range p.Parts {
		c.walk(&p.Parts[i], depth+1)
	}
	if len(p.Parts) > 0 {
		return
	}
	if p.Filename != "" {
		c.attach(p)
		return
	}
	switch strings.ToLower(strings.TrimSpace(p.MimeType)) {
	case "text/plain", "":
		c.text(p, false)
	case "text/html":
		c.text(p, true)
	default:
		if p.Body.AttachmentID != "" {
			c.attach(p)
		}
	}
}

func (c *collector) attach(p *part) {
	if len(c.attachments) >= maxAttachments {
		c.incomplete = true
		return
	}
	c.attachments = append(c.attachments, attachment{
		Name:     cleanLine(p.Filename, maxFilenameRunes),
		MimeType: cleanLine(strings.ToLower(p.MimeType), 128),
		Size:     max(p.Body.Size, 0),
	})
}

func (c *collector) text(p *part, markup bool) {
	if p.Body.Data == "" {
		if p.Body.AttachmentID != "" {
			c.incomplete = true
		}
		return
	}
	data, ok := decodeData(p.Body.Data)
	if !ok {
		c.incomplete = true
		return
	}
	converted := convertCharset(data, headerValue(p.Headers, "Content-Type"))
	if markup {
		if c.markup.Len() >= maxTextBytes {
			c.incomplete = true
			return
		}
		c.markup.Write(converted)
		c.markup.WriteByte('\n')
		return
	}
	if c.plain.Len() >= maxTextBytes {
		c.incomplete = true
		return
	}
	if c.plain.Len() > 0 {
		c.plain.WriteString("\n\n")
	}
	c.plain.Write(converted)
}

func decodeData(data string) ([]byte, bool) {
	if len(data) > base64.RawURLEncoding.EncodedLen(maxPartBytes) {
		return nil, false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(data, "="))
	if err != nil {
		return nil, false
	}
	return decoded, true
}

func convertCharset(data []byte, contentType string) []byte {
	_, params, err := mime.ParseMediaType(contentType)
	label := strings.ToLower(params["charset"])
	if err != nil || label == "" || label == "utf-8" || label == "us-ascii" {
		return data
	}
	reader, err := charset.NewReaderLabel(label, bytes.NewReader(data))
	if err != nil {
		return data
	}
	converted, err := io.ReadAll(io.LimitReader(reader, maxTextBytes+1))
	if err != nil {
		return data
	}
	return converted
}

func headerValue(headers []header, name string) string {
	for i := range headers {
		if strings.EqualFold(headers[i].Name, name) {
			return headers[i].Value
		}
	}
	return ""
}

func headerValues(headers []header, name string) []string {
	var values []string
	for i := range headers {
		if strings.EqualFold(headers[i].Name, name) {
			values = append(values, headers[i].Value)
		}
	}
	return values
}

func decodeMessage(body []byte) (*message, error) {
	var decoded message
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, fail(CodeResponseInvalid)
	}
	return &decoded, nil
}
