package knowledge

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func requireCode(t *testing.T, err error, want ErrorCode) {
	t.Helper()
	code, ok := CodeOf(err)
	if !ok || code != want {
		t.Fatalf("error = %v, want code %s", err, want)
	}
}

func writeSource(t *testing.T, name string, content []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("write source: %v", err)
	}
	return path
}

func TestLoadFileAcceptsSupportedFormats(t *testing.T) {
	cases := []struct {
		name   string
		file   string
		format Format
	}{
		{"markdown", "Guide.MD", FormatMarkdown},
		{"text", "notes.txt", FormatText},
		{"html", "page.html", FormatHTML},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeSource(t, tc.file, []byte("hello"))
			loaded, err := loadFile(path)
			if err != nil {
				t.Fatalf("loadFile: %v", err)
			}
			if loaded.format != tc.format || loaded.kind != SourceKindFile || loaded.uri != path || string(loaded.data) != "hello" {
				t.Fatalf("loaded = %+v", loaded)
			}
		})
	}
}

func TestLoadFileLabelNeverCarriesTheHostPath(t *testing.T) {
	path := writeSource(t, "refund policy [v2].md", []byte("hello"))
	loaded, err := loadFile(path)
	if err != nil {
		t.Fatalf("loadFile: %v", err)
	}
	if loaded.label != "refund-policy-v2-.md" {
		t.Errorf("label = %q", loaded.label)
	}
	if strings.ContainsAny(loaded.label, "[]#<>") || strings.Contains(loaded.label, filepath.Dir(path)) {
		t.Errorf("label = %q leaks structure or path", loaded.label)
	}
}

func TestLoadFileRefusals(t *testing.T) {
	big := bytes.Repeat([]byte("a"), MaxSourceBytes+1)
	cases := []struct {
		name  string
		setup func(t *testing.T) string
		want  ErrorCode
	}{
		{"unsupported extension", func(t *testing.T) string { return writeSource(t, "doc.pdf", []byte("%PDF")) }, ErrorCodeSourceUnsupported},
		{"no extension", func(t *testing.T) string { return writeSource(t, "README", []byte("x")) }, ErrorCodeSourceUnsupported},
		{"htm extension", func(t *testing.T) string { return writeSource(t, "page.htm", []byte("x")) }, ErrorCodeSourceUnsupported},
		{"missing file", func(t *testing.T) string { return filepath.Join(t.TempDir(), "missing.md") }, ErrorCodeSourceNotFound},
		{"directory named like a file", func(t *testing.T) string {
			path := filepath.Join(t.TempDir(), "dir.md")
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
			return path
		}, ErrorCodeSourceNotRegular},
		{"symlink to a regular file", func(t *testing.T) string {
			target := writeSource(t, "real.md", []byte("x"))
			link := filepath.Join(t.TempDir(), "link.md")
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
			return link
		}, ErrorCodeSourceNotRegular},
		{"dangling symlink", func(t *testing.T) string {
			link := filepath.Join(t.TempDir(), "dangling.md")
			if err := os.Symlink(filepath.Join(t.TempDir(), "gone.md"), link); err != nil {
				t.Fatal(err)
			}
			return link
		}, ErrorCodeSourceNotRegular},
		{"oversized", func(t *testing.T) string { return writeSource(t, "big.md", big) }, ErrorCodeSourceTooLarge},
		{"invalid utf8", func(t *testing.T) string { return writeSource(t, "latin.txt", []byte{0x63, 0x61, 0x66, 0xE9}) }, ErrorCodeSourceNotUTF8},
		{"nul bytes", func(t *testing.T) string { return writeSource(t, "binary.txt", []byte("ab\x00cd")) }, ErrorCodeSourceNotUTF8},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadFile(tc.setup(t))
			requireCode(t, err, tc.want)
			if strings.Contains(err.Error(), os.TempDir()) {
				t.Errorf("error %q carries a host path", err.Error())
			}
		})
	}
}

func TestLoadFileAcceptsASourceAtTheSizeLimit(t *testing.T) {
	path := writeSource(t, "limit.txt", bytes.Repeat([]byte("a"), MaxSourceBytes))
	if _, err := loadFile(path); err != nil {
		t.Fatalf("loadFile at the limit: %v", err)
	}
}

