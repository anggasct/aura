package webhook

import (
	"time"

	"github.com/anggasct/aura/internal/secret"
)

// KeyEntry names one signing key: its identifier and the environment
// variable holding its secret, plus an optional instant after which the key
// no longer authenticates new requests (zero means active without expiry).
type KeyEntry struct {
	ID          string
	SecretEnv   string
	AcceptUntil time.Time
}

// KeyConfig is a resolved signing key: identifier, secret value, and expiry.
type KeyConfig struct {
	ID          string
	Secret      string
	AcceptUntil time.Time
}

// KeySource supplies key material by environment variable name; production
// resolves through the secret package so raw values stay confined here.
type KeySource func(envName string) (string, error)

// KeyRing holds the resolved webhook signing keys plus the redaction vault
// that proves no secret reaches logs or errors. Construction resolves every
// referenced secret and fails closed: a missing secret is a startup error,
// never a silently unusable key.
type KeyRing struct {
	keys  map[string]KeyConfig
	vault *secret.Vault
}

// NewKeyRing resolves entries through source and indexes them by ID.
// Duplicate IDs are rejected.
func NewKeyRing(entries []KeyEntry, source KeySource) (*KeyRing, error) {
	if entries == nil {
		return nil, Errorf(ErrorCodeInvalidArgument, "key entries must not be nil")
	}
	if source == nil {
		return nil, Errorf(ErrorCodeInvalidArgument, "key source must not be nil")
	}
	ring := &KeyRing{keys: make(map[string]KeyConfig, len(entries)), vault: secret.New()}
	for _, entry := range entries {
		if entry.ID == "" {
			return nil, Errorf(ErrorCodeInvalidArgument, "key entry id must not be empty")
		}
		if _, duplicate := ring.keys[entry.ID]; duplicate {
			return nil, Errorf(ErrorCodeInvalidArgument, "key id %q configured more than once", entry.ID)
		}
		value, err := source(entry.SecretEnv)
		if err != nil {
			return nil, Errorf(ErrorCodeKeyResolutionFailed, "key %q secret is unavailable", entry.ID)
		}
		if value == "" {
			return nil, Errorf(ErrorCodeKeyResolutionFailed, "key %q secret is unavailable", entry.ID)
		}
		ring.keys[entry.ID] = KeyConfig{ID: entry.ID, Secret: value, AcceptUntil: entry.AcceptUntil}
		ring.vault.Set(entry.ID, value)
	}
	return ring, nil
}

// Lookup returns the secret for a key that may still authenticate at now.
// An unknown identifier and an expired key are indistinguishable to callers:
// both fail authentication with the same error and the same log content.
func (r *KeyRing) Lookup(id string, now time.Time) (string, error) {
	key, ok := r.keys[id]
	if !ok || (!key.AcceptUntil.IsZero() && !now.Before(key.AcceptUntil)) {
		return "", Errorf(ErrorCodeAuthFailed, "unknown or expired key")
	}
	return key.Secret, nil
}

// Vault exposes the redaction vault backing the ring.
func (r *KeyRing) Vault() *secret.Vault {
	return r.vault
}
