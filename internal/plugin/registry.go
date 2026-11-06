package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"time"
)

const (
	StateQuarantined = "quarantined"
	StateActive      = "active"
	StateDisabled    = "disabled"

	ApprovedByOwner = "owner"
)

type Record struct {
	ID              string
	State           string
	Version         string
	ApprovedVersion string
	UpdatedAt       time.Time
}

type VersionRecord struct {
	ID           string
	Version      string
	Digest       string
	ManifestJSON string
	InstalledAt  time.Time
}

type GrantRecord struct {
	ID string
	GrantKey
	Fingerprint string
	ApprovedAt  time.Time
	ApprovedBy  string
}

type InstallRecord struct {
	Plugin     Record
	Version    VersionRecord
	DropGrants []GrantKey
}

type ApprovalRecord struct {
	ID      string
	Version string
	Digest  string
	Grants  []GrantRecord
	At      time.Time
}

type Registry interface {
	Install(ctx context.Context, in *InstallRecord) error
	Approve(ctx context.Context, in *ApprovalRecord) error
	SetState(ctx context.Context, id, state string, at time.Time) error
	Get(ctx context.Context, id string) (Record, error)
	List(ctx context.Context) ([]Record, error)
	Version(ctx context.Context, id, version string) (VersionRecord, error)
	Grants(ctx context.Context, id string) ([]GrantRecord, error)
	Remove(ctx context.Context, id string) error
}

type storedManifest struct {
	Manifest     *Manifest `json:"manifest"`
	BundleSHA256 string    `json:"bundle_sha256"`
}

func encodeStored(manifest *Manifest, bundleSHA256 string) (string, error) {
	normalized := manifest.normalized()
	data, err := json.Marshal(&storedManifest{Manifest: &normalized, BundleSHA256: bundleSHA256})
	if err != nil {
		return "", Wrap(ErrManifestInvalid, err, "the manifest cannot be encoded")
	}
	return string(data), nil
}

func decodeStored(raw string) (*Manifest, string, error) {
	decoder := json.NewDecoder(bytes.NewReader([]byte(raw)))
	decoder.DisallowUnknownFields()
	var stored storedManifest
	if err := decoder.Decode(&stored); err != nil || stored.Manifest == nil || stored.BundleSHA256 == "" {
		return nil, "", Errorf(ErrStorage, "a stored manifest is unreadable")
	}
	if err := stored.Manifest.Validate(); err != nil {
		return nil, "", Wrap(ErrStorage, err, "a stored manifest no longer validates")
	}
	return stored.Manifest, stored.BundleSHA256, nil
}
