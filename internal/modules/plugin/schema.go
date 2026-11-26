package plugin

import (
	"github.com/anggasct/aura/internal/platform/sqlite"
)

const schemaSQL = `
CREATE TABLE plugin (
    id TEXT PRIMARY KEY,
    state TEXT NOT NULL CHECK (state IN ('quarantined','active','disabled')),
    version TEXT NOT NULL,
    approved_version TEXT,
    updated_at TEXT NOT NULL
);

CREATE TABLE plugin_version (
    plugin_id TEXT NOT NULL REFERENCES plugin(id) ON DELETE CASCADE,
    version TEXT NOT NULL,
    digest TEXT NOT NULL,
    manifest_json TEXT NOT NULL,
    installed_at TEXT NOT NULL,
    PRIMARY KEY (plugin_id, version)
);

CREATE TABLE plugin_grant (
    plugin_id TEXT NOT NULL REFERENCES plugin(id) ON DELETE CASCADE,
    surface TEXT NOT NULL CHECK (surface IN ('tool','capability','egress','secret')),
    value TEXT NOT NULL,
    fingerprint TEXT NOT NULL,
    approved_at TEXT NOT NULL,
    approved_by TEXT NOT NULL,
    PRIMARY KEY (plugin_id, surface, value)
);
`

func Schema() sqlite.SchemaOwner {
	return sqlite.StaticSchema{Name: "plugin", SQL: schemaSQL}
}
