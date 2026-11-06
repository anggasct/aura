package email

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"mime/quotedprintable"
	"net/mail"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	mimepkg "mime"
)

var replyDate = time.Date(2026, time.November, 5, 9, 30, 0, 0, time.UTC)

func baseReply() replyMessage {
	return replyMessage{
		From:       testAddress,
		To:         ownerAddress,
		Subject:    "Plan the week",
		MessageID:  "aura.0123456789abcdef0123456789abcdef@example.com",
		InReplyTo:  "m1@example.com",
		References: []string{"root@example.com", "m1@example.com"},
		Body:       "Here is the agenda.",
		Date:       replyDate,
	}
}

func renderMessage(t *testing.T, reply *replyMessage) (parsed *mail.Message, raw []byte) {
	t.Helper()
	raw, err := reply.render()
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	parsed, err = mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	return parsed, raw
}

func decodedBody(t *testing.T, parsed *mail.Message) string {
	t.Helper()
	reader := parsed.Body
	switch parsed.Header.Get("Content-Transfer-Encoding") {
	case "quoted-printable":
		reader = quotedprintable.NewReader(parsed.Body)
	case "base64":
		reader = base64.NewDecoder(base64.StdEncoding, parsed.Body)
	case "7bit":
	default:
		t.Fatalf("unexpected transfer encoding %q", parsed.Header.Get("Content-Transfer-Encoding"))
	}
	body, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return strings.ReplaceAll(string(body), "\r\n", "\n")
}

func TestRender_BuildsAThreadedAutoReply(t *testing.T) {
	reply := baseReply()
	parsed, raw := renderMessage(t, &reply)
	want := map[string]string{
		"From":                      testAddress,
		"To":                        ownerAddress,
		"Subject":                   "Re: Plan the week",
		"Message-Id":                "<" + reply.MessageID + ">",
		"In-Reply-To":               "<m1@example.com>",
		"Auto-Submitted":            "auto-replied",
		"Mime-Version":              "1.0",
		"Content-Type":              "text/plain; charset=utf-8",
		"Content-Transfer-Encoding": "7bit",
		"Date":                      "Thu, 05 Nov 2026 09:30:00 +0000",
	}
	for key, value := range want {
		if got := parsed.Header.Get(key); got != value {
			t.Errorf("%s = %q, want %q", key, got, value)
		}
	}
	if got := parsed.Header.Get("References"); strings.Fields(got)[0] != "<root@example.com>" || strings.Fields(got)[1] != "<m1@example.com>" || len(strings.Fields(got)) != 2 {
		t.Errorf("References = %q", got)
	}
	if body := decodedBody(t, parsed); body != "Here is the agenda.\n" {
		t.Errorf("body = %q", body)
	}
	for _, forbidden := range []string{"Cc", "Bcc", "Reply-To", "Sender"} {
		if _, present := parsed.Header[forbidden]; present {
			t.Errorf("header %s present", forbidden)
		}
	}
	if bytes.ContainsAny(raw, "\x00") || bytes.Contains(raw, []byte("\n\n")) {
		t.Error("message holds a NUL or a bare line feed")
	}
}

func TestReplySubject(t *testing.T) {
	cases := map[string]struct {
		original string
		want     string
	}{
		"plain":                  {original: "Plan the week", want: "Re: Plan the week"},
		"already a reply":        {original: "Re: Plan the week", want: "Re: Plan the week"},
		"stacked replies":        {original: "RE: re: Re:   Plan the week", want: "Re: Plan the week"},
		"tight prefix":           {original: "re:Plan", want: "Re: Plan"},
		"re inside a word":       {original: "Reunion notes", want: "Re: Reunion notes"},
		"re later in the line":   {original: "Plan re: week", want: "Re: Plan re: week"},
		"empty":                  {original: "", want: "Re:"},
		"only a prefix":          {original: "Re:", want: "Re:"},
		"line break":             {original: "Plan\r\nBcc: attacker@evil.example", want: "Re: Plan Bcc: attacker@evil.example"},
		"nul":                    {original: "Plan\x00week", want: "Re: Plan week"},
		"unicode line break":     {original: "Plan week", want: "Re: Plan week"},
		"invalid utf-8":          {original: "Plan\xffweek", want: "Re: Planweek"},
		"truncated on a rune":    {original: strings.Repeat("é", 100), want: "Re: " + strings.Repeat("é", maxReplySubjectBytes/2)},
		"truncated at the bound": {original: strings.Repeat("a", 500), want: "Re: " + strings.Repeat("a", maxReplySubjectBytes)},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := replySubject(tc.original)
			if got != tc.want {
				t.Fatalf("replySubject(%q) = %q, want %q", tc.original, got, tc.want)
			}
			if again := replySubject(got); again != got {
				t.Errorf("replySubject is not idempotent: %q then %q", got, again)
			}
		})
	}
}

