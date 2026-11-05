package httpchat

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/hex"
	"io"
	"log/slog"
	"strings"
)

const (
	KeyPrefix = "aura_sk_"

	KeyIDLength     = 12
	keyIDBytes      = 8
	keySecretBytes  = 32
	keySecretLength = 52
	keySeparator    = '_'
)

var keyEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

type IssuedKey struct {
	ID        string
	Plaintext string
	Digest    string
}

func (k IssuedKey) String() string {
	return "api key " + k.ID
}

func (k IssuedKey) LogValue() slog.Value {
	return slog.StringValue(k.String())
}

func NewKey() (IssuedKey, error) {
	return issueKey(rand.Reader)
}

func issueKey(source io.Reader) (IssuedKey, error) {
	idBytes := make([]byte, keyIDBytes)
	secretBytes := make([]byte, keySecretBytes)
	if _, err := io.ReadFull(source, idBytes); err != nil {
		return IssuedKey{}, &Error{Code: ErrorCodeKeyGeneration, Detail: "random source failed", Cause: err}
	}
	if _, err := io.ReadFull(source, secretBytes); err != nil {
		return IssuedKey{}, &Error{Code: ErrorCodeKeyGeneration, Detail: "random source failed", Cause: err}
	}
	id := strings.ToLower(keyEncoding.EncodeToString(idBytes))[:KeyIDLength]
	secret := strings.ToLower(keyEncoding.EncodeToString(secretBytes))
	plaintext := KeyPrefix + id + string(keySeparator) + secret
	return IssuedKey{ID: id, Plaintext: plaintext, Digest: KeyDigest(plaintext)}, nil
}

func KeyDigest(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

func ParseKey(raw string) (string, error) {
	rest, found := strings.CutPrefix(raw, KeyPrefix)
	if !found || len(rest) != KeyIDLength+1+keySecretLength || rest[KeyIDLength] != keySeparator {
		return "", Errorf(ErrorCodeKeyMalformed, "api key has the wrong shape")
	}
	id, secret := rest[:KeyIDLength], rest[KeyIDLength+1:]
	if !isKeyAlphabet(id) || !isKeyAlphabet(secret) {
		return "", Errorf(ErrorCodeKeyMalformed, "api key has characters outside its alphabet")
	}
	return id, nil
}

func isKeyAlphabet(text string) bool {
	for i := range len(text) {
		c := text[i]
		if (c < 'a' || c > 'z') && (c < '2' || c > '7') {
			return false
		}
	}
	return true
}

func VerifyKey(plaintext, digest string) bool {
	presented := KeyDigest(plaintext)
	return subtle.ConstantTimeCompare([]byte(presented), []byte(digest)) == 1
}
