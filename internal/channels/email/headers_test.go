package email

import (
	"strings"
	"testing"
)

func TestSenderOf(t *testing.T) {
	cases := map[string]struct {
		lines []string
		want  string
		ok    bool
	}{
		"plain address":          {lines: []string{"From: owner@example.com"}, want: "owner@example.com", ok: true},
		"display name":           {lines: []string{"From: The Owner <owner@example.com>"}, want: "owner@example.com", ok: true},
		"quoted display name":    {lines: []string{`From: "Owner, The" <owner@example.com>`}, want: "owner@example.com", ok: true},
		"encoded display name":   {lines: []string{"From: =?UTF-8?Q?M=C3=BCller?= <owner@example.com>"}, want: "owner@example.com", ok: true},
		"uppercase address":      {lines: []string{"From: Owner@Example.COM"}, want: "owner@example.com", ok: true},
		"two addresses":          {lines: []string{"From: a@example.com, b@example.com"}},
		"two from headers":       {lines: []string{"From: a@example.com", "From: b@example.com"}},
		"same address twice":     {lines: []string{"From: a@example.com", "From: a@example.com"}},
		"missing from":           {lines: []string{"To: a@example.com"}},
		"empty from":             {lines: []string{"From:"}},
		"unparsable from":        {lines: []string{"From: not an address"}},
		"address without domain": {lines: []string{"From: owner"}},
		"empty group":            {lines: []string{"From: Team: ;"}},
		"group of one":           {lines: []string{"From: Team: a@example.com;"}},
		"quoted at sign":         {lines: []string{`From: "a@b"@example.com`}},
		"angle brackets only":    {lines: []string{"From: <>"}},
		"control character":      {lines: []string{"From: a\x00@example.com"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			header := headerFrom(t, tc.lines...)
			got, ok := senderOf(header)
			if ok != tc.ok || got != tc.want {
				t.Fatalf("senderOf() = %q, %v; want %q, %v", got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestIsAutomated(t *testing.T) {
	cases := map[string]struct {
		lines []string
		want  bool
	}{
		"ordinary mail":              {lines: []string{"From: a@example.com"}},
		"auto-submitted no":          {lines: []string{"Auto-Submitted: no"}},
		"auto-submitted no cased":    {lines: []string{"Auto-Submitted: No"}},
		"auto-replied":               {lines: []string{"Auto-Submitted: auto-replied"}},
		"auto-generated":             {lines: []string{"Auto-Submitted: auto-generated"}},
		"auto-notified":              {lines: []string{"Auto-Submitted: auto-notified"}},
		"auto-submitted with params": {lines: []string{"Auto-Submitted: auto-replied; owner=a@example.com"}},
		"no with a parameter":        {lines: []string{"Auto-Submitted: no; x=1"}},
		"empty auto-submitted":       {lines: []string{"Auto-Submitted:"}},
		"no then auto-replied":       {lines: []string{"Auto-Submitted: no", "Auto-Submitted: auto-replied"}, want: true},
		"list id":                    {lines: []string{"List-Id: <team.example.com>"}, want: true},
		"empty list id":              {lines: []string{"List-Id:"}, want: true},
		"precedence bulk":            {lines: []string{"Precedence: bulk"}, want: true},
		"precedence list":            {lines: []string{"Precedence: list"}, want: true},
		"precedence junk":            {lines: []string{"Precedence: Junk"}, want: true},
		"precedence padded":          {lines: []string{"Precedence:   BULK  "}, want: true},
		"precedence first-class":     {lines: []string{"Precedence: first-class"}},
	}
	want := map[string]bool{"auto-replied": true, "auto-generated": true, "auto-notified": true, "auto-submitted with params": true, "no with a parameter": false, "empty auto-submitted": true}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			expected := tc.want
			if value, ok := want[name]; ok {
				expected = value
			}
			if got := isAutomated(headerFrom(t, tc.lines...)); got != expected {
				t.Fatalf("isAutomated() = %v, want %v", got, expected)
			}
		})
	}
}

func TestMessageIDOf(t *testing.T) {
	cases := map[string]struct {
		lines []string
		want  string
		ok    bool
	}{
		"ordinary id":     {lines: []string{"Message-ID: <abc.123@example.com>"}, want: "abc.123@example.com", ok: true},
		"missing":         {lines: []string{"From: a@example.com"}},
		"empty":           {lines: []string{"Message-ID:"}},
		"no brackets":     {lines: []string{"Message-ID: abc@example.com"}},
		"empty brackets":  {lines: []string{"Message-ID: <>"}},
		"two ids":         {lines: []string{"Message-ID: <a@example.com> <b@example.com>"}},
		"too long":        {lines: []string{"Message-ID: <" + strings.Repeat("a", 1000) + "@example.com>"}},
		"id without host": {lines: []string{"Message-ID: <abc>"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, ok := messageIDOf(headerFrom(t, tc.lines...))
			if ok != tc.ok || got != tc.want {
				t.Fatalf("messageIDOf() = %q, %v; want %q, %v", got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestThreadRoot(t *testing.T) {
	cases := map[string]struct {
		lines []string
		want  string
	}{
		"new thread":                 {lines: []string{"Message-ID: <c@example.com>"}, want: "c@example.com"},
		"first reference":            {lines: []string{"Message-ID: <c@example.com>", "References: <a@example.com> <b@example.com>"}, want: "a@example.com"},
		"reference over reply to":    {lines: []string{"Message-ID: <c@example.com>", "References: <a@example.com>", "In-Reply-To: <b@example.com>"}, want: "a@example.com"},
		"reply to without reference": {lines: []string{"Message-ID: <c@example.com>", "In-Reply-To: <b@example.com>"}, want: "b@example.com"},
		"malformed references":       {lines: []string{"Message-ID: <c@example.com>", "References: a@example.com"}, want: "c@example.com"},
		"malformed then reply to":    {lines: []string{"Message-ID: <c@example.com>", "References: junk", "In-Reply-To: <b@example.com>"}, want: "b@example.com"},
		"empty references":           {lines: []string{"Message-ID: <c@example.com>", "References:"}, want: "c@example.com"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			header := headerFrom(t, tc.lines...)
			id, ok := messageIDOf(header)
			if !ok {
				t.Fatal("fixture has no Message-ID")
			}
			if got := threadRoot(header, id); got != tc.want {
				t.Fatalf("threadRoot() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSubjectOf(t *testing.T) {
	cases := map[string]struct {
		line string
		want string
	}{
		"plain":        {line: "Subject: Quarterly report", want: "Quarterly report"},
		"encoded word": {line: "Subject: =?UTF-8?Q?Caf=C3=A9?= menu", want: "Café menu"},
		"empty":        {line: "Subject:", want: ""},
		"padded":       {line: "Subject:   many    spaces  ", want: "many spaces"},
		"too long":     {line: "Subject: " + strings.Repeat("a", 2000), want: strings.Repeat("a", maxSubjectBytes)},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := subjectOf(headerFrom(t, tc.line)); got != tc.want {
				t.Fatalf("subjectOf() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDigestOfIsStableAndSeparatesInputs(t *testing.T) {
	first := digestOf("a@example.com")
	if first != digestOf("a@example.com") || len(first) != digestChars {
		t.Fatalf("digest = %q, want a stable %d characters", first, digestChars)
	}
	if first == digestOf("b@example.com") {
		t.Fatal("different inputs share a digest")
	}
	if strings.ToLower(first) != first || strings.ContainsAny(first, "=/+") {
		t.Errorf("digest %q is not a lowercase base32 string", first)
	}
}
