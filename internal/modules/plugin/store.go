package plugin

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/sqlite"
)

const pluginColumns = `id, state, version, COALESCE(approved_version, ''), updated_at`

type rowScanner interface {
	Scan(dest ...any) error
}

type SQLiteStore struct {
	db *sql.DB
}

var _ Registry = (*SQLiteStore)(nil)

func NewSQLiteStore(db *sql.DB) (*SQLiteStore, error) {
	if db == nil {
		return nil, fault.New(ErrInvalidArgument, "database must not be nil")
	}
	return &SQLiteStore{db: db}, nil
}

func validatePluginState(state string) error {
	switch state {
	case StateQuarantined, StateActive, StateDisabled:
		return nil
	default:
		return fault.New(ErrInvalidArgument, "plugin state is not recognised")
	}
}

func validatePluginRow(record *Record) error {
	if strings.TrimSpace(record.ID) == "" || strings.TrimSpace(record.Version) == "" {
		return fault.New(ErrInvalidArgument, "plugin needs an id and a version")
	}
	if record.UpdatedAt.IsZero() {
		return fault.New(ErrInvalidArgument, "plugin needs an update time")
	}
	return validatePluginState(record.State)
}

func validatePluginVersionRow(version *VersionRecord) error {
	if strings.TrimSpace(version.ID) == "" || strings.TrimSpace(version.Version) == "" || version.Digest == "" || version.ManifestJSON == "" {
		return fault.New(ErrInvalidArgument, "plugin version needs an id, a version, a digest and a manifest")
	}
	if version.InstalledAt.IsZero() {
		return fault.New(ErrInvalidArgument, "plugin version needs an install time")
	}
	return nil
}

func (s *SQLiteStore) Install(ctx context.Context, in *InstallRecord) error {
	if s == nil || s.db == nil {
		return fault.New(ErrInvalidArgument, "database must not be nil")
	}
	if in == nil {
		return fault.New(ErrInvalidArgument, "plugin install must not be nil")
	}
	if err := validatePluginRow(&in.Plugin); err != nil {
		return err
	}
	if err := validatePluginVersionRow(&in.Version); err != nil {
		return err
	}
	if in.Plugin.ID != in.Version.ID || in.Plugin.Version != in.Version.Version {
		return fault.New(ErrInvalidArgument, "plugin row and version row disagree")
	}
	return sqlite.RunInTx(ctx, s.db, func(txCtx context.Context) error {
		conn := sqlite.Conn(txCtx, s.db)
		if _, err := conn.ExecContext(txCtx, `
INSERT INTO plugin (id, state, version, approved_version, updated_at) VALUES (?, ?, ?, NULLIF(?, ''), ?)
ON CONFLICT(id) DO UPDATE SET state = excluded.state, version = excluded.version, approved_version = excluded.approved_version, updated_at = excluded.updated_at`,
			in.Plugin.ID, in.Plugin.State, in.Plugin.Version, in.Plugin.ApprovedVersion, sqlite.FormatTime(in.Plugin.UpdatedAt)); err != nil {
			return pluginWriteError(err, ErrVersionConflict, "install plugin")
		}
		if _, err := conn.ExecContext(txCtx, `
INSERT INTO plugin_version (plugin_id, version, digest, manifest_json, installed_at) VALUES (?, ?, ?, ?, ?)`,
			in.Version.ID, in.Version.Version, in.Version.Digest, in.Version.ManifestJSON, sqlite.FormatTime(in.Version.InstalledAt)); err != nil {
			return pluginWriteError(err, ErrVersionConflict, "record plugin version")
		}
		for _, key := range in.DropGrants {
			if _, err := conn.ExecContext(txCtx, `DELETE FROM plugin_grant WHERE plugin_id = ? AND surface = ? AND value = ?`, in.Plugin.ID, key.Surface, key.Value); err != nil {
				return pluginWriteError(err, ErrVersionConflict, "drop plugin grant")
			}
		}
		return nil
	})
}

