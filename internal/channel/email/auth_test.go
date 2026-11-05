package email

import (
	"strings"
	"testing"

	"github.com/emersion/go-message/mail"
)

func headerFrom(t *testing.T, lines ...string) *mail.Header {
	t.Helper()
	raw := strings.Join(lines, "\r\n") + "\r\n\r\n"
	header, ok := readHeader([]byte(raw))
	if !ok {
		t.Fatalf("readHeader rejected %q", raw)
	}
	return header
}

func TestAuthenticatedSender(t *testing.T) {
	const trusted = "Authentication-Results: mx.example.com; dmarc=pass header.from=example.com"
	cases := map[string]struct {
		lines []string
		want  bool
	}{
		"dmarc pass from the trusted server": {
			lines: []string{trusted, "From: a@example.com"},
			want:  true,
		},
		"no authentication results": {
			lines: []string{"From: a@example.com"},
		},
		"dmarc fail": {
			lines: []string{"Authentication-Results: mx.example.com; dmarc=fail header.from=example.com"},
		},
		"dmarc none": {
			lines: []string{"Authentication-Results: mx.example.com; dmarc=none header.from=example.com"},
		},
		"dmarc temperror": {
			lines: []string{"Authentication-Results: mx.example.com; dmarc=temperror header.from=example.com"},
		},
		"dmarc permerror": {
			lines: []string{"Authentication-Results: mx.example.com; dmarc=permerror header.from=example.com"},
		},
		"dkim and spf pass without dmarc": {
			lines: []string{"Authentication-Results: mx.example.com; dkim=pass header.d=example.com; spf=pass smtp.mailfrom=example.com"},
		},
		"only the identifier": {
			lines: []string{"Authentication-Results: mx.example.com; none"},
		},
		"dmarc pass for another from domain": {
			lines: []string{"Authentication-Results: mx.example.com; dmarc=pass header.from=attacker.net"},
		},
		"dmarc pass for a lookalike domain": {
			lines: []string{"Authentication-Results: mx.example.com; dmarc=pass header.from=example.com.attacker.net"},
		},
		"dmarc pass for a subdomain": {
			lines: []string{"Authentication-Results: mx.example.com; dmarc=pass header.from=mail.example.com"},
		},
		"header from in different case": {
			lines: []string{"Authentication-Results: mx.example.com; dmarc=pass header.from=EXAMPLE.COM"},
			want:  true,
		},
		"header from with a trailing dot": {
			lines: []string{"Authentication-Results: mx.example.com; dmarc=pass header.from=example.com."},
			want:  true,
		},
		"header from as the full address": {
			lines: []string{"Authentication-Results: mx.example.com; dmarc=pass header.from=a@example.com"},
			want:  true,
		},
		"header from as another full address": {
			lines: []string{"Authentication-Results: mx.example.com; dmarc=pass header.from=a@attacker.net"},
		},
		"dmarc pass without header from": {
			lines: []string{"Authentication-Results: mx.example.com; dmarc=pass"},
			want:  true,
		},
		"identifier in different case": {
			lines: []string{"Authentication-Results: MX.Example.COM; dmarc=pass header.from=example.com"},
			want:  true,
		},
		"identifier with a version": {
			lines: []string{"Authentication-Results: mx.example.com 1; dmarc=pass header.from=example.com"},
			want:  true,
		},
		"identifier with a comment": {
			lines: []string{"Authentication-Results: mx.example.com (Postfix 3.9); dmarc=pass header.from=example.com"},
			want:  true,
		},
		"result with a comment": {
			lines: []string{"Authentication-Results: mx.example.com; dmarc=pass (p=reject dis=none) header.from=example.com"},
			want:  true,
		},
		"folded header": {
			lines: []string{"Authentication-Results: mx.example.com;", "\tdkim=pass header.d=example.com;", "\tdmarc=pass header.from=example.com"},
			want:  true,
		},
		"comment hides a pass behind a fail": {
			lines: []string{"Authentication-Results: mx.example.com; dmarc=fail (was dmarc=pass) header.from=example.com"},
		},
		"unclosed comment": {
			lines: []string{"Authentication-Results: mx.example.com; dmarc=pass (header.from=example.com"},
		},
		"malformed result": {
			lines: []string{"Authentication-Results: mx.example.com; dmarc"},
		},
		"unsupported version": {
			lines: []string{"Authentication-Results: mx.example.com 2; dmarc=pass header.from=example.com"},
		},
		"forged id with a pass": {
			lines: []string{"Authentication-Results: mx.attacker.net; dmarc=pass header.from=example.com"},
		},
		"forged id that extends the trusted one": {
			lines: []string{"Authentication-Results: mx.example.com.attacker.net; dmarc=pass header.from=example.com"},
		},
		"forged id above the trusted header": {
			lines: []string{"Authentication-Results: mx.attacker.net; dmarc=pass header.from=example.com", "Authentication-Results: mx.example.com; dmarc=fail header.from=example.com"},
		},
		"forged id above a passing trusted header": {
			lines: []string{"Authentication-Results: mx.attacker.net; dmarc=fail header.from=example.com", trusted},
			want:  true,
		},
		"forged pass below a failing trusted header": {
			lines: []string{"Authentication-Results: mx.example.com; dmarc=fail header.from=example.com", trusted},
		},
		"forged pass below a trusted header without dmarc": {
			lines: []string{"Authentication-Results: mx.example.com; spf=pass smtp.mailfrom=example.com", trusted},
		},
		"forged pass below a trusted header for another domain": {
			lines: []string{"Authentication-Results: mx.example.com; dmarc=pass header.from=attacker.net", trusted},
		},
		"forged pass below an unreadable trusted header": {
			lines: []string{"Authentication-Results: mx.example.com; dmarc=pass (", trusted},
		},
		"forged pass below a trusted header that cannot parse": {
			lines: []string{"Authentication-Results: mx.example.com; dmarc", trusted},
		},
		"forged pass below a foreign header above the trusted fail": {
			lines: []string{"Authentication-Results: mx.attacker.net; dmarc=pass header.from=example.com", "Authentication-Results: mx.example.com; dmarc=none header.from=example.com", trusted},
		},
		"pass for the domain and a fail for it": {
			lines: []string{"Authentication-Results: mx.example.com; dmarc=pass header.from=example.com; dmarc=fail header.from=example.com"},
		},
		"fail for the domain and a pass for it": {
			lines: []string{"Authentication-Results: mx.example.com; dmarc=fail header.from=example.com; dmarc=pass header.from=example.com"},
		},
		"pass for the domain beside a fail for another": {
			lines: []string{"Authentication-Results: mx.example.com; dmarc=fail header.from=attacker.net; dmarc=pass header.from=example.com"},
			want:  true,
		},
		"header name in a different case": {
			lines: []string{"authentication-results: mx.example.com; dmarc=pass header.from=example.com"},
			want:  true,
		},
		"similar header name": {
			lines: []string{"X-Authentication-Results: mx.example.com; dmarc=pass header.from=example.com"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			header := headerFrom(t, tc.lines...)
			if got := authenticatedSender(&header.Header.Header, "mx.example.com", "example.com"); got != tc.want {
				t.Fatalf("authenticatedSender() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAuthenticatedSenderFailsClosedWithoutInputs(t *testing.T) {
	header := headerFrom(t, "Authentication-Results: mx.example.com; dmarc=pass header.from=example.com")
	for name, tc := range map[string]struct{ id, domain string }{
		"empty server id": {id: "", domain: "example.com"},
		"empty domain":    {id: "mx.example.com", domain: ""},
	} {
		t.Run(name, func(t *testing.T) {
			if authenticatedSender(&header.Header.Header, tc.id, tc.domain) {
				t.Fatal("authenticatedSender() = true without a server id or domain")
			}
		})
	}
}

func TestHeaderFieldsAreTopmostFirst(t *testing.T) {
	header := headerFrom(t,
		"Authentication-Results: first.example",
		"Received: from relay",
		"Authentication-Results: second.example",
		"Authentication-Results: third.example",
	)
	var ids []string
	fields := header.Header.Header.Fields()
	for fields.Next() {
		if fields.Key() == authResultsKey {
			ids = append(ids, fields.Value())
		}
	}
	want := []string{"first.example", "second.example", "third.example"}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Fatalf("Authentication-Results order = %v, want raw order %v", ids, want)
	}
}
