package webhook

import (
	"time"

	"github.com/anggasct/aura/internal/secret"
)

type KeyEntry struct {
	ID          string
	SecretEnv   string
	AcceptUntil time.Time
}

type KeyConfig struct {
	ID          string
	Secret      string
	AcceptUntil time.Time
}

type KeySource func(envName string) (string, error)

type KeyRing struct {
	keys  map[string]KeyConfig
	vault *secret.Vault
}

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

func (r *KeyRing) Lookup(id string, now time.Time) (string, error) {
	key, ok := r.keys[id]
	if !ok || (!key.AcceptUntil.IsZero() && !now.Before(key.AcceptUntil)) {
		return "", Errorf(ErrorCodeAuthFailed, "unknown or expired key")
	}
	return key.Secret, nil
}

func (r *KeyRing) Vault() *secret.Vault {
	return r.vault
}
