package secret

import (
	"strings"
	"testing"
)

func TestMatchSensitiveDetectsCredentialPatternsAndVaultValues(t *testing.T) {
	vault := New()
	vault.Set("api_key", "super-secret-canary-42")

	if pattern, ok := MatchSensitive("normal sentence without secrets", vault); ok || pattern != "" {
		t.Fatalf("MatchSensitive(clean) = (%q, %t), want (\"\", false)", pattern, ok)
	}
	if pattern, ok := MatchSensitive("token is super-secret-canary-42 here", vault); !ok || pattern != "configured_secret" {
		t.Fatalf("MatchSensitive(vault) = (%q, %t), want (\"configured_secret\", true)", pattern, ok)
	}
	awsKey := "AKIA" + "ABCDEFGHIJKLMNOP"
	if pattern, ok := MatchSensitive("use "+awsKey+" now", nil); !ok || pattern != "aws_access_key_id" {
		t.Fatalf("MatchSensitive(aws) = (%q, %t), want (\"aws_access_key_id\", true)", pattern, ok)
	}
	ghToken := "gh" + "p_" + strings.Repeat("a1B2", 9)
	if !ContainsCredentialPattern(ghToken) {
		t.Fatal("ContainsCredentialPattern missed github token")
	}
	if len(CredentialPatternNames()) != 12 {
		t.Fatalf("CredentialPatternNames len = %d, want 12", len(CredentialPatternNames()))
	}
}
