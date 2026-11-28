package secret

import (
	"context"
	"regexp"
)

type Screen interface {
	Contains(text string) bool
}

type Source interface {
	Snapshot(ctx context.Context) (Screen, error)
}

type credentialPattern struct {
	name string
	re   *regexp.Regexp
}

var credentialPatterns = []credentialPattern{
	{name: "private_key_block", re: regexp.MustCompile(`-{5}BEGIN (?:[A-Z0-9]+ )*PRIVATE KEY(?: BLOCK)?-{5}`)},
	{name: "aws_access_key_id", re: regexp.MustCompile(`\b(?:AKIA|ASIA|AGPA|AIDA|AROA|ANPA|ANVA|AIPA)[0-9A-Z]{16}\b`)},
	{name: "aws_secret_assignment", re: regexp.MustCompile(`(?i)\baws_secret_access_key\b["']?\s*[:=]\s*["']?[A-Za-z0-9/+=]{40}`)},
	{name: "github_token", re: regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{36,255}\b`)},
	{name: "github_fine_grained_token", re: regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{22,255}\b`)},
	{name: "gitlab_token", re: regexp.MustCompile(`\bglpat-[A-Za-z0-9_-]{20,}`)},
	{name: "google_api_key", re: regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}`)},
	{name: "slack_token", re: regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}`)},
	{name: "stripe_live_key", re: regexp.MustCompile(`\b[rs]k_live_[0-9A-Za-z]{16,}\b`)},
	{name: "npm_token", re: regexp.MustCompile(`\bnpm_[A-Za-z0-9]{36}\b`)},
	{name: "anthropic_api_key", re: regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_-]{20,}`)},
	{name: "azure_storage_account_key", re: regexp.MustCompile(`AccountKey=[A-Za-z0-9+/]{40,}={0,2}`)},
}

func CredentialPatternNames() []string {
	names := make([]string, 0, len(credentialPatterns))
	for i := range credentialPatterns {
		names = append(names, credentialPatterns[i].name)
	}
	return names
}

func MatchCredentialPattern(text string) (string, bool) {
	for i := range credentialPatterns {
		if credentialPatterns[i].re.MatchString(text) {
			return credentialPatterns[i].name, true
		}
	}
	return "", false
}

func ContainsCredentialPattern(text string) bool {
	_, matched := MatchCredentialPattern(text)
	return matched
}

func MatchSensitive(text string, vault Screen) (string, bool) {
	if pattern, matched := MatchCredentialPattern(text); matched {
		return pattern, true
	}
	if vault != nil && vault.Contains(text) {
		return "configured_secret", true
	}
	return "", false
}
