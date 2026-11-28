package profile

import (
	"github.com/anggasct/aura/internal/platform/sqlite"
)

const schemaSQL = `
CREATE TABLE profile_fact (
    id TEXT PRIMARY KEY,
    owner_id TEXT NOT NULL,
    category TEXT NOT NULL CHECK (category IN ('preference','tool','language','timezone','project','habit')),
    fact_key TEXT NOT NULL,
    fact_value TEXT NOT NULL,
    value_digest TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('candidate','active','rejected','expired','deleted')),
    origin TEXT NOT NULL CHECK (origin IN ('derived','owner')),
    owner_verified INTEGER NOT NULL DEFAULT 0 CHECK (owner_verified IN (0,1)),
    confidence REAL NOT NULL CHECK (confidence >= 0.0 AND confidence <= 1.0),
    confidence_policy_version TEXT NOT NULL,
    conflicts_with_id TEXT REFERENCES profile_fact(id) ON DELETE SET NULL,
    expires_at TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE(owner_id, category, fact_key, value_digest)
);

CREATE VIRTUAL TABLE profile_fact_fts USING fts5(
    fact_key,
    fact_value,
    content='profile_fact',
    content_rowid='rowid',
    tokenize='unicode61'
);

CREATE TABLE profile_evidence (
    fact_id TEXT NOT NULL REFERENCES profile_fact(id) ON DELETE CASCADE,
    source_event_id TEXT NOT NULL REFERENCES runtime_event(id) ON DELETE RESTRICT,
    source_digest TEXT NOT NULL,
    extractor_provider TEXT,
    extractor_model TEXT,
    extractor_model_version TEXT,
    prompt_version TEXT,
    prompt_digest TEXT,
    observed_at TEXT NOT NULL,
    PRIMARY KEY (fact_id, source_event_id, source_digest)
);

CREATE INDEX profile_fact_lookup_idx ON profile_fact(owner_id, status, category, fact_key, expires_at, confidence DESC, id);

CREATE TRIGGER profile_fact_fts_insert AFTER INSERT ON profile_fact BEGIN INSERT INTO profile_fact_fts(rowid, fact_key, fact_value) VALUES (new.rowid, new.fact_key, new.fact_value); END;

CREATE TRIGGER profile_fact_fts_delete AFTER DELETE ON profile_fact BEGIN INSERT INTO profile_fact_fts(profile_fact_fts, rowid, fact_key, fact_value) VALUES ('delete', old.rowid, old.fact_key, old.fact_value); END;

CREATE TRIGGER profile_fact_fts_update AFTER UPDATE OF fact_key, fact_value ON profile_fact BEGIN INSERT INTO profile_fact_fts(profile_fact_fts, rowid, fact_key, fact_value) VALUES ('delete', old.rowid, old.fact_key, old.fact_value); INSERT INTO profile_fact_fts(rowid, fact_key, fact_value) VALUES (new.rowid, new.fact_key, new.fact_value); END;
`