func (s *SQLiteStore) Approve(ctx context.Context, in *ApprovalRecord) error {
	if s == nil || s.db == nil {
		return fault.New(ErrInvalidArgument, "database must not be nil")
	}
	if in == nil {
		return fault.New(ErrInvalidArgument, "plugin approval must not be nil")
	}
	if strings.TrimSpace(in.ID) == "" || strings.TrimSpace(in.Version) == "" || in.Digest == "" || in.At.IsZero() {
		return fault.New(ErrInvalidArgument, "plugin approval needs an id, a version, a digest and a time")
	}
	return sqlite.RunInTx(ctx, s.db, func(txCtx context.Context) error {
		conn := sqlite.Conn(txCtx, s.db)
		var current, digest string
		err := conn.QueryRowContext(txCtx, `
SELECT p.version, COALESCE(v.digest, '') FROM plugin p
LEFT JOIN plugin_version v ON v.plugin_id = p.id AND v.version = ?
WHERE p.id = ?`, in.Version, in.ID).Scan(&current, &digest)
		if errors.Is(err, sql.ErrNoRows) {
			return fault.New(ErrNotFound, "plugin does not exist")
		}
		if err != nil {
			return sqlite.Classify(fmt.Errorf("read plugin for approval: %w", err))
		}
		if current != in.Version || digest != in.Digest {
			return fault.New(ErrDigestMismatch, "plugin changed since the review")
		}
		if _, err := conn.ExecContext(txCtx, `UPDATE plugin SET state = ?, approved_version = ?, updated_at = ? WHERE id = ?`,
			StateActive, in.Version, sqlite.FormatTime(in.At), in.ID); err != nil {
			return pluginWriteError(err, ErrDigestMismatch, "approve plugin")
		}
		if _, err := conn.ExecContext(txCtx, `DELETE FROM plugin_grant WHERE plugin_id = ?`, in.ID); err != nil {
			return pluginWriteError(err, ErrDigestMismatch, "replace plugin grants")
		}
		for i := range in.Grants {
			grant := &in.Grants[i]
			if grant.ID != in.ID || grant.Value == "" || grant.Fingerprint == "" || grant.ApprovedBy == "" || grant.ApprovedAt.IsZero() {
				return fault.New(ErrInvalidArgument, "plugin grant is incomplete")
			}
			if _, err := conn.ExecContext(txCtx, `
INSERT INTO plugin_grant (plugin_id, surface, value, fingerprint, approved_at, approved_by) VALUES (?, ?, ?, ?, ?, ?)`,
				grant.ID, grant.Surface, grant.Value, grant.Fingerprint, sqlite.FormatTime(grant.ApprovedAt), grant.ApprovedBy); err != nil {
				return pluginWriteError(err, ErrDigestMismatch, "record plugin grant")
			}
		}
		return nil
	})
}

func (s *SQLiteStore) SetState(ctx context.Context, id, state string, at time.Time) error {
	if s == nil || s.db == nil {
		return fault.New(ErrInvalidArgument, "database must not be nil")
	}
	if err := validatePluginState(state); err != nil {
		return err
	}
	result, err := sqlite.Conn(ctx, s.db).ExecContext(ctx, `UPDATE plugin SET state = ?, updated_at = ? WHERE id = ?`, state, sqlite.FormatTime(at), id)
	if err != nil {
		return pluginWriteError(err, ErrInvalidArgument, "set plugin state")
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return sqlite.Classify(fmt.Errorf("set plugin state: %w", err))
	}
	if changed == 0 {
		return fault.New(ErrNotFound, "plugin does not exist")
	}
	return nil
}

