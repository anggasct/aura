package gmail

import (
	"bytes"
	"mime"
	"net/mail"
	"strings"
	"testing"
	"unicode/utf8"
)

func parseSubject(t *testing.T, header string) (decoded string, lines []string) {
	t.Helper()
	message, err := mail.ReadMessage(bytes.NewBufferString(header + "\r\n\r\n"))
	if err != nil {
		t.Fatalf("parse %q: %v", header, err)
	}
	decoded, err = new(mime.WordDecoder).DecodeHeader(message.Header.Get("Subject"))
	if err != nil {
		t.Fatalf("decode %q: %v", header, err)
	}
	return decoded, strings.Split(header, crlf)
}

func checkSubjectHeader(t *testing.T, subject string) {
	t.Helper()
	header := subjectHeader(subject)
	decoded, lines := parseSubject(t, header)
	if decoded != subject {
		t.Fatalf("subject %q round-trips to %q via %q", subject, decoded, header)
	}
	for _, line := range lines {
		if len(line) > foldWidth {
			t.Fatalf("line of %d bytes in %q", len(line), header)
		}
		for i := range len(line) {
			if line[i] < 0x20 || line[i] > 0x7e {
				t.Fatalf("byte %#x in %q", line[i], header)
			}
		}
	}
	for i, line := range lines {
		if i > 0 && !strings.HasPrefix(line, " ") {
			t.Fatalf("continuation line %q does not start with whitespace", line)
		}
	}
}

func TestSubjectHeaderStaysWithinTheFoldWidthAndRoundTrips(t *testing.T) {
	cases := []struct {
		name    string
		subject string
		plain   bool
	}{
		{"short", "Hello", true},
		{"fits on the first line", strings.Repeat("a", 69), true},
		{"one byte past the first line folds", strings.Repeat("ab ", 23) + "abc", true},
		{"many words fold", strings.Repeat("word ", 60) + "end", true},
		{"single long word is encoded", strings.Repeat("a", 200), false},
		{"word of 77 after a fold", "x " + strings.Repeat("a", 77), true},
		{"word of 78 after a fold", "x " + strings.Repeat("a", 78), false},
		{"non-ascii", "Café ☕", false},
		{"long non-ascii", strings.Repeat("日本語のとても長い件名 ", 40), false},
		{"four byte runes", strings.Repeat("🙂", 100), false},
		{"encoded word marker", "see =?UTF-8?B?aGk=?= here", false},
		{"uppercase marker", "x =?utf-8?q?a?= y", false},
		{"leading space", " hello", false},
		{"trailing space", "hello ", false},
		{"double space", "a  b", false},
		{"punctuation", `Re: "quoted" (parens) <tag> ; , : @ \ [x]`, true},
		{"maximum length", strings.Repeat("é", maxSubjectBytes/2), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			checkSubjectHeader(t, tc.subject)
			header := subjectHeader(tc.subject)
			if plain := !strings.Contains(header, "=?UTF-8?B?"); plain != tc.plain {
				t.Fatalf("plain = %v, want %v: %q", plain, tc.plain, header)
			}
		})
	}
}

func TestEncodedSubjectNeverSplitsARune(t *testing.T) {
	header := encodedSubject(strings.Repeat("a🙂", 60))
	for _, line := range strings.Split(header, crlf) {
		word := strings.TrimSpace(strings.TrimPrefix(line, subjectPrefix))
		decoded, err := new(mime.WordDecoder).Decode(word)
		if err != nil || !utf8.ValidString(decoded) || strings.ContainsRune(decoded, utf8.RuneError) {
			t.Fatalf("word %q does not decode to whole runes: %v", word, err)
		}
	}
}

