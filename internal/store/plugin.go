package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	PluginStateQuarantined = "quarantined"
	PluginStateActive      = "active"
	PluginStateDisabled    = "disabled"
)

const (
	PluginSurfaceTool       = "tool"
	PluginSurfaceCapability = "capability"
	PluginSurfaceEgress     = "egress"
	PluginSurfaceSecret     = "secret"
)

type Plugin struct {
	ID              string
	State           string
	Version         string
	ApprovedVersion string
	UpdatedAt       time.Time
}

type PluginVersion struct {
	PluginID     string
	Version      string
	Digest       string
	ManifestJSON string
	InstalledAt  time.Time
}

type PluginGrant struct {
	PluginID    string
	Surface     string
	Value       string
	Fingerprint string
	ApprovedAt  time.Time
	ApprovedBy  string
}

type PluginGrantKey struct {
	Surface string
	Value   string
}

type PluginInstall struct {
	Plugin     Plugin
	Version    PluginVersion
	DropGrants []PluginGrantKey
}

type PluginApproval struct {
	PluginID string
	Version  string
	Digest   string
	Grants   []PluginGrant
	At       time.Time
}

type PluginStore interface {
	Install(ctx context.Context, in *PluginInstall) error
	Approve(ctx context.Context, in *PluginApproval) error
	SetState(ctx context.Context, id, state string, at time.Time) error
	Get(ctx context.Context, id string) (Plugin, error)
	List(ctx context.Context) ([]Plugin, error)
	Version(ctx context.Context, id, version string) (PluginVersion, error)
	Grants(ctx context.Context, id string) ([]PluginGrant, error)
	Remove(ctx context.Context, id string) error
}

type sqlitePluginStore struct {
	db *sql.DB
}

func NewPluginStore(db *sql.DB) PluginStore {
	return &sqlitePluginStore{db: db}
}

const pluginColumns = `id, state, version, COALESCE(approved_version, ''), updated_at`

func validatePluginState(state string) error {
	switch state {
	case PluginStateQuarantined, PluginStateActive, PluginStateDisabled:
		return nil
	default:
		return Errorf(ErrorCodePluginInvalid, "plugin state is not recognised")
	}
}

func validatePluginRow(plugin *Plugin) error {
	if strings.TrimSpace(plugin.ID) == "" || strings.TrimSpace(plugin.Version) == "" {
		return Errorf(ErrorCodePluginInvalid, "plugin needs an id and a version")
	}
	if plugin.UpdatedAt.IsZero() {
		return Errorf(ErrorCodePluginInvalid, "plugin needs an update time")
	}
	return validatePluginState(plugin.State)
}

func validatePluginVersionRow(version *PluginVersion) error {
	if strings.TrimSpace(version.PluginID) == "" || strings.TrimSpace(version.Version) == "" || version.Digest == "" || version.ManifestJSON == "" {
		return Errorf(ErrorCodePluginInvalid, "plugin version needs an id, a version, a digest and a manifest")
	}
	if version.InstalledAt.IsZero() {
		return Errorf(ErrorCodePluginInvalid, "plugin version needs an install time")
	}
	return nil
}