const agentOriginStepSQL = `
CREATE TABLE profile_fact_keep AS SELECT
    id, owner_id, category, fact_key, fact_value, value_digest, status, origin,
    owner_verified, confidence, confidence_policy_version, conflicts_with_id,
    expires_at, created_at, updated_at
FROM profile_fact;

CREATE TABLE profile_evidence_keep AS SELECT
    fact_id, source_event_id, source_digest, extractor_provider, extractor_model,
    extractor_model_version, prompt_version, prompt_digest, observed_at
FROM profile_evidence;

DROP TABLE profile_evidence;

DROP TABLE profile_fact_fts;

DROP TABLE profile_fact;

CREATE TABLE profile_fact (
    id TEXT PRIMARY KEY,
    owner_id TEXT NOT NULL,
    category TEXT NOT NULL CHECK (category IN ('preference','tool','language','timezone','project','habit')),
    fact_key TEXT NOT NULL,
    fact_value TEXT NOT NULL,
    value_digest TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('candidate','active','rejected','expired','deleted')),
    origin TEXT NOT NULL CHECK (origin IN ('derived','owner','agent')),
    owner_verified INTEGER NOT NULL DEFAULT 0 CHECK (owner_verified IN (0,1)),
    confidence REAL NOT NULL CHECK (confidence >= 0.0 AND confidence <= 1.0),
    confidence_policy_version TEXT NOT NULL,
    conflicts_with_id TEXT REFERENCES profile_fact(id) ON DELETE SET NULL,
    expires_at TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE(owner_id, category, fact_key, value_digest)
);

CREATE VIRTUAL TABLE profile_fact_fts USING fts5(
    fact_key,
    fact_value,
    content='profile_fact',
    content_rowid='rowid',
    tokenize='unicode61'
);

CREATE TABLE profile_evidence (
    fact_id TEXT NOT NULL REFERENCES profile_fact(id) ON DELETE CASCADE,
    source_event_id TEXT NOT NULL REFERENCES runtime_event(id) ON DELETE RESTRICT,
    source_digest TEXT NOT NULL,
    extractor_provider TEXT,
    extractor_model TEXT,
    extractor_model_version TEXT,
    prompt_version TEXT,
    prompt_digest TEXT,
    observed_at TEXT NOT NULL,
    PRIMARY KEY (fact_id, source_event_id, source_digest)
);

CREATE INDEX profile_fact_lookup_idx ON profile_fact(owner_id, status, category, fact_key, expires_at, confidence DESC, id);

CREATE TRIGGER profile_fact_fts_insert AFTER INSERT ON profile_fact BEGIN INSERT INTO profile_fact_fts(rowid, fact_key, fact_value) VALUES (new.rowid, new.fact_key, new.fact_value); END;

CREATE TRIGGER profile_fact_fts_delete AFTER DELETE ON profile_fact BEGIN INSERT INTO profile_fact_fts(profile_fact_fts, rowid, fact_key, fact_value) VALUES ('delete', old.rowid, old.fact_key, old.fact_value); END;

CREATE TRIGGER profile_fact_fts_update AFTER UPDATE OF fact_key, fact_value ON profile_fact BEGIN INSERT INTO profile_fact_fts(profile_fact_fts, rowid, fact_key, fact_value) VALUES ('delete', old.rowid, old.fact_key, old.fact_value); INSERT INTO profile_fact_fts(rowid, fact_key, fact_value) VALUES (new.rowid, new.fact_key, new.fact_value); END;

INSERT INTO profile_fact (
    id, owner_id, category, fact_key, fact_value, value_digest, status, origin,
    owner_verified, confidence, confidence_policy_version, conflicts_with_id,
    expires_at, created_at, updated_at
) SELECT
    id, owner_id, category, fact_key, fact_value, value_digest, status, origin,
    owner_verified, confidence, confidence_policy_version, conflicts_with_id,
    expires_at, created_at, updated_at
FROM profile_fact_keep;

INSERT INTO profile_evidence (
    fact_id, source_event_id, source_digest, extractor_provider, extractor_model,
    extractor_model_version, prompt_version, prompt_digest, observed_at
) SELECT
    fact_id, source_event_id, source_digest, extractor_provider, extractor_model,
    extractor_model_version, prompt_version, prompt_digest, observed_at
FROM profile_evidence_keep;

DROP TABLE profile_evidence_keep;

DROP TABLE profile_fact_keep;
`

const agentOriginStepVersion = 28

const taintSourceStepSQL = `
ALTER TABLE profile_fact ADD COLUMN taint_source TEXT;
`

const taintSourceStepVersion = 35

func Schema() sqlite.SchemaOwner {
	return sqlite.StaticSchema{
		Name: "profile",
		SQL:  schemaSQL,
		Steps: []sqlite.Step{
			{Version: agentOriginStepVersion, SQL: agentOriginStepSQL},
			{Version: taintSourceStepVersion, SQL: taintSourceStepSQL},
		},
	}
}
