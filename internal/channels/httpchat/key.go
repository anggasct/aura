package httpchat

import (
	"crypto/rand"
	"encoding/base32"
	"io"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/principal"
)

const (
	KeyPrefix   = principal.KeyPrefix
	KeyIDLength = principal.KeyIDLength
	keyIDBytes  = 8
)

var keyEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

func isKeyAlphabet(text string) bool {
	return principal.IsKeyAlphabet(text)
}

type IssuedKey = principal.IssuedKey

func NewKey() (IssuedKey, error) {
	return issueKey(rand.Reader)
}

func issueKey(source io.Reader) (IssuedKey, error) {
	issued, err := principal.IssueKey(source)
	if err != nil {
		if fault.HasCode(err, principal.ErrorCodeKeyGeneration) {
			return IssuedKey{}, fault.Wrap(ErrorCodeKeyGeneration, "random source failed", err)
		}
		return IssuedKey{}, err
	}
	return issued, nil
}

func KeyDigest(plaintext string) string {
	return principal.KeyDigest(plaintext)
}

func ParseKey(raw string) (string, error) {
	id, err := principal.ParseKey(raw)
	if err != nil {
		if fault.HasCode(err, principal.ErrorCodeKeyMalformed) {
			return "", fault.Wrap(ErrorCodeKeyMalformed, err.Error(), err)
		}
		return "", err
	}
	return id, nil
}

func VerifyKey(plaintext, digest string) bool {
	return principal.VerifyKey(plaintext, digest)
}
