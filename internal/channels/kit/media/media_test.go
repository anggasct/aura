package media

import (
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"testing/iotest"
)

func TestEligible(t *testing.T) {
	cases := map[string]bool{
		"image/png":                  true,
		"image/jpeg; charset=binary": true,
		"AUDIO/OGG":                  true,
		" video/mp4 ":                true,
		"application/pdf":            true,
		"Application/PDF; x=y":       true,
		"text/html":                  false,
		"application/octet-stream":   false,
		"application/zip":            false,
		"":                           false,
		"imagepng":                   false,
		"application/pdfx":           false,
		"text/plain; image/png":      false,
	}
	for input, want := range cases {
		t.Run(input, func(t *testing.T) {
			if got := Eligible(input); got != want {
				t.Errorf("Eligible(%q) = %v, want %v", input, got, want)
			}
		})
	}
}

func TestIsVisionImage(t *testing.T) {
	cases := map[string]bool{
		"image/png":       true,
		"image/jpeg":      true,
		"IMAGE/WEBP":      true,
		"image/gif; q=1":  true,
		"image/svg+xml":   false,
		"image/heic":      false,
		"application/pdf": false,
		"":                false,
	}
	for input, want := range cases {
		t.Run(input, func(t *testing.T) {
			if got := IsVisionImage(input); got != want {
				t.Errorf("IsVisionImage(%q) = %v, want %v", input, got, want)
			}
		})
	}
}

func TestSanitizeFilename(t *testing.T) {
	cases := map[string]string{
		"cat.png":                "cat.png",
		"../../etc/passwd":       "passwd",
		"":                       "attachment",
		"   ":                    "attachment",
		"a\x00b.png":             "ab.png",
		strings.Repeat("n", 200): strings.Repeat("n", 128),
	}
	for input, want := range cases {
		if got := SanitizeFilename(input); got != want {
			t.Errorf("SanitizeFilename(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestSpool(t *testing.T) {
	cases := []struct {
		name     string
		body     io.Reader
		limit    int64
		wantCode ErrorCode
		wantBody string
	}{
		{name: "within the bound", body: strings.NewReader("hello"), limit: 10, wantBody: "hello"},
		{name: "exactly the bound", body: strings.NewReader("hello"), limit: 5, wantBody: "hello"},
		{name: "one byte over", body: strings.NewReader("hello!"), limit: 5, wantCode: ErrorCodeSizeExceeded},
		{name: "far over", body: strings.NewReader(strings.Repeat("x", 4096)), limit: 5, wantCode: ErrorCodeSizeExceeded},
		{name: "read failure", body: io.MultiReader(strings.NewReader("par"), iotest.ErrReader(errors.New("reset"))), limit: 10, wantCode: ErrorCodeIncomplete},
		{name: "cut-off body", body: iotest.TimeoutReader(strings.NewReader("abcdef")), limit: 10, wantCode: ErrorCodeIncomplete},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			scratch := t.TempDir()
			t.Setenv("TMPDIR", scratch)
			file, err := Spool(tc.body, tc.limit)
			if tc.wantCode != "" {
				if code, ok := CodeOf(err); !ok || code != tc.wantCode {
					t.Fatalf("Spool error = %v, want code %q", err, tc.wantCode)
				}
				if file != nil {
					t.Error("Spool returned a file alongside an error")
				}
				assertEmpty(t, scratch)
				return
			}
			if err != nil {
				t.Fatalf("Spool: %v", err)
			}
			got, err := io.ReadAll(file)
			if err != nil || string(got) != tc.wantBody {
				t.Errorf("spooled = %q (%v), want %q from the start", got, err, tc.wantBody)
			}
			Discard(file)
			assertEmpty(t, scratch)
		})
	}
}

func TestDiscardIgnoresNil(t *testing.T) {
	Discard(nil)
}

func assertEmpty(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("temp files left behind: %v", entries)
	}
}
