package knowledge

import (
	"context"

	"github.com/anggasct/aura/internal/platform/secret"
)

type SecretScreen interface {
	Contains(text string) bool
}

type SecretSource interface {
	Snapshot(ctx context.Context) (SecretScreen, error)
}

func CredentialPatternNames() []string {
	return secret.CredentialPatternNames()
}

func screenText(text string, vault SecretScreen) error {
	if pattern, matched := secret.MatchCredentialPattern(text); matched {
		return Errorf(ErrorCodeSecretInvalid, "source contains credential-like content (%s)", pattern)
	}
	if vault != nil && vault.Contains(text) {
		return Errorf(ErrorCodeSecretInvalid, "source contains a configured secret value")
	}
	return nil
}
