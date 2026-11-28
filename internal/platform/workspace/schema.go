package workspace

import (
	"github.com/anggasct/aura/internal/platform/sqlite"
)

const taintStepVersion = 32

const taintStepSQL = `
CREATE TABLE workspace_taint (
    id TEXT PRIMARY KEY,
    scope TEXT NOT NULL CHECK (scope IN ('file','workspace')),
    path TEXT,
    content_sha256 TEXT,
    device INTEGER,
    inode INTEGER,
    source TEXT NOT NULL,
    session_id TEXT,
    recorded_at TEXT NOT NULL,
    CHECK ((scope = 'file' AND path IS NOT NULL AND content_sha256 IS NOT NULL AND device IS NOT NULL AND inode IS NOT NULL)
        OR (scope = 'workspace' AND path IS NULL AND content_sha256 IS NULL AND device IS NULL AND inode IS NULL))
);

CREATE INDEX workspace_taint_path_idx ON workspace_taint(path);
CREATE INDEX workspace_taint_digest_idx ON workspace_taint(content_sha256);
CREATE INDEX workspace_taint_inode_idx ON workspace_taint(device, inode);
`

func Schema() sqlite.SchemaOwner {
	return sqlite.StaticSchema{
		Name:  "workspace",
		Steps: []sqlite.Step{{Version: taintStepVersion, SQL: taintStepSQL}},
	}
}