func (s *SQLiteStore) Get(ctx context.Context, id string) (Record, error) {
	if s == nil || s.db == nil {
		return Record{}, fault.New(ErrInvalidArgument, "database must not be nil")
	}
	record, err := scanPlugin(sqlite.Conn(ctx, s.db).QueryRowContext(ctx, `SELECT `+pluginColumns+` FROM plugin WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Record{}, fault.New(ErrNotFound, "plugin does not exist")
	}
	if err != nil {
		return Record{}, sqlite.Classify(fmt.Errorf("read plugin: %w", err))
	}
	return record, nil
}

func (s *SQLiteStore) List(ctx context.Context) ([]Record, error) {
	if s == nil || s.db == nil {
		return nil, fault.New(ErrInvalidArgument, "database must not be nil")
	}
	rows, err := sqlite.Conn(ctx, s.db).QueryContext(ctx, `SELECT `+pluginColumns+` FROM plugin ORDER BY id`)
	if err != nil {
		return nil, sqlite.Classify(fmt.Errorf("list plugins: %w", err))
	}
	defer func() { _ = rows.Close() }()
	var records []Record
	for rows.Next() {
		record, err := scanPlugin(rows)
		if err != nil {
			return nil, fmt.Errorf("list plugins: %w", err)
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, sqlite.Classify(fmt.Errorf("list plugins: %w", err))
	}
	return records, nil
}

func (s *SQLiteStore) Version(ctx context.Context, id, version string) (VersionRecord, error) {
	if s == nil || s.db == nil {
		return VersionRecord{}, fault.New(ErrInvalidArgument, "database must not be nil")
	}
	var out VersionRecord
	var installed string
	err := sqlite.Conn(ctx, s.db).QueryRowContext(ctx, `
SELECT plugin_id, version, digest, manifest_json, installed_at FROM plugin_version WHERE plugin_id = ? AND version = ?`, id, version).
		Scan(&out.ID, &out.Version, &out.Digest, &out.ManifestJSON, &installed)
	if errors.Is(err, sql.ErrNoRows) {
		return VersionRecord{}, fault.New(ErrNotFound, "plugin version does not exist")
	}
	if err != nil {
		return VersionRecord{}, sqlite.Classify(fmt.Errorf("read plugin version: %w", err))
	}
	if out.InstalledAt, err = sqlite.ParseTime(installed); err != nil {
		return VersionRecord{}, fmt.Errorf("parse plugin version install time: %w", err)
	}
	return out, nil
}

func (s *SQLiteStore) Grants(ctx context.Context, id string) ([]GrantRecord, error) {
	if s == nil || s.db == nil {
		return nil, fault.New(ErrInvalidArgument, "database must not be nil")
	}
	rows, err := sqlite.Conn(ctx, s.db).QueryContext(ctx, `
SELECT plugin_id, surface, value, fingerprint, approved_at, approved_by FROM plugin_grant WHERE plugin_id = ? ORDER BY surface, value`, id)
	if err != nil {
		return nil, sqlite.Classify(fmt.Errorf("list plugin grants: %w", err))
	}
	defer func() { _ = rows.Close() }()
	var grants []GrantRecord
	for rows.Next() {
		var grant GrantRecord
		var approved string
		if err := rows.Scan(&grant.ID, &grant.Surface, &grant.Value, &grant.Fingerprint, &approved, &grant.ApprovedBy); err != nil {
			return nil, sqlite.Classify(fmt.Errorf("list plugin grants: %w", err))
		}
		if grant.ApprovedAt, err = sqlite.ParseTime(approved); err != nil {
			return nil, fmt.Errorf("parse plugin grant time: %w", err)
		}
		grants = append(grants, grant)
	}
	if err := rows.Err(); err != nil {
		return nil, sqlite.Classify(fmt.Errorf("list plugin grants: %w", err))
	}
	return grants, nil
}

func (s *SQLiteStore) Remove(ctx context.Context, id string) error {
	if s == nil || s.db == nil {
		return fault.New(ErrInvalidArgument, "database must not be nil")
	}
	result, err := sqlite.Conn(ctx, s.db).ExecContext(ctx, `DELETE FROM plugin WHERE id = ?`, id)
	if err != nil {
		return pluginWriteError(err, ErrInvalidArgument, "remove plugin")
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return sqlite.Classify(fmt.Errorf("remove plugin: %w", err))
	}
	if changed == 0 {
		return fault.New(ErrNotFound, "plugin does not exist")
	}
	return nil
}

func pluginWriteError(err error, conflict ErrorCode, action string) error {
	classified := sqlite.Classify(err)
	switch {
	case fault.HasCode(classified, sqlite.ErrorCodeConstraintUnique),
		fault.HasCode(classified, sqlite.ErrorCodeConstraintPrimaryKey):
		return fault.New(conflict, "plugin or version already exists")
	case fault.HasCode(classified, sqlite.ErrorCodeConstraintCheck):
		return fault.New(ErrInvalidArgument, "plugin violates a storage constraint")
	}
	return fmt.Errorf("%s: %w", action, classified)
}

func scanPlugin(row rowScanner) (Record, error) {
	var record Record
	var updated string
	if err := row.Scan(&record.ID, &record.State, &record.Version, &record.ApprovedVersion, &updated); err != nil {
		return Record{}, err
	}
	var err error
	if record.UpdatedAt, err = sqlite.ParseTime(updated); err != nil {
		return Record{}, fmt.Errorf("parse plugin update time: %w", err)
	}
	return record, nil
}
