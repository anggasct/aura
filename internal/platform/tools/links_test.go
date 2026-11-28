package tools

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func TestLinkMatchCanonicalizesOnlySchemeHostPortAndFragment(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
		ok   bool
	}{
		{name: "scheme and host case", raw: "HTTPS://Example.COM/Path/Case?Key=Val", want: "https://example.com/Path/Case?Key=Val", ok: true},
		{name: "default https port removed", raw: "https://example.com:443/a", want: "https://example.com/a", ok: true},
		{name: "default http port removed", raw: "http://example.com:80/a", want: "http://example.com/a", ok: true},
		{name: "non default port kept", raw: "https://example.com:8443/a", want: "https://example.com:8443/a", ok: true},
		{name: "empty path becomes slash", raw: "https://example.com", want: "https://example.com/", ok: true},
		{name: "empty path with query", raw: "https://example.com?a=1&b=2", want: "https://example.com/?a=1&b=2", ok: true},
		{name: "fragment dropped", raw: "https://example.com/doc#section-2", want: "https://example.com/doc", ok: true},
		{name: "idna punycode host", raw: "https://bücher.example/Über?q=%2f%2F", want: "https://xn--bcher-kva.example/Über?q=%2f%2F", ok: true},
		{name: "percent encoding byte exact", raw: "https://example.com/%61/%41?k=%20&v=%2f", want: "https://example.com/%61/%41?k=%20&v=%2f", ok: true},
		{name: "dot segments not collapsed", raw: "https://example.com/a/../b", want: "https://example.com/a/../b", ok: true},
		{name: "query order preserved", raw: "https://example.com/p?b=2&a=1", want: "https://example.com/p?b=2&a=1", ok: true},
		{name: "userinfo rejected", raw: "https://user:pass@example.com/p", ok: false},
		{name: "non http scheme rejected", raw: "ftp://example.com/file", ok: false},
		{name: "over 2048 bytes rejected", raw: "https://example.com/" + strings.Repeat("a", MaxMatchURLBytes), ok: false},
		{name: "exact 2048 bytes accepted", raw: "https://example.com/" + strings.Repeat("a", MaxMatchURLBytes-len("https://example.com/")), want: "https://example.com/" + strings.Repeat("a", MaxMatchURLBytes-len("https://example.com/")), ok: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := CanonicalURL(tc.raw)
			if ok != tc.ok || got != tc.want {
				t.Errorf("CanonicalURL(%q) = %q, %v; want %q, %v", tc.raw, got, ok, tc.want, tc.ok)
			}
		})
	}

	a, okA := CanonicalURL("https://example.com/Path?a=1")
	b, okB := CanonicalURL("https://example.com/path?a=1")
	if !okA || !okB || a == b {
		t.Errorf("path case must stay distinct: %q vs %q", a, b)
	}

	extracted := ExtractURLs("Check (https://Example.COM:443/docs/v1?x=1#top.), <http://bücher.example:80/p>, and `https://example.org/q;`!")
	wantExtracted := []string{
		"https://example.com/docs/v1?x=1",
		"http://xn--bcher-kva.example/p",
		"https://example.org/q",
	}
	if !slices.Equal(extracted, wantExtracted) {
		t.Errorf("ExtractURLs = %v, want %v", extracted, wantExtracted)
	}

	dottedI := ExtractURLs(strings.Repeat("İ", 30) + " https://example.com/a " + strings.Repeat("İ", 10) + " HTTP://Example.ORG/b")
	if !slices.Equal(dottedI, []string{"https://example.com/a", "http://example.org/b"}) {
		t.Errorf("ExtractURLs with U+0130 = %v", dottedI)
	}
	kelvin := ExtractURLs(strings.Repeat("\u212a", 10) + " https://a.co/1 " + strings.Repeat("\u212a", 5) + " https://b.co/2")
	if !slices.Equal(kelvin, []string{"https://a.co/1", "https://b.co/2"}) {
		t.Errorf("ExtractURLs with U+212A = %v", kelvin)
	}
}

func TestTaintedGrantOperationValidation(t *testing.T) {
	base := func(risk Risk, grant TaintedGrant) Definition {
		d := Definition{
			Name: "aura_probe", Version: Version, Operation: "probe.run", Risk: risk, TaintedGrant: grant,
			Schema: json.RawMessage(`{"type":"object"}`),
			Validator: func(raw json.RawMessage) (json.RawMessage, error) {
				return raw, nil
			},
		}
		if err := d.ApplyOperationFlags(); err != nil {
			t.Fatalf("ApplyOperationFlags: %v", err)
		}
		return d
	}
	for _, grant := range []TaintedGrant{"", TaintedGrantLabels, TaintedGrantInert} {
		def := base(RiskTrustGrant, grant)
		if err := def.ValidateOperation(); err != nil {
			t.Errorf("trust grant with TaintedGrant=%q failed validation: %v", grant, err)
		}
	}
	badRisk := base(RiskChange, TaintedGrantLabels)
	if err := badRisk.ValidateOperation(); err == nil {
		t.Error("change with TaintedGrant passed validation")
	}
	badMode := base(RiskTrustGrant, "unknown")
	if err := badMode.ValidateOperation(); err == nil {
		t.Error("trust grant with unknown TaintedGrant passed validation")
	}
	noOp := Definition{Name: "plain", Version: Version, TaintedGrant: TaintedGrantLabels}
	if err := noOp.ValidateOperation(); err == nil {
		t.Error("non-operation with TaintedGrant passed validation")
	}
}