func TestRender_HeaderInjectionNeverReachesTheWire(t *testing.T) {
	reply := baseReply()
	reply.Subject = "hello\r\nBcc: attacker@evil.example\r\nX-Injected: yes\r\n\r\nbody smuggled"
	reply.InReplyTo = "m1@example.com>\r\nBcc: attacker@evil.example"
	reply.References = []string{"ok@example.com", "bad@example.com>\r\nBcc: attacker@evil.example", "nul\x00@example.com", "sp ace@example.com", "<angle@example.com>", "ünï@example.com"}
	parsed, raw := renderMessage(t, &reply)
	for _, forbidden := range []string{"Bcc", "X-Injected", "Cc"} {
		if _, present := parsed.Header[forbidden]; present {
			t.Errorf("header %s reached the wire", forbidden)
		}
	}
	if got := parsed.Header.Get("Subject"); got != "Re: hello Bcc: attacker@evil.example X-Injected: yes body smuggled" {
		t.Errorf("Subject = %q, want one flattened line", got)
	}
	if _, present := parsed.Header["In-Reply-To"]; present {
		t.Error("an unsafe In-Reply-To id reached the wire")
	}
	if got := parsed.Header.Get("References"); strings.Join(strings.Fields(got), " ") != "<ok@example.com>" {
		t.Errorf("References = %q, want only the safe id", got)
	}
	if bytes.Contains(raw, []byte("\r\nBcc:")) || bytes.Contains(raw, []byte("\r\nX-Injected:")) {
		t.Error("raw message starts a line with an injected header")
	}
	if body := decodedBody(t, parsed); strings.Contains(body, "smuggled") {
		t.Errorf("body = %q, want the injected text kept out of it", body)
	}
}

func TestRender_RejectsUnsafeEnvelopeAndIdentifiers(t *testing.T) {
	cases := map[string]func(*replyMessage){
		"recipient with a line break": func(m *replyMessage) { m.To = "owner@example.com\r\nBcc: x@evil.example" },
		"recipient with an angle":     func(m *replyMessage) { m.To = "a>b@example.com" },
		"recipient list":              func(m *replyMessage) { m.To = "a@example.com,b@example.com" },
		"empty recipient":             func(m *replyMessage) { m.To = "" },
		"sender with a line break":    func(m *replyMessage) { m.From = "aura@example.com\r\nBcc: x@evil.example" },
		"message id with a space":     func(m *replyMessage) { m.MessageID = "a b@example.com" },
		"empty message id":            func(m *replyMessage) { m.MessageID = "" },
		"empty body":                  func(m *replyMessage) { m.Body = " \n\t " },
		"body of control characters":  func(m *replyMessage) { m.Body = "\x00\x01\x02" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			reply := baseReply()
			mutate(&reply)
			raw, err := reply.render()
			if code, ok := CodeOf(err); !ok || code != ErrorCodeMessageInvalid || raw != nil {
				t.Fatalf("render = %q, %v; want message_invalid", raw, err)
			}
		})
	}
}

func TestRender_NonASCIISubjectIsEncodedAndDecodes(t *testing.T) {
	subjects := []string{
		"Rapat énergie 日本語",
		"Ünïcode " + strings.Repeat("日本語", 30),
		"😀 emoji",
	}
	for _, original := range subjects {
		t.Run(original[:min(len(original), 12)], func(t *testing.T) {
			reply := baseReply()
			reply.Subject = original
			parsed, raw := renderMessage(t, &reply)
			headerEnd := bytes.Index(raw, []byte("\r\n\r\n"))
			for i, b := range raw[:headerEnd] {
				if b >= utf8.RuneSelf {
					t.Fatalf("header byte %d is not ASCII", i)
				}
			}
			decoded, err := new(mimepkg.WordDecoder).DecodeHeader(parsed.Header.Get("Subject"))
			if err != nil {
				t.Fatalf("DecodeHeader: %v", err)
			}
			if want := replySubject(original); decoded != want {
				t.Errorf("decoded subject = %q, want %q", decoded, want)
			}
			for _, line := range strings.Split(string(raw[:headerEnd]), "\r\n") {
				if len(line) > 998 {
					t.Errorf("header line of %d bytes", len(line))
				}
			}
		})
	}
}

func TestRender_BoundsReferencesToTheLastTwenty(t *testing.T) {
	reply := baseReply()
	reply.References = nil
	for i := range 30 {
		reply.References = append(reply.References, fmt.Sprintf("r%02d@example.com", i))
	}
	parsed, _ := renderMessage(t, &reply)
	fields := strings.Fields(parsed.Header.Get("References"))
	if len(fields) != maxReferenceIDs {
		t.Fatalf("References carries %d ids, want %d", len(fields), maxReferenceIDs)
	}
	if fields[0] != "<r10@example.com>" || fields[len(fields)-1] != "<r29@example.com>" {
		t.Errorf("References = %v, want the last twenty in order", fields)
	}
}