func TestSanitizeLabel(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"plain", "guide.md", "guide.md"},
		{"spaces become one separator", "my   refund policy.md", "my-refund-policy.md"},
		{"brackets and hash removed", "a[b]#c<d>.md", "a-b-c-d-.md"},
		{"unicode letters kept", "r" + runeText(0xE9) + "sum" + runeText(0xE9) + ".md", "r" + runeText(0xE9) + "sum" + runeText(0xE9) + ".md"},
		{"invisible characters stripped", "a" + runeText(0x202E, 0x200B) + "b.md", "ab.md"},
		{"newline stripped", "a\nb.md", "a-b.md"},
		{"url shape", "docs.example.com/help/refunds", "docs.example.com/help/refunds"},
		{"empty falls back", "", "source"},
		{"only symbols falls back", "[]#", "source"},
		{"capped", strings.Repeat("a", 500), strings.Repeat("a", maxLabelRunes)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitizeLabel(tc.input); got != tc.want {
				t.Errorf("sanitizeLabel(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

type fakeBody struct {
	reader io.Reader
	closed *bool
}

func (b fakeBody) Read(p []byte) (int, error) { return b.reader.Read(p) }

func (b fakeBody) Close() error {
	*b.closed = true
	return nil
}

type fakeDoer struct {
	status      int
	header      http.Header
	body        []byte
	length      int64
	err         error
	closed      bool
	requestedAs *url.URL
}

func (d *fakeDoer) Do(req *http.Request) (*http.Response, error) {
	d.requestedAs = req.URL
	if d.err != nil {
		return nil, d.err
	}
	status := d.status
	if status == 0 {
		status = http.StatusOK
	}
	length := d.length
	if length == 0 {
		length = int64(len(d.body))
	}
	return &http.Response{
		StatusCode:    status,
		Header:        d.header,
		ContentLength: length,
		Body:          fakeBody{reader: bytes.NewReader(d.body), closed: &d.closed},
	}, nil
}

func contentType(value string) http.Header {
	return http.Header{"Content-Type": []string{value}}
}

func TestLoadURLFormatsAndIdentity(t *testing.T) {
	cases := []struct {
		name   string
		raw    string
		header string
		format Format
		uri    string
		label  string
	}{
		{"html", "https://Docs.Example.com/help/refunds", "text/html; charset=utf-8", FormatHTML, "https://docs.example.com/help/refunds", "docs.example.com/help/refunds"},
		{"xhtml", "https://docs.example.com/a", "application/xhtml+xml", FormatHTML, "https://docs.example.com/a", "docs.example.com/a"},
		{"markdown", "https://docs.example.com/a", "text/markdown", FormatMarkdown, "https://docs.example.com/a", "docs.example.com/a"},
		{"plain text", "https://docs.example.com/a.txt", "text/plain", FormatText, "https://docs.example.com/a.txt", "docs.example.com/a.txt"},
		{"plain text served for a markdown path", "https://docs.example.com/readme.md", "text/plain; charset=UTF-8", FormatMarkdown, "https://docs.example.com/readme.md", "docs.example.com/readme.md"},
		{"query kept in uri but not in label", "https://docs.example.com/a?page=2&lang=en", "text/html", FormatHTML, "https://docs.example.com/a?page=2&lang=en", "docs.example.com/a"},
		{"fragment dropped", "https://docs.example.com/a#intro", "text/html", FormatHTML, "https://docs.example.com/a", "docs.example.com/a"},
		{"surrounding space trimmed", "  https://docs.example.com/a  ", "text/html", FormatHTML, "https://docs.example.com/a", "docs.example.com/a"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doer := &fakeDoer{header: contentType(tc.header), body: []byte("hello")}
			loaded, err := loadURL(t.Context(), doer, tc.raw)
			if err != nil {
				t.Fatalf("loadURL: %v", err)
			}
			if loaded.format != tc.format || loaded.uri != tc.uri || loaded.label != tc.label || loaded.kind != SourceKindURL {
				t.Fatalf("loaded = %+v", loaded)
			}
			if doer.requestedAs == nil || doer.requestedAs.String() != tc.uri {
				t.Errorf("requested %v, want %s", doer.requestedAs, tc.uri)
			}
			if !doer.closed {
				t.Error("response body was not closed")
			}
		})
	}
}

func TestLoadURLRefusals(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want ErrorCode
	}{
		{"plain http", "http://docs.example.com/a", ErrorCodeURLRefused},
		{"ftp", "ftp://docs.example.com/a", ErrorCodeURLRefused},
		{"userinfo", "https://user:pass@docs.example.com/a", ErrorCodeURLRefused},
		{"username only", "https://token@docs.example.com/a", ErrorCodeURLRefused},
		{"no host", "https:///a", ErrorCodeURLRefused},
		{"unparsable", "https://%zz", ErrorCodeURLRefused},
		{"oversized uri", "https://docs.example.com/" + strings.Repeat("a", maxURIBytes), ErrorCodeURLRefused},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doer := &fakeDoer{header: contentType("text/html"), body: []byte("x")}
			_, err := loadURL(t.Context(), doer, tc.raw)
			requireCode(t, err, tc.want)
			if doer.requestedAs != nil {
				t.Error("a refused URL reached the network")
			}
			if strings.Contains(err.Error(), "pass") || strings.Contains(err.Error(), "token@") {
				t.Errorf("error %q prints credentials", err.Error())
			}
		})
	}
}

