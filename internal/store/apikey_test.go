package store

import (
	"strings"
	"testing"
	"time"
)

func testAPIKey(id string) *APIKey {
	return &APIKey{
		ID:        id,
		KeySHA256: strings.Repeat(id[:1], apiKeyDigestLength),
		PersonaID: "main",
		Class:     APIKeyClassTrusted,
		Label:     "laptop",
		CreatedAt: time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC),
	}
}

func TestAPIKeyRoundTrip(t *testing.T) {
	ctx := t.Context()
	keys := NewAPIKeyStore(newTestDB(t))
	want := testAPIKey("aaaaaaaaaaaa")
	if err := keys.Create(ctx, want); err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := keys.GetByID(ctx, want.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got != *want || !got.Active() {
		t.Fatalf("GetByID = %+v, want %+v active", got, *want)
	}
}

func TestAPIKeyGetByIDUnknown(t *testing.T) {
	keys := NewAPIKeyStore(newTestDB(t))
	_, err := keys.GetByID(t.Context(), "zzzzzzzzzzzz")
	if code, ok := CodeOf(err); !ok || code != ErrorCodeAPIKeyNotFound {
		t.Fatalf("GetByID error = %v, want %s", err, ErrorCodeAPIKeyNotFound)
	}
}

func TestAPIKeyListOrdersByCreation(t *testing.T) {
	ctx := t.Context()
	keys := NewAPIKeyStore(newTestDB(t))
	if listed, err := keys.List(ctx); err != nil || len(listed) != 0 {
		t.Fatalf("List on empty store = %v, %v", listed, err)
	}
	later := testAPIKey("bbbbbbbbbbbb")
	later.CreatedAt = later.CreatedAt.Add(time.Hour)
	for _, key := range []*APIKey{later, testAPIKey("aaaaaaaaaaaa")} {
		if err := keys.Create(ctx, key); err != nil {
			t.Fatalf("Create %s: %v", key.ID, err)
		}
	}
	listed, err := keys.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(listed) != 2 || listed[0].ID != "aaaaaaaaaaaa" || listed[1].ID != "bbbbbbbbbbbb" {
		t.Fatalf("List = %+v, want creation order", listed)
	}
}

func TestAPIKeyRevokeIsIdempotent(t *testing.T) {
	ctx := t.Context()
	keys := NewAPIKeyStore(newTestDB(t))
	key := testAPIKey("aaaaaaaaaaaa")
	if err := keys.Create(ctx, key); err != nil {
		t.Fatalf("Create: %v", err)
	}
	first := key.CreatedAt.Add(time.Minute)
	if changed, err := keys.Revoke(ctx, key.ID, first); err != nil || !changed {
		t.Fatalf("first Revoke = %v, %v; want changed", changed, err)
	}
	if changed, err := keys.Revoke(ctx, key.ID, first.Add(time.Hour)); err != nil || changed {
		t.Fatalf("second Revoke = %v, %v; want unchanged", changed, err)
	}
	got, err := keys.GetByID(ctx, key.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Active() || !got.RevokedAt.Equal(first) {
		t.Fatalf("revoked key = %+v, want revoked at %s", got, first)
	}
}

func TestAPIKeyRevokeUnknown(t *testing.T) {
	keys := NewAPIKeyStore(newTestDB(t))
	_, err := keys.Revoke(t.Context(), "zzzzzzzzzzzz", time.Now())
	if code, ok := CodeOf(err); !ok || code != ErrorCodeAPIKeyNotFound {
		t.Fatalf("Revoke error = %v, want %s", err, ErrorCodeAPIKeyNotFound)
	}
}

func TestAPIKeyCreateRejects(t *testing.T) {
	ctx := t.Context()
	keys := NewAPIKeyStore(newTestDB(t))
	if err := keys.Create(ctx, testAPIKey("aaaaaaaaaaaa")); err != nil {
		t.Fatalf("seed Create: %v", err)
	}
	sameDigest := testAPIKey("bbbbbbbbbbbb")
	sameDigest.KeySHA256 = strings.Repeat("a", apiKeyDigestLength)
	badClass := testAPIKey("cccccccccccc")
	badClass.Class = "admin"
	shortID := testAPIKey("dddddddddddd")
	shortID.ID = "short"
	noLabel := testAPIKey("eeeeeeeeeeee")
	noLabel.Label = " "
	noTime := testAPIKey("ffffffffffff")
	noTime.CreatedAt = time.Time{}
	cases := map[string]struct {
		key  *APIKey
		want ErrorCode
	}{
		"duplicate id":     {key: testAPIKey("aaaaaaaaaaaa"), want: ErrorCodeAPIKeyConflict},
		"duplicate digest": {key: sameDigest, want: ErrorCodeAPIKeyConflict},
		"unknown class":    {key: badClass, want: ErrorCodeAPIKeyInvalid},
		"short id":         {key: shortID, want: ErrorCodeAPIKeyInvalid},
		"blank label":      {key: noLabel, want: ErrorCodeAPIKeyInvalid},
		"missing time":     {key: noTime, want: ErrorCodeAPIKeyInvalid},
		"nil key":          {key: nil, want: ErrorCodeInvalidArgument},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := keys.Create(ctx, tc.key)
			if code, ok := CodeOf(err); !ok || code != tc.want {
				t.Fatalf("Create error = %v, want %s", err, tc.want)
			}
		})
	}
}