func TestRender_BodyEncodings(t *testing.T) {
	cases := map[string]struct {
		body     string
		encoding string
	}{
		"short ascii":         {body: "Hello\nWorld", encoding: "7bit"},
		"dot line":            {body: "line\n.\n..hidden\nend", encoding: "7bit"},
		"long ascii line":     {body: strings.Repeat("word ", 60), encoding: "quoted-printable"},
		"equals sign":         {body: strings.Repeat("a=b ", 30), encoding: "quoted-printable"},
		"some accents":        {body: "Café au lait, s'il vous plaît. " + strings.Repeat("x", 80), encoding: "quoted-printable"},
		"mostly non-ascii":    {body: strings.Repeat("日本語のテキスト", 20), encoding: "base64"},
		"trailing whitespace": {body: "line one  \nline two\t", encoding: "7bit"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			reply := baseReply()
			reply.Body = tc.body
			parsed, raw := renderMessage(t, &reply)
			if got := parsed.Header.Get("Content-Transfer-Encoding"); got != tc.encoding {
				t.Fatalf("Content-Transfer-Encoding = %q, want %q", got, tc.encoding)
			}
			if want := cleanBody(tc.body) + "\n"; decodedBody(t, parsed) != want {
				t.Errorf("body does not round-trip: %q", decodedBody(t, parsed))
			}
			for _, line := range strings.Split(string(raw), "\r\n") {
				if len(line) > 998 {
					t.Errorf("line of %d bytes", len(line))
				}
			}
			if tc.encoding != "7bit" {
				for _, line := range strings.Split(string(raw), "\r\n") {
					if len(line) > 78 {
						t.Errorf("encoded line of %d bytes", len(line))
					}
				}
			}
			if bytes.Contains(raw, []byte("\n")) && bytes.Count(raw, []byte("\n")) != bytes.Count(raw, []byte("\r\n")) {
				t.Error("message holds a bare line feed")
			}
		})
	}
}

func TestReplyBody_JoinsPartsAndBoundsTheSize(t *testing.T) {
	if got := replyBody([]string{"first", "second\r\nline"}); got != "first\n\nsecond\nline" {
		t.Errorf("replyBody = %q", got)
	}
	huge := strings.Repeat("é", MaxReplyBodyBytes)
	got := replyBody([]string{huge})
	if len(got) > MaxReplyBodyBytes || !strings.HasSuffix(got, replyTruncationNotice) || !utf8.ValidString(got) {
		t.Fatalf("bounded body: %d bytes, valid %v, notice %v", len(got), utf8.ValidString(got), strings.HasSuffix(got, replyTruncationNotice))
	}
	exact := strings.Repeat("a", MaxReplyBodyBytes)
	if replyBody([]string{exact}) != exact {
		t.Error("a body at the bound was altered")
	}
	if got := replyBody([]string{"ok\x00\x07text\xff"}); got != "oktext�" {
		t.Errorf("replyBody = %q, want control characters stripped and invalid bytes replaced", got)
	}
}

func TestReplyMessageID_IsDeterministicFromTheEffectAndDomain(t *testing.T) {
	first := replyMessageID("turn-1", "example.com")
	if first != replyMessageID("turn-1", "example.com") {
		t.Error("the same effect produced two ids")
	}
	if first == replyMessageID("turn-2", "example.com") {
		t.Error("two effects share an id")
	}
	if !strings.HasSuffix(first, "@example.com") || !headerSafeID(first) {
		t.Errorf("id %q is not a header-safe id in the account domain", first)
	}
}

func TestHeaderSafeID(t *testing.T) {
	cases := map[string]struct {
		id   string
		want bool
	}{
		"plain":           {id: "a.b+c@example.com", want: true},
		"empty":           {id: ""},
		"space":           {id: "a b@example.com"},
		"carriage return": {id: "a\r@example.com"},
		"line feed":       {id: "a\n@example.com"},
		"nul":             {id: "a\x00@example.com"},
		"opening angle":   {id: "<a@example.com"},
		"closing angle":   {id: "a@example.com>"},
		"comma":           {id: "a,b@example.com"},
		"quote":           {id: "\"a\"@example.com"},
		"non-ascii":       {id: "é@example.com"},
		"too long":        {id: strings.Repeat("a", maxHeaderIDBytes+1)},
		"at the bound":    {id: strings.Repeat("a", maxHeaderIDBytes), want: true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := headerSafeID(tc.id); got != tc.want {
				t.Fatalf("headerSafeID(%q) = %v, want %v", tc.id, got, tc.want)
			}
		})
	}
}
