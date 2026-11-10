package httpchat

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"testing"
)

var keyShape = regexp.MustCompile(`^aura_sk_[a-z2-7]{12}_[a-z2-7]{52}$`)

func TestNewKeyShapeAndDigest(t *testing.T) {
	seen := map[string]bool{}
	for range 64 {
		issued, err := NewKey()
		if err != nil {
			t.Fatalf("NewKey: %v", err)
		}
		if !keyShape.MatchString(issued.Plaintext) {
			t.Fatalf("plaintext %q does not match the key format", issued.Plaintext)
		}
		if issued.Digest != KeyDigest(issued.Plaintext) || len(issued.Digest) != 64 {
			t.Fatalf("digest %q does not hash the plaintext", issued.Digest)
		}
		id, err := ParseKey(issued.Plaintext)
		if err != nil || id != issued.ID || len(id) != KeyIDLength {
			t.Fatalf("ParseKey = %q, %v; want %q", id, err, issued.ID)
		}
		seen[issued.Plaintext] = true
	}
	if len(seen) != 64 {
		t.Fatalf("minted %d distinct keys of 64", len(seen))
	}
}

func TestIssueKeyFailsClosedOnRandomSourceError(t *testing.T) {
	cases := map[string]int{"id read fails": 0, "secret read fails": keyIDBytes}
	for name, readable := range cases {
		t.Run(name, func(t *testing.T) {
			source := &limitedReader{remaining: readable}
			issued, err := issueKey(source)
			if code, ok := CodeOf(err); !ok || code != ErrorCodeKeyGeneration {
				t.Fatalf("issueKey error = %v, want %s", err, ErrorCodeKeyGeneration)
			}
			if issued.Plaintext != "" || issued.Digest != "" {
				t.Fatalf("issueKey returned key material on failure: %+v", issued)
			}
			if !errors.Is(err, errEntropy) {
				t.Fatalf("issueKey error %v lost its cause", err)
			}
		})
	}
}

var errEntropy = errors.New("entropy exhausted")

type limitedReader struct {
	remaining int
}

func (r *limitedReader) Read(p []byte) (int, error) {
	if r.remaining <= 0 {
		return 0, errEntropy
	}
	n := min(len(p), r.remaining)
	for i := range n {
		p[i] = byte(i + 1)
	}
	r.remaining -= n
	return n, nil
}

func TestParseKeyIsStrict(t *testing.T) {
	valid := "aura_sk_" + strings.Repeat("a", 12) + "_" + strings.Repeat("b", 52)
	if id, err := ParseKey(valid); err != nil || id != strings.Repeat("a", 12) {
		t.Fatalf("ParseKey(valid) = %q, %v", id, err)
	}
	cases := map[string]string{
		"empty":              "",
		"prefix only":        "aura_sk_",
		"wrong prefix":       "aura_pk_" + strings.Repeat("a", 12) + "_" + strings.Repeat("b", 52),
		"no prefix":          strings.Repeat("a", 12) + "_" + strings.Repeat("b", 52),
		"short id":           "aura_sk_" + strings.Repeat("a", 11) + "_" + strings.Repeat("b", 52),
		"long id":            "aura_sk_" + strings.Repeat("a", 13) + "_" + strings.Repeat("b", 52),
		"short secret":       "aura_sk_" + strings.Repeat("a", 12) + "_" + strings.Repeat("b", 51),
		"long secret":        "aura_sk_" + strings.Repeat("a", 12) + "_" + strings.Repeat("b", 53),
		"missing separator":  "aura_sk_" + strings.Repeat("a", 12) + "-" + strings.Repeat("b", 52),
		"uppercase id":       "aura_sk_" + strings.Repeat("A", 12) + "_" + strings.Repeat("b", 52),
		"uppercase secret":   "aura_sk_" + strings.Repeat("a", 12) + "_" + strings.Repeat("B", 52),
		"digit one in id":    "aura_sk_" + strings.Repeat("a", 11) + "1_" + strings.Repeat("b", 52),
		"digit eight":        "aura_sk_" + strings.Repeat("a", 12) + "_" + strings.Repeat("b", 51) + "8",
		"padding character":  "aura_sk_" + strings.Repeat("a", 12) + "_" + strings.Repeat("b", 51) + "=",
		"trailing space":     valid + " ",
		"leading space":      " " + valid,
		"trailing newline":   valid + "\n",
		"embedded separator": "aura_sk_" + strings.Repeat("a", 5) + "_" + strings.Repeat("a", 6) + "_" + strings.Repeat("b", 52),
		"nul byte":           "aura_sk_" + strings.Repeat("a", 12) + "_" + strings.Repeat("b", 51) + "\x00",
		"multibyte":          "aura_sk_" + strings.Repeat("a", 12) + "_" + strings.Repeat("b", 49) + "é",
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			id, err := ParseKey(raw)
			if code, ok := CodeOf(err); !ok || code != ErrorCodeKeyMalformed {
				t.Fatalf("ParseKey error = %v, want %s", err, ErrorCodeKeyMalformed)
			}
			if id != "" {
				t.Errorf("ParseKey returned id %q for a malformed key", id)
			}
		})
	}
}

func TestVerifyKey(t *testing.T) {
	issued, err := NewKey()
	if err != nil {
		t.Fatalf("NewKey: %v", err)
	}
	other, err := NewKey()
	if err != nil {
		t.Fatalf("NewKey: %v", err)
	}
	cases := map[string]struct {
		presented, digest string
		want              bool
	}{
		"matching key":       {presented: issued.Plaintext, digest: issued.Digest, want: true},
		"other key":          {presented: other.Plaintext, digest: issued.Digest},
		"same id wrong tail": {presented: issued.Plaintext[:len(issued.Plaintext)-1] + "z", digest: issued.Digest},
		"empty digest":       {presented: issued.Plaintext},
		"short digest":       {presented: issued.Plaintext, digest: issued.Digest[:63]},
		"uppercase digest":   {presented: issued.Plaintext, digest: strings.ToUpper(issued.Digest)},
		"empty presented":    {digest: issued.Digest},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := VerifyKey(tc.presented, tc.digest); got != tc.want {
				t.Fatalf("VerifyKey = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestIssuedKeyNeverPrintsItsSecret(t *testing.T) {
	issued, err := NewKey()
	if err != nil {
		t.Fatalf("NewKey: %v", err)
	}
	secret := issued.Plaintext[len(KeyPrefix)+KeyIDLength+1:]
	var logged bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logged, nil))
	logger.InfoContext(t.Context(), "minted", "key", issued)
	logger.InfoContext(t.Context(), "minted", "key", &issued)
	for _, rendered := range []string{
		logged.String(),
		issued.String(),
		fmt.Sprintf("%+v", issued),
	} {
		if strings.Contains(rendered, secret) || strings.Contains(rendered, issued.Plaintext) {
			t.Fatalf("rendering leaks the key secret: %q", rendered)
		}
		if !strings.Contains(rendered, issued.ID) {
			t.Errorf("rendering %q lost the key id", rendered)
		}
	}
}