func TestFoldTokensKeepsAddressesWhole(t *testing.T) {
	folded, fits := foldTokens("To: ", []string{"a@example.com", strings.Repeat("b", 100) + "@example.com", "c@example.com"}, ",")
	if fits {
		t.Fatal("a token over the width cannot fit")
	}
	if got := strings.Count(folded, crlf); got != 2 {
		t.Fatalf("folds = %d in %q", got, folded)
	}
	parsed, err := mail.ReadMessage(bytes.NewBufferString(folded + "\r\n\r\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	list, err := parsed.Header.AddressList("To")
	if err != nil || len(list) != 3 {
		t.Fatalf("list = %v, %v", list, err)
	}
}

func TestValidAddress(t *testing.T) {
	cases := []struct {
		address string
		want    bool
	}{
		{"alice@example.com", true},
		{"first.last+tag@sub.example.co.uk", true},
		{"o'brien@example.com", true},
		{"a_b-c@example-host.com", true},
		{"x@xn--bcher-kva.example", true},
		{"A@EXAMPLE.COM", true},
		{"", false},
		{"alice", false},
		{"@example.com", false},
		{"alice@", false},
		{"alice@example", false},
		{"alice@example.", false},
		{"alice@.example.com", false},
		{"alice@exa mple.com", false},
		{"alice@-example.com", false},
		{"alice@example-.com", false},
		{"alice@example.123", false},
		{"alice@[127.0.0.1]", false},
		{"alice@127.0.0.1", false},
		{" alice@example.com", false},
		{"alice@example.com ", false},
		{"alice@example.com,bob@example.com", false},
		{"alice@example.com;", false},
		{"<alice@example.com>", false},
		{"Alice <alice@example.com>", false},
		{"alice@example.com (Alice)", false},
		{"\"a\"@example.com", false},
		{"a\\@b@example.com", false},
		{"alice@exämple.com", false},
		{"älice@example.com", false},
		{"alice@example.com\r\n", false},
		{"alice@example.com\x00", false},
		{"alice@example.com\x7f", false},
		{strings.Repeat("a", 65) + "@example.com", false},
		{"a@" + strings.Repeat("b", 64) + ".com", false},
		{"a@" + strings.Repeat("b.", 130) + "com", false},
	}
	for _, tc := range cases {
		t.Run(tc.address, func(t *testing.T) {
			if got := validAddress(tc.address); got != tc.want {
				t.Fatalf("validAddress(%q) = %v, want %v", tc.address, got, tc.want)
			}
		})
	}
}

func TestValidMessageID(t *testing.T) {
	cases := []struct {
		id   string
		want bool
	}{
		{"<abc123@mail.example.com>", true},
		{"<CAF=x+y_z/1@mail.gmail.com>", true},
		{"<a.b@c>", true},
		{"", false},
		{"<>", false},
		{"<@x>", false},
		{"abc@example.com", false},
		{"<a b@example.com>", false},
		{"<a@example.com> <b@example.com>", false},
		{"<a@example.com>\r\n", false},
		{"<a\x00@example.com>", false},
		{"<a@exämple.com>", false},
		{"<a..b@example.com>", false},
		{"<" + strings.Repeat("a", 250) + "@example.com>", false},
	}
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			if got := validMessageID(tc.id); got != tc.want {
				t.Fatalf("validMessageID(%q) = %v, want %v", tc.id, got, tc.want)
			}
		})
	}
}

func FuzzSubjectHeader(f *testing.F) {
	for _, seed := range []string{"Hello", "Café ☕", "=?UTF-8?B?aGk=?=", strings.Repeat("word ", 40), strings.Repeat("🙂", 80), " a  b ", "x\ty"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, subject string) {
		if !validSubject(subject) {
			return
		}
		checkSubjectHeader(t, subject)
	})
}

func FuzzCleanBody(f *testing.F) {
	for _, seed := range []string{"plain", "a\r\nb\rc", "x\x00y\u200bz", "\xff\xfe", strings.Repeat("日", 40000)} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, text string) {
		cleaned := cleanBody(text)
		if !utf8.ValidString(cleaned) {
			t.Fatalf("invalid utf-8 from %q", text)
		}
		for _, r := range cleaned {
			if r != '\n' && r != '\t' && (unwanted(r) || lineBreak(r)) {
				t.Fatalf("kept %U from %q", r, text)
			}
		}
		if strings.Contains(cleaned, "\n\n\n") {
			t.Fatalf("blank lines not collapsed in %q", cleaned)
		}
	})
}
