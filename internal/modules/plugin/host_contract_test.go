package plugin

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"
	"testing"
)

func mailTool(display string) string {
	return `api_version: aura.plugin/v1
id: mailer
version: 1.0.0
name: Mailer
description: Mail access.
capabilities: [network]
tools:
  - name: send
    description: Send mail.
    tier: effect
` + display + `egress: [api.mail.example]
secrets: []
auth:
  kind: none
backend:
  kind: mcp
  transport: streamable_http
  url: https://api.mail.example/mcp
`
}

const displayLine = "    display: [to, cc, subject, body]\n"

func TestOAuth2LoginFieldsValidate(t *testing.T) {
	accepted := []struct {
		name string
		edit func(t *testing.T) string
		want func(spec *Auth) bool
	}{
		{"portless loopback redirect", func(t *testing.T) string {
			return replaceOnce(t, oauthManifest, "http://127.0.0.1:8765/callback", "http://127.0.0.1/callback")
		}, nil},
		{"authorize params", func(t *testing.T) string {
			return replaceOnce(t, oauthManifest, "  scopes:", "  authorize_params:\n    access_type: offline\n    prompt: consent\n  scopes:")
		}, func(a *Auth) bool {
			return a.AuthorizeParams["access_type"] == "offline" && a.AuthorizeParams["prompt"] == "consent"
		}},
		{"access only", func(t *testing.T) string {
			return replaceOnce(t, oauthManifest, "  scopes:", "  access_only: true\n  scopes:")
		}, func(a *Auth) bool { return a.AccessOnly }},
		{"issuer", func(t *testing.T) string {
			return replaceOnce(t, oauthManifest, "  scopes:", "  issuer: https://accounts.mail.example\n  scopes:")
		}, func(a *Auth) bool { return a.Issuer == "https://accounts.mail.example" }},
	}
	for _, tc := range accepted {
		t.Run("accepts "+tc.name, func(t *testing.T) {
			manifest, err := ParseManifest([]byte(tc.edit(t)))
			if err != nil {
				t.Fatalf("ParseManifest: %v", err)
			}
			if tc.want != nil && !tc.want(&manifest.Auth) {
				t.Fatalf("auth = %+v", manifest.Auth)
			}
			spec := manifest.AuthSpec("fp")
			if spec.Issuer != manifest.Auth.Issuer || spec.AccessOnly != manifest.Auth.AccessOnly || len(spec.AuthorizeParams) != len(manifest.Auth.AuthorizeParams) {
				t.Fatalf("auth spec = %+v does not carry the manifest fields", spec)
			}
		})
	}
	rejected := []struct {
		name string
		edit func(t *testing.T) string
	}{
		{"unknown authorize param", func(t *testing.T) string {
			return replaceOnce(t, oauthManifest, "  scopes:", "  authorize_params:\n    login_hint: a@b.example\n  scopes:")
		}},
		{"redirect override param", func(t *testing.T) string {
			return replaceOnce(t, oauthManifest, "  scopes:", "  authorize_params:\n    redirect_uri: https://evil.example/cb\n  scopes:")
		}},
		{"unknown access type", func(t *testing.T) string {
			return replaceOnce(t, oauthManifest, "  scopes:", "  authorize_params:\n    access_type: forever\n  scopes:")
		}},
		{"unknown prompt", func(t *testing.T) string {
			return replaceOnce(t, oauthManifest, "  scopes:", "  authorize_params:\n    prompt: login\n  scopes:")
		}},
		{"issuer over http", func(t *testing.T) string {
			return replaceOnce(t, oauthManifest, "  scopes:", "  issuer: http://accounts.mail.example\n  scopes:")
		}},
		{"issuer with a query", func(t *testing.T) string {
			return replaceOnce(t, oauthManifest, "  scopes:", "  issuer: https://accounts.mail.example/?a=b\n  scopes:")
		}},
		{"issuer with credentials", func(t *testing.T) string {
			return replaceOnce(t, oauthManifest, "  scopes:", "  issuer: https://user:pw@accounts.mail.example\n  scopes:")
		}},
		{"authorize params on none", func(t *testing.T) string {
			return replaceOnce(t, baseManifest, "kind: none", "kind: none\n  authorize_params:\n    prompt: consent")
		}},
		{"access only on none", func(t *testing.T) string {
			return replaceOnce(t, baseManifest, "kind: none", "kind: none\n  access_only: true")
		}},
		{"issuer on none", func(t *testing.T) string {
			return replaceOnce(t, baseManifest, "kind: none", "kind: none\n  issuer: https://accounts.mail.example")
		}},
		{"empty port after the colon", func(t *testing.T) string {
			return replaceOnce(t, oauthManifest, "http://127.0.0.1:8765/callback", "http://127.0.0.1:/callback")
		}},
	}
	for _, tc := range rejected {
		t.Run("rejects "+tc.name, func(t *testing.T) {
			if _, err := ParseManifest([]byte(tc.edit(t))); !hasCode(err, ErrManifestInvalid) {
				t.Fatalf("ParseManifest error = %v, want %s", err, ErrManifestInvalid)
			}
		})
	}
}

