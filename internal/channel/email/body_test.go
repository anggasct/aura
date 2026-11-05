package email

import (
	"strings"
	"testing"
)

func mime(headers, body string) []byte {
	return []byte(strings.ReplaceAll(headers+"\n\n"+body, "\n", "\r\n"))
}

func TestMessageBody(t *testing.T) {
	multipart := func(subtype, parts string) []byte {
		return mime("From: a@example.com\nMIME-Version: 1.0\nContent-Type: multipart/"+subtype+`; boundary="b1"`, parts+"\n--b1--\n")
	}
	part := func(contentType, encoding, content string) string {
		header := "--b1\nContent-Type: " + contentType
		if encoding != "" {
			header += "\nContent-Transfer-Encoding: " + encoding
		}
		return header + "\n\n" + content + "\n"
	}
	cases := map[string]struct {
		raw  []byte
		want string
	}{
		"plain text": {
			raw:  mime("From: a@example.com\nContent-Type: text/plain; charset=utf-8", "Hello there.\nSecond line."),
			want: "Hello there.\nSecond line.",
		},
		"no content type": {
			raw:  mime("From: a@example.com", "Plain body"),
			want: "Plain body",
		},
		"quoted reply history is kept": {
			raw:  mime("From: a@example.com\nContent-Type: text/plain", "Thanks!\n\nOn Mon, Owner wrote:\n> earlier text\n> more"),
			want: "Thanks!\n\nOn Mon, Owner wrote:\n> earlier text\n> more",
		},
		"quoted printable": {
			raw:  mime("From: a@example.com\nContent-Type: text/plain; charset=utf-8\nContent-Transfer-Encoding: quoted-printable", "Caf=C3=A9 and a soft=\nbreak"),
			want: "Café and a softbreak",
		},
		"base64": {
			raw:  mime("From: a@example.com\nContent-Type: text/plain; charset=utf-8\nContent-Transfer-Encoding: base64", "SGVsbG8gd29ybGQ="),
			want: "Hello world",
		},
		"latin1 charset": {
			raw:  mime("From: a@example.com\nContent-Type: text/plain; charset=iso-8859-1\nContent-Transfer-Encoding: quoted-printable", "Caf=E9"),
			want: "Café",
		},
		"alternative prefers plain": {
			raw:  multipart("alternative", part("text/plain; charset=utf-8", "", "plain version")+part("text/html; charset=utf-8", "", "<p>html version</p>")),
			want: "plain version",
		},
		"alternative with html first still prefers plain": {
			raw:  multipart("alternative", part("text/html; charset=utf-8", "", "<p>html version</p>")+part("text/plain; charset=utf-8", "", "plain version")),
			want: "plain version",
		},
		"html only is stripped": {
			raw:  mime("From: a@example.com\nContent-Type: text/html; charset=utf-8", "<html><head><title>t</title><style>p{color:red}</style></head><body><p>Hello <b>world</b></p><div>Next &amp; last</div><script>alert(1)</script></body></html>"),
			want: "Hello world\n\nNext & last",
		},
		"html entities and breaks": {
			raw:  mime("From: a@example.com\nContent-Type: text/html", "one<br>two<br/>three&nbsp;four &lt;tag&gt;"),
			want: "one\ntwo\nthree four <tag>",
		},
		"whitespace only plain falls back to html": {
			raw:  multipart("alternative", part("text/plain", "", "   \n ")+part("text/html", "", "<p>real</p>")),
			want: "real",
		},
		"attachment is ignored": {
			raw:  multipart("mixed", part("text/plain", "", "body text")+"--b1\nContent-Type: text/plain; name=\"notes.txt\"\nContent-Disposition: attachment; filename=\"notes.txt\"\n\nattachment text\n"),
			want: "body text",
		},
		"attachment only": {
			raw: multipart("mixed", "--b1\nContent-Type: application/pdf\nContent-Disposition: attachment; filename=\"a.pdf\"\n\n%PDF-1.4\n"),
		},
		"nested multipart": {
			raw: mime("From: a@example.com\nMIME-Version: 1.0\nContent-Type: multipart/mixed; boundary=\"outer\"",
				"--outer\nContent-Type: multipart/alternative; boundary=\"inner\"\n\n--inner\nContent-Type: text/plain\n\ndeep plain\n--inner--\n--outer--\n"),
			want: "deep plain",
		},
		"image part is ignored": {
			raw:  multipart("mixed", part("image/png", "base64", "iVBORw0KGgo=")+part("text/plain", "", "caption")),
			want: "caption",
		},
		"control characters are removed": {
			raw:  mime("From: a@example.com\nContent-Type: text/plain", "ok\x00\x1b[31m text\x07"),
			want: "ok[31m text",
		},
		"empty body": {
			raw: mime("From: a@example.com\nContent-Type: text/plain", ""),
		},
		"unknown charset still yields text": {
			raw:  mime("From: a@example.com\nContent-Type: text/plain; charset=x-unknown-charset", "plain ascii"),
			want: "plain ascii",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := messageBody(tc.raw)
			if err != nil {
				t.Fatalf("messageBody() error = %v", err)
			}
			if got != tc.want {
				t.Fatalf("messageBody() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestMessageBodyRejectsUnreadableMessages(t *testing.T) {
	cases := map[string][]byte{
		"no header":         []byte("not a message at all"),
		"leading space":     []byte(" From: a@example.com\r\n\r\nbody"),
		"malformed header":  []byte("From a@example.com\r\n\r\nbody"),
		"invalid key bytes": []byte("Fr om: a@example.com\r\n\r\nbody"),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := messageBody(raw); err == nil {
				t.Fatal("messageBody() accepted an unreadable message")
			}
		})
	}
}

func TestMessageBodyBoundsMimeStructure(t *testing.T) {
	var nested strings.Builder
	for depth := range 20 {
		nested.WriteString("--b" + string(rune('a'+depth)) + "\nContent-Type: multipart/mixed; boundary=\"b" + string(rune('b'+depth)) + "\"\n\n")
	}
	raw := mime("From: a@example.com\nMIME-Version: 1.0\nContent-Type: multipart/mixed; boundary=\"ba\"", nested.String()+"--bu\nContent-Type: text/plain\n\ntoo deep\n")
	got, err := messageBody(raw)
	if err != nil {
		t.Fatalf("messageBody() error = %v", err)
	}
	if got != "" {
		t.Errorf("messageBody() = %q, want nothing from a structure deeper than the bound", got)
	}
}

func TestTurnInput(t *testing.T) {
	long := strings.Repeat("é", maxTurnInputBytes)
	cases := map[string]struct {
		subject string
		body    string
		check   func(t *testing.T, got string)
	}{
		"subject and body": {subject: "Plan", body: "Do it", check: func(t *testing.T, got string) {
			if got != "Subject: Plan\n\nDo it" {
				t.Errorf("turnInput() = %q", got)
			}
		}},
		"subject only": {subject: "Quick question", check: func(t *testing.T, got string) {
			if got != "Subject: Quick question" {
				t.Errorf("turnInput() = %q", got)
			}
		}},
		"body only": {body: "Just text", check: func(t *testing.T, got string) {
			if got != "Just text" {
				t.Errorf("turnInput() = %q", got)
			}
		}},
		"nothing": {check: func(t *testing.T, got string) {
			if got != "" {
				t.Errorf("turnInput() = %q, want empty", got)
			}
		}},
		"exactly at the bound": {body: strings.Repeat("a", maxTurnInputBytes), check: func(t *testing.T, got string) {
			if len(got) != maxTurnInputBytes || strings.HasSuffix(got, truncationNotice) {
				t.Errorf("turnInput() length = %d, want %d without a notice", len(got), maxTurnInputBytes)
			}
		}},
		"over the bound is cut on a rune boundary": {body: long, check: func(t *testing.T, got string) {
			if !strings.HasSuffix(got, truncationNotice) || len(got) > maxTurnInputBytes+len(truncationNotice) {
				t.Errorf("turnInput() length = %d, want at most %d with a notice", len(got), maxTurnInputBytes+len(truncationNotice))
			}
			if strings.ContainsRune(got, '�') {
				t.Error("turnInput() cut a multibyte character")
			}
		}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			tc.check(t, turnInput(tc.subject, tc.body))
		})
	}
}