func (s *sqlitePluginStore) Install(ctx context.Context, in *PluginInstall) error {
	if in == nil {
		return errNilArgument("plugin install")
	}
	if err := validatePluginRow(&in.Plugin); err != nil {
		return err
	}
	if err := validatePluginVersionRow(&in.Version); err != nil {
		return err
	}
	if in.Plugin.ID != in.Version.PluginID || in.Plugin.Version != in.Version.Version {
		return Errorf(ErrorCodePluginInvalid, "plugin row and version row disagree")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return classifyBusy(fmt.Errorf("begin plugin install: %w", err))
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `
INSERT INTO plugin (id, state, version, approved_version, updated_at) VALUES (?, ?, ?, NULLIF(?, ''), ?)
ON CONFLICT(id) DO UPDATE SET state = excluded.state, version = excluded.version, approved_version = excluded.approved_version, updated_at = excluded.updated_at`,
		in.Plugin.ID, in.Plugin.State, in.Plugin.Version, in.Plugin.ApprovedVersion, formatTime(in.Plugin.UpdatedAt)); err != nil {
		return pluginWriteError(err, "install plugin")
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO plugin_version (plugin_id, version, digest, manifest_json, installed_at) VALUES (?, ?, ?, ?, ?)`,
		in.Version.PluginID, in.Version.Version, in.Version.Digest, in.Version.ManifestJSON, formatTime(in.Version.InstalledAt)); err != nil {
		return pluginWriteError(err, "record plugin version")
	}
	for _, key := range in.DropGrants {
		if _, err := tx.ExecContext(ctx, `DELETE FROM plugin_grant WHERE plugin_id = ? AND surface = ? AND value = ?`, in.Plugin.ID, key.Surface, key.Value); err != nil {
			return pluginWriteError(err, "drop plugin grant")
		}
	}
	if err := tx.Commit(); err != nil {
		return classifyBusy(fmt.Errorf("commit plugin install: %w", err))
	}
	return nil
}

func (s *sqlitePluginStore) Approve(ctx context.Context, in *PluginApproval) error {
	if in == nil {
		return errNilArgument("plugin approval")
	}
	if strings.TrimSpace(in.PluginID) == "" || strings.TrimSpace(in.Version) == "" || in.Digest == "" || in.At.IsZero() {
		return Errorf(ErrorCodePluginInvalid, "plugin approval needs an id, a version, a digest and a time")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return classifyBusy(fmt.Errorf("begin plugin approval: %w", err))
	}
	defer func() { _ = tx.Rollback() }()
	var current, digest string
	err = tx.QueryRowContext(ctx, `
SELECT p.version, COALESCE(v.digest, '') FROM plugin p
LEFT JOIN plugin_version v ON v.plugin_id = p.id AND v.version = ?
WHERE p.id = ?`, in.Version, in.PluginID).Scan(&current, &digest)
	if errors.Is(err, sql.ErrNoRows) {
		return Errorf(ErrorCodePluginNotFound, "plugin does not exist")
	}
	if err != nil {
		return classifyBusy(fmt.Errorf("read plugin for approval: %w", err))
	}
	if current != in.Version || digest != in.Digest {
		return Errorf(ErrorCodePluginConflict, "plugin changed since the review")
	}
	if _, err := tx.ExecContext(ctx, `UPDATE plugin SET state = ?, approved_version = ?, updated_at = ? WHERE id = ?`,
		PluginStateActive, in.Version, formatTime(in.At), in.PluginID); err != nil {
		return pluginWriteError(err, "approve plugin")
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM plugin_grant WHERE plugin_id = ?`, in.PluginID); err != nil {
		return pluginWriteError(err, "replace plugin grants")
	}
	for i := range in.Grants {
		grant := &in.Grants[i]
		if grant.PluginID != in.PluginID || grant.Value == "" || grant.Fingerprint == "" || grant.ApprovedBy == "" || grant.ApprovedAt.IsZero() {
			return Errorf(ErrorCodePluginInvalid, "plugin grant is incomplete")
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO plugin_grant (plugin_id, surface, value, fingerprint, approved_at, approved_by) VALUES (?, ?, ?, ?, ?, ?)`,
			grant.PluginID, grant.Surface, grant.Value, grant.Fingerprint, formatTime(grant.ApprovedAt), grant.ApprovedBy); err != nil {
			return pluginWriteError(err, "record plugin grant")
		}
	}
	if err := tx.Commit(); err != nil {
		return classifyBusy(fmt.Errorf("commit plugin approval: %w", err))
	}
	return nil
}

func (s *sqlitePluginStore) SetState(ctx context.Context, id, state string, at time.Time) error {
	if err := validatePluginState(state); err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE plugin SET state = ?, updated_at = ? WHERE id = ?`, state, formatTime(at), id)
	if err != nil {
		return pluginWriteError(err, "set plugin state")
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("set plugin state: %w", err)
	}
	if changed == 0 {
		return Errorf(ErrorCodePluginNotFound, "plugin does not exist")
	}
	return nil
}

func (s *sqlitePluginStore) Get(ctx context.Context, id string) (Plugin, error) {
	plugin, err := scanPlugin(s.db.QueryRowContext(ctx, `SELECT `+pluginColumns+` FROM plugin WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Plugin{}, Errorf(ErrorCodePluginNotFound, "plugin does not exist")
	}
	if err != nil {
		return Plugin{}, classifyBusy(fmt.Errorf("read plugin: %w", err))
	}
	return plugin, nil
}

func (s *sqlitePluginStore) List(ctx context.Context) ([]Plugin, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+pluginColumns+` FROM plugin ORDER BY id`)
	if err != nil {
		return nil, classifyBusy(fmt.Errorf("list plugins: %w", err))
	}
	defer func() { _ = rows.Close() }()
	var plugins []Plugin
	for rows.Next() {
		plugin, err := scanPlugin(rows)
		if err != nil {
			return nil, fmt.Errorf("list plugins: %w", err)
		}
		plugins = append(plugins, plugin)
	}
	if err := rows.Err(); err != nil {
		return nil, classifyBusy(fmt.Errorf("list plugins: %w", err))
	}
	return plugins, nil
}

func (s *sqlitePluginStore) Version(ctx context.Context, id, version string) (PluginVersion, error) {
	var out PluginVersion
	var installed string
	err := s.db.QueryRowContext(ctx, `
SELECT plugin_id, version, digest, manifest_json, installed_at FROM plugin_version WHERE plugin_id = ? AND version = ?`, id, version).
		Scan(&out.PluginID, &out.Version, &out.Digest, &out.ManifestJSON, &installed)
	if errors.Is(err, sql.ErrNoRows) {
		return PluginVersion{}, Errorf(ErrorCodePluginNotFound, "plugin version does not exist")
	}
	if err != nil {
		return PluginVersion{}, classifyBusy(fmt.Errorf("read plugin version: %w", err))
	}
	if out.InstalledAt, err = parseTime(installed); err != nil {
		return PluginVersion{}, fmt.Errorf("parse plugin version install time: %w", err)
	}
	return out, nil
}

func (s *sqlitePluginStore) Grants(ctx context.Context, id string) ([]PluginGrant, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT plugin_id, surface, value, fingerprint, approved_at, approved_by FROM plugin_grant WHERE plugin_id = ? ORDER BY surface, value`, id)
	if err != nil {
		return nil, classifyBusy(fmt.Errorf("list plugin grants: %w", err))
	}
	defer func() { _ = rows.Close() }()
	var grants []PluginGrant
	for rows.Next() {
		var grant PluginGrant
		var approved string
		if err := rows.Scan(&grant.PluginID, &grant.Surface, &grant.Value, &grant.Fingerprint, &approved, &grant.ApprovedBy); err != nil {
			return nil, fmt.Errorf("list plugin grants: %w", err)
		}
		if grant.ApprovedAt, err = parseTime(approved); err != nil {
			return nil, fmt.Errorf("parse plugin grant time: %w", err)
		}
		grants = append(grants, grant)
	}
	if err := rows.Err(); err != nil {
		return nil, classifyBusy(fmt.Errorf("list plugin grants: %w", err))
	}
	return grants, nil
}

func (s *sqlitePluginStore) Remove(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM plugin WHERE id = ?`, id)
	if err != nil {
		return pluginWriteError(err, "remove plugin")
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("remove plugin: %w", err)
	}
	if changed == 0 {
		return Errorf(ErrorCodePluginNotFound, "plugin does not exist")
	}
	return nil
}

func pluginWriteError(err error, action string) error {
	switch {
	case isConstraintUnique(err):
		return codedError(ErrorCodePluginConflict, "plugin or version already exists", err)
	case isConstraintCheck(err):
		return codedError(ErrorCodePluginInvalid, "plugin violates a storage constraint", err)
	}
	return classifyBusy(fmt.Errorf("%s: %w", action, err))
}

func scanPlugin(row rowScanner) (Plugin, error) {
	var plugin Plugin
	var updated string
	if err := row.Scan(&plugin.ID, &plugin.State, &plugin.Version, &plugin.ApprovedVersion, &updated); err != nil {
		return Plugin{}, err
	}
	var err error
	if plugin.UpdatedAt, err = parseTime(updated); err != nil {
		return Plugin{}, fmt.Errorf("parse plugin update time: %w", err)
	}
	return plugin, nil
}