func TestLoadURLResponseRefusals(t *testing.T) {
	big := bytes.Repeat([]byte("a"), MaxSourceBytes+1)
	cases := []struct {
		name  string
		doer  *fakeDoer
		want  ErrorCode
		check func(t *testing.T, doer *fakeDoer)
	}{
		{"not found", &fakeDoer{status: http.StatusNotFound, header: contentType("text/html"), body: []byte("x")}, ErrorCodeFetchFailed, nil},
		{"redirect status without follow", &fakeDoer{status: http.StatusFound, header: contentType("text/html")}, ErrorCodeFetchFailed, nil},
		{"pdf", &fakeDoer{header: contentType("application/pdf"), body: []byte("%PDF")}, ErrorCodeSourceUnsupported, nil},
		{"json", &fakeDoer{header: contentType("application/json"), body: []byte("{}")}, ErrorCodeSourceUnsupported, nil},
		{"missing content type", &fakeDoer{header: http.Header{}, body: []byte("x")}, ErrorCodeSourceUnsupported, nil},
		{"latin1 charset", &fakeDoer{header: contentType("text/html; charset=iso-8859-1"), body: []byte("x")}, ErrorCodeSourceNotUTF8, nil},
		{"invalid utf8 body", &fakeDoer{header: contentType("text/plain"), body: []byte{0x63, 0xE9}}, ErrorCodeSourceNotUTF8, nil},
		{"compressed", &fakeDoer{header: http.Header{"Content-Type": []string{"text/html"}, "Content-Encoding": []string{"gzip"}}, body: []byte("x")}, ErrorCodeSourceUnsupported, nil},
		{"declared too large", &fakeDoer{header: contentType("text/html"), body: []byte("x"), length: MaxSourceBytes + 1}, ErrorCodeSourceTooLarge, nil},
		{"streamed too large without length", &fakeDoer{header: contentType("text/html"), body: big, length: -1}, ErrorCodeSourceTooLarge, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadURL(t.Context(), tc.doer, "https://docs.example.com/a")
			requireCode(t, err, tc.want)
			if !tc.doer.closed {
				t.Error("response body was not closed")
			}
		})
	}
}

func TestLoadURLTransportErrorsDoNotLeakTheURL(t *testing.T) {
	const raw = "https://docs.example.com/a?token=sekret-value"
	t.Run("plain failure", func(t *testing.T) {
		doer := &fakeDoer{err: &url.Error{Op: "Get", URL: raw, Err: errors.New("connection reset")}}
		_, err := loadURL(t.Context(), doer, raw)
		requireCode(t, err, ErrorCodeFetchFailed)
		if strings.Contains(err.Error(), "sekret-value") {
			t.Errorf("error %q leaks the query", err.Error())
		}
	})
	t.Run("coded refusal wrapped by the client", func(t *testing.T) {
		doer := &fakeDoer{err: &url.Error{Op: "Get", URL: raw, Err: Errorf(ErrorCodeURLRefused, "destination is not permitted")}}
		_, err := loadURL(t.Context(), doer, raw)
		requireCode(t, err, ErrorCodeURLRefused)
		if strings.Contains(err.Error(), "sekret-value") {
			t.Errorf("error %q leaks the query", err.Error())
		}
	})
	t.Run("cancelled context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		doer := &fakeDoer{err: &url.Error{Op: "Get", URL: raw, Err: context.Canceled}}
		_, err := loadURL(ctx, doer, raw)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
	})
}