func TestLoginFieldChangesQuarantineAndInvalidateTheToken(t *testing.T) {
	cases := []struct {
		name string
		edit func(t *testing.T, text string) string
	}{
		{"authorize params added", func(t *testing.T, text string) string {
			return replaceOnce(t, text, "  scopes:", "  authorize_params:\n    access_type: offline\n  scopes:")
		}},
		{"access only added", func(t *testing.T, text string) string {
			return replaceOnce(t, text, "  scopes:", "  access_only: true\n  scopes:")
		}},
		{"issuer added", func(t *testing.T, text string) string {
			return replaceOnce(t, text, "  scopes:", "  issuer: https://accounts.mail.example\n  scopes:")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t)
			env.approve("mailer", env.install(oauthManifest, nil))
			first := authSurface(t, env, "mailer", "1.0.0")
			env.install(tc.edit(t, bump(t, oauthManifest, "1.1.0")), nil)
			record, err := env.registry.Get(t.Context(), "mailer")
			if err != nil || record.State != StateQuarantined {
				t.Fatalf("record = %+v, %v; want quarantined", record, err)
			}
			if authSurface(t, env, "mailer", "1.1.0") == first {
				t.Fatal("the auth fingerprint did not change")
			}
		})
	}
}

func TestManifestsWithoutTheNewFieldsKeepTheirCanonicalFormAndFingerprints(t *testing.T) {
	manifest, err := ParseManifest([]byte(baseManifest))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	canonical, err := manifest.Canonical()
	if err != nil {
		t.Fatalf("Canonical: %v", err)
	}
	for _, absent := range []string{`"display"`, `"authorize_params"`, `"access_only"`, `"issuer"`} {
		if strings.Contains(string(canonical), absent) {
			t.Fatalf("canonical manifest carries %s: %s", absent, canonical)
		}
	}
	tool := &manifest.Tools[0]
	sum := sha256.Sum256([]byte("tool\x00" + tool.Name + "\x00" + tool.Tier + "\x00" + tool.Description))
	if got := ToolFingerprint(tool); got != hex.EncodeToString(sum[:]) {
		t.Fatalf("tool fingerprint = %s, want the pre-display form %x", got, sum)
	}
}

func TestToolDisplayValidation(t *testing.T) {
	cases := []struct {
		name    string
		display string
		valid   bool
	}{
		{"none", "", true},
		{"ordered names", displayLine, true},
		{"single name", "    display: [to]\n", true},
		{"empty list", "    display: []\n", true},
		{"duplicate", "    display: [to, cc, to]\n", false},
		{"dotted path", "    display: [message.to]\n", false},
		{"index path", "    display: ['to[0]']\n", false},
		{"leading digit", "    display: ['1to']\n", false},
		{"empty name", "    display: ['']\n", false},
		{"space", "    display: ['to cc']\n", false},
		{"too long", "    display: [" + strings.Repeat("a", 65) + "]\n", false},
		{"too many", "    display: [" + strings.Join([]string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k", "l", "m", "n", "o", "p", "q"}, ",") + "]\n", false},
		{"not a list", "    display: to\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseManifest([]byte(mailTool(tc.display)))
			if tc.valid && err != nil {
				t.Fatalf("ParseManifest: %v", err)
			}
			if !tc.valid && !hasCode(err, ErrManifestInvalid) {
				t.Fatalf("ParseManifest error = %v, want %s", err, ErrManifestInvalid)
			}
		})
	}
}

func TestDisplayChangesWidenTheToolAndNothingElse(t *testing.T) {
	approved := mustSurface(t, mailTool(displayLine), "bundle")
	cases := []struct {
		name      string
		candidate string
		widened   bool
	}{
		{"identical", mailTool(displayLine), false},
		{"reordered", mailTool("    display: [cc, to, subject, body]\n"), true},
		{"field dropped", mailTool("    display: [to, cc, subject]\n"), true},
		{"field added", mailTool("    display: [to, cc, subject, body, bcc]\n"), true},
		{"display removed", mailTool(""), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			candidate := mustSurface(t, tc.candidate, "bundle")
			diff := Compare(&approved, &candidate)
			if diff.Widens() != tc.widened || diff.Quarantines() || len(diff.Removed) != 0 {
				t.Fatalf("diff = %+v", diff)
			}
			if tc.widened {
				want := []Change{{Surface: SurfaceTool, Value: "send", Kind: ChangeChanged, Detail: DetailDisplayChanged}}
				if !slices.Equal(diff.Widened, want) {
					t.Fatalf("widened = %+v, want %+v", diff.Widened, want)
				}
			}
		})
	}
	plain := mustSurface(t, mailTool(""), "bundle")
	added := mustSurface(t, mailTool(displayLine), "bundle")
	if diff := Compare(&plain, &added); !diff.Widens() {
		t.Fatalf("adding a display to an approved tool = %+v, want widening", diff)
	}
}

func TestDisplayReachesReviewAndTheRuntimePlan(t *testing.T) {
	env := newTestEnv(t)
	env.approve("mailer", env.install(mailTool(displayLine), nil))
	review, err := env.service.Review(t.Context(), "mailer")
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if len(review.Tools) != 1 || !slices.Equal(review.Tools[0].Display, []string{"to", "cc", "subject", "body"}) {
		t.Fatalf("review tools = %+v", review.Tools)
	}
	plan, err := env.service.Plan(t.Context())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(plan.Active) != 1 || len(plan.Active[0].Tools) != 1 || !slices.Equal(plan.Active[0].Tools[0].Display, []string{"to", "cc", "subject", "body"}) {
		t.Fatalf("plan = %+v", plan)
	}
}

func TestAnApprovedToolStaysApprovedWhenADisplaylessUpgradeKeepsItsFingerprint(t *testing.T) {
	env := newTestEnv(t)
	env.approve("mailer", env.install(mailTool(""), nil))
	result := env.install(bump(t, mailTool(""), "1.0.1"), nil)
	if len(result.Diff.Widened) != 0 || result.Diff.Quarantines() {
		t.Fatalf("diff = %+v, want no widening", result.Diff)
	}
}
