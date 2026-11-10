package knowledge

import (
	"slices"
	"strings"
	"testing"
)

func credentialFixtures() map[string]string {
	return map[string]string{
		"private_key_block":         "-----BEGIN " + "RSA PRIVATE KEY-----\nMIIE",
		"aws_access_key_id":         "AKIA" + "ABCDEFGHIJKLMNOP",
		"aws_secret_assignment":     "aws_secret_access_key = " + strings.Repeat("aB3/", 10),
		"github_token":              "gh" + "p_" + strings.Repeat("a1B2", 9),
		"github_fine_grained_token": "github_pat_" + strings.Repeat("a1B2c3", 5),
		"gitlab_token":              "gl" + "pat-" + strings.Repeat("x9", 12),
		"google_api_key":            "AI" + "za" + strings.Repeat("Ab1_", 9)[:35],
		"slack_token":               "xo" + "xb-1234567890-abcdefghij",
		"stripe_live_key":           "sk_" + "live_" + strings.Repeat("Zz9", 6),
		"npm_token":                 "npm" + "_" + strings.Repeat("a1", 18),
		"anthropic_api_key":         "sk-" + "ant-api03-" + strings.Repeat("Ab", 12),
		"azure_storage_account_key": "AccountKey=" + strings.Repeat("Qw1+", 11) + "==",
	}
}

type fakeVault struct {
	values []string
}

func (v fakeVault) Contains(text string) bool {
	return slices.ContainsFunc(v.values, func(value string) bool { return strings.Contains(text, value) })
}

func TestEveryCredentialPatternHasAFixtureAndIsDetected(t *testing.T) {
	fixtures := credentialFixtures()
	names := CredentialPatternNames()
	if len(names) != len(fixtures) {
		t.Fatalf("patterns = %d, fixtures = %d; every named pattern needs exactly one fixture", len(names), len(fixtures))
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			fixture, ok := fixtures[name]
			if !ok {
				t.Fatalf("pattern %s has no fixture", name)
			}
			err := screenText("Some documentation text.\n"+fixture+"\nMore text.", nil)
			if code, coded := CodeOf(err); !coded || code != ErrorCodeSecretInvalid {
				t.Fatalf("screenText error = %v, want %s", err, ErrorCodeSecretInvalid)
			}
			if strings.Contains(err.Error(), fixture) {
				t.Error("error message prints the matched credential")
			}
			if !strings.Contains(err.Error(), name) {
				t.Errorf("error message %q does not name the pattern", err.Error())
			}
		})
	}
}

func TestCredentialPatternNamesAreUnique(t *testing.T) {
	names := CredentialPatternNames()
	seen := map[string]bool{}
	for _, name := range names {
		if seen[name] {
			t.Errorf("duplicate pattern name %s", name)
		}
		seen[name] = true
	}
}

func TestScreenTextAcceptsOrdinaryDocumentation(t *testing.T) {
	cases := []struct {
		name string
		text string
	}{
		{"prose", "Refunds are issued within thirty days. Contact support for help."},
		{"short github prefix", "tokens look like ghp_ followed by characters"},
		{"public key block", "-----BEGIN PUBLIC KEY-----\nabc\n-----END PUBLIC KEY-----"},
		{"kebab case slug", "sk-learn-compatible-estimators-are-documented-here-in-detail"},
		{"aws key prefix alone", "AKIA is the prefix of access key identifiers"},
		{"empty", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := screenText(tc.text, fakeVault{values: []string{"vault-secret-value"}}); err != nil {
				t.Fatalf("screenText error = %v", err)
			}
		})
	}
}

func TestScreenTextRejectsVaultValuesWithoutPrintingThem(t *testing.T) {
	const secretValue = "hunter2-vault-value"
	err := screenText("The password is "+secretValue+" for the admin panel.", fakeVault{values: []string{secretValue}})
	if code, coded := CodeOf(err); !coded || code != ErrorCodeSecretInvalid {
		t.Fatalf("error = %v, want %s", err, ErrorCodeSecretInvalid)
	}
	if strings.Contains(err.Error(), secretValue) {
		t.Error("error message prints the configured secret")
	}
}
