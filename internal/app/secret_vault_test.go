package app

import (
	"strings"
	"testing"

	appconfig "github.com/anggasct/aura/internal/app/config"
	"github.com/anggasct/aura/internal/modules/github"
	"github.com/anggasct/aura/internal/platform/secret/secrettest"
)

func TestConfiguredGitHubCredentialsJoinTheSecretVault(t *testing.T) {
	cfg := appconfig.Default()
	cfg.Tools.GitHub.Credentials = []github.Credential{{Name: "ci", Ref: secrettest.Ref("env://AURA_TEST_GITHUB_TOKEN")}}
	if _, err := buildStrictSecretVault(&cfg); err != nil && strings.Contains(err.Error(), "github_credential_ci") {
		t.Errorf("strict vault names an unset github credential as unresolved: %v", err)
	}
	t.Setenv("AURA_TEST_GITHUB_TOKEN", "ghp-vault-token")
	if !buildSecretVault(&cfg).Contains("the token is ghp-vault-token") {
		t.Error("lenient vault does not hold the github token")
	}
	strict, err := buildStrictSecretVault(&cfg)
	if err != nil {
		t.Fatalf("strict vault: %v", err)
	}
	if !strict.Contains("the token is ghp-vault-token") {
		t.Error("strict vault does not hold the github token")
	}
}
