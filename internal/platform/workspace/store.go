package workspace

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/anggasct/aura/internal/platform/sqlite"
)

const (
	ScopeFile          = "file"
	ScopeWorkspace     = "workspace"
	DefaultScanEntries = 50000
	MaxSourceBytes     = 120
	fallbackSource     = "untrusted content"
)

type TaintRecord struct {
	ID            string
	Scope         string
	Path          string
	ContentSHA256 string
	Device        int64
	Inode         int64
	Source        string
	SessionID     string
	RecordedAt    time.Time
}

type WalkedFile struct {
	Path          string
	ContentSHA256 string
	Device        int64
	Inode         int64
	Changed       bool
}

type WalkResult struct {
	Files         []WalkedFile
	ExceededBound bool
}

type WalkStats struct {
	Recorded int
	Moved    int
	Deleted  int
	Wide     int
}

type TaintStore struct {
	db    *sql.DB
	clock func() time.Time
}

func NewTaintStore(db *sql.DB) *TaintStore {
	return &TaintStore{
		db:    db,
		clock: func() time.Time { return time.Now().UTC() },
	}
}

func normalizeRelativePath(path string) string {
	return filepath.ToSlash(filepath.Clean(path))
}

func boundSource(source string) string {
	source = strings.TrimSpace(source)
	if source == "" {
		return fallbackSource
	}
	if len(source) <= MaxSourceBytes {
		return source
	}
	cut := MaxSourceBytes
	for cut > 0 && !utf8.RuneStart(source[cut]) {
		cut--
	}
	return strings.ToValidUTF8(source[:cut], "")
}

func newRecordID() (string, error) {
	var raw [12]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("workspace: generate taint record id: %w", err)
	}
	return "wt-" + hex.EncodeToString(raw[:]), nil
}

func nullableText(v string) any {
	if v == "" {
		return nil
	}
	return v
}

func (s *TaintStore) now() time.Time {
	if s != nil && s.clock != nil {
		return s.clock().UTC()
	}
	return time.Now().UTC()
}

func (s *TaintStore) RecordFile(ctx context.Context, path, sha256Hex string, device, inode int64, source, sessionID string) error {
	if s == nil || s.db == nil {
		return nil
	}
	cleanPath := normalizeRelativePath(path)
	if err := ValidateRelative(cleanPath); err != nil {
		return err
	}
	id, err := newRecordID()
	if err != nil {
		return err
	}
	src := boundSource(source)
	now := sqlite.FormatTime(s.now())
	return sqlite.RunInTx(ctx, s.db, func(ctx context.Context) error {
		q := sqlite.Conn(ctx, s.db)
		if _, err := q.ExecContext(ctx,
			`DELETE FROM workspace_taint WHERE scope = ? AND path = ? AND device = ? AND inode = ?`,
			ScopeFile, cleanPath, device, inode,
		); err != nil {
			return fmt.Errorf("workspace: replace existing taint row: %w", err)
		}
		if _, err := q.ExecContext(ctx,
			`INSERT INTO workspace_taint (id, scope, path, content_sha256, device, inode, source, session_id, recorded_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id, ScopeFile, cleanPath, sha256Hex, device, inode, src, nullableText(sessionID), now,
		); err != nil {
			return fmt.Errorf("workspace: insert file taint row: %w", err)
		}
		return nil
	})
}

func (s *TaintStore) RecordFileWithID(ctx context.Context, id, path, sha256Hex string, device, inode int64, source, sessionID string) (bool, error) {
	if s == nil || s.db == nil {
		return false, nil
	}
	cleanPath := normalizeRelativePath(path)
	if err := ValidateRelative(cleanPath); err != nil {
		return false, err
	}
	src := boundSource(source)
	now := sqlite.FormatTime(s.now())
	res, err := sqlite.Conn(ctx, s.db).ExecContext(ctx,
		`INSERT INTO workspace_taint (id, scope, path, content_sha256, device, inode, source, session_id, recorded_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO NOTHING`,
		id, ScopeFile, cleanPath, sha256Hex, device, inode, src, nullableText(sessionID), now,
	)
	if err != nil {
		return false, fmt.Errorf("workspace: insert deterministic file taint row: %w", err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return rows > 0, nil
}

func (s *TaintStore) RecordWorkspaceWide(ctx context.Context, source, sessionID string) (bool, error) {
	if s == nil || s.db == nil {
		return false, nil
	}
	id, err := newRecordID()
	if err != nil {
		return false, err
	}
	return s.RecordWorkspaceWideWithID(ctx, id, source, sessionID)
}

func (s *TaintStore) RecordWorkspaceWideWithID(ctx context.Context, id, source, sessionID string) (bool, error) {
	if s == nil || s.db == nil {
		return false, nil
	}
	src := boundSource(source)
	now := sqlite.FormatTime(s.now())
	created := false
	err := sqlite.RunInTx(ctx, s.db, func(ctx context.Context) error {
		q := sqlite.Conn(ctx, s.db)
		var exists bool
		if err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM workspace_taint WHERE scope = ?)`, ScopeWorkspace).Scan(&exists); err != nil {
			return fmt.Errorf("workspace: check wide taint row: %w", err)
		}
		if exists {
			return nil
		}
		res, err := q.ExecContext(ctx,
			`INSERT INTO workspace_taint (id, scope, path, content_sha256, device, inode, source, session_id, recorded_at)
			 VALUES (?, ?, NULL, NULL, NULL, NULL, ?, ?, ?)
			 ON CONFLICT(id) DO NOTHING`,
			id, ScopeWorkspace, src, nullableText(sessionID), now,
		)
		if err != nil {
			return fmt.Errorf("workspace: insert wide taint row: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		created = n > 0
		return nil
	})
	return created, err
}

func (s *TaintStore) ClearPath(ctx context.Context, path string) (int64, error) {
	if s == nil || s.db == nil {
		return 0, nil
	}
	cleanPath := normalizeRelativePath(path)
	res, err := sqlite.Conn(ctx, s.db).ExecContext(ctx,
		`DELETE FROM workspace_taint WHERE scope = ? AND path = ?`,
		ScopeFile, cleanPath,
	)
	if err != nil {
		return 0, fmt.Errorf("workspace: clear taint path %s: %w", cleanPath, err)
	}
	return res.RowsAffected()
}

func (s *TaintStore) HasRecords(ctx context.Context) (bool, error) {
	if s == nil || s.db == nil {
		return false, nil
	}
	var exists bool
	if err := sqlite.Conn(ctx, s.db).QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM workspace_taint)`).Scan(&exists); err != nil {
		return false, fmt.Errorf("workspace: check taint records: %w", err)
	}
	return exists, nil
}

func (s *TaintStore) HasWorkspaceWide(ctx context.Context) (bool, error) {
	if s == nil || s.db == nil {
		return false, nil
	}
	var exists bool
	if err := sqlite.Conn(ctx, s.db).QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM workspace_taint WHERE scope = ?)`, ScopeWorkspace).Scan(&exists); err != nil {
		return false, fmt.Errorf("workspace: check workspace-wide taint: %w", err)
	}
	return exists, nil
}

func (s *TaintStore) OldestSource(ctx context.Context) (source string, found bool, err error) {
	if s == nil || s.db == nil {
		return "", false, nil
	}
	err = sqlite.Conn(ctx, s.db).QueryRowContext(ctx,
		`SELECT source FROM workspace_taint ORDER BY recorded_at ASC, rowid ASC LIMIT 1`,
	).Scan(&source)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("workspace: read oldest taint source: %w", err)
	}
	return source, true, nil
}

func (s *TaintStore) MatchesFile(ctx context.Context, path, sha256Hex string, device, inode int64) (bool, error) {
	if s == nil || s.db == nil {
		return false, nil
	}
	cleanPath := normalizeRelativePath(path)
	var matched bool
	err := sqlite.Conn(ctx, s.db).QueryRowContext(ctx,
		`SELECT EXISTS(
			SELECT 1 FROM workspace_taint
			WHERE scope = ?
			   OR (scope = ? AND (path = ? OR content_sha256 = ? OR (device = ? AND inode = ?)))
		)`,
		ScopeWorkspace, ScopeFile, cleanPath, sha256Hex, device, inode,
	).Scan(&matched)
	if err != nil {
		return false, fmt.Errorf("workspace: match file taint: %w", err)
	}
	return matched, nil
}

func (s *TaintStore) MatchesEntry(ctx context.Context, path string, device, inode int64, hasStat bool) (bool, error) {
	if s == nil || s.db == nil {
		return false, nil
	}
	cleanPath := normalizeRelativePath(path)
	var matched bool
	var err error
	if hasStat {
		err = sqlite.Conn(ctx, s.db).QueryRowContext(ctx,
			`SELECT EXISTS(
				SELECT 1 FROM workspace_taint
				WHERE scope = ?
				   OR (scope = ? AND (path = ? OR (device = ? AND inode = ?)))
			)`,
			ScopeWorkspace, ScopeFile, cleanPath, device, inode,
		).Scan(&matched)
	} else {
		err = sqlite.Conn(ctx, s.db).QueryRowContext(ctx,
			`SELECT EXISTS(
				SELECT 1 FROM workspace_taint
				WHERE scope = ?
				   OR (scope = ? AND path = ?)
			)`,
			ScopeWorkspace, ScopeFile, cleanPath,
		).Scan(&matched)
	}
	if err != nil {
		return false, fmt.Errorf("workspace: match entry taint: %w", err)
	}
	return matched, nil
}

func (s *TaintStore) List(ctx context.Context, pathFilter string) ([]TaintRecord, error) {
	if s == nil || s.db == nil {
		return nil, nil
	}
	var rows *sql.Rows
	var err error
	if strings.TrimSpace(pathFilter) != "" {
		cleanPath := normalizeRelativePath(pathFilter)
		if err := ValidateRelative(cleanPath); err != nil {
			return nil, err
		}
		rows, err = sqlite.Conn(ctx, s.db).QueryContext(ctx,
			`SELECT id, scope, path, content_sha256, device, inode, source, session_id, recorded_at
			 FROM workspace_taint
			 WHERE scope = ? AND path = ?
			 ORDER BY recorded_at ASC, id ASC`,
			ScopeFile, cleanPath,
		)
	} else {
		rows, err = sqlite.Conn(ctx, s.db).QueryContext(ctx,
			`SELECT id, scope, path, content_sha256, device, inode, source, session_id, recorded_at
			 FROM workspace_taint
			 ORDER BY CASE WHEN scope = 'workspace' THEN 0 ELSE 1 END, recorded_at ASC, id ASC`,
		)
	}
	if err != nil {
		return nil, fmt.Errorf("workspace: list taint records: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var records []TaintRecord
	for rows.Next() {
		var rec TaintRecord
		var path, digest, sessionID sql.NullString
		var dev, ino sql.NullInt64
		var recordedAt string
		if err := rows.Scan(&rec.ID, &rec.Scope, &path, &digest, &dev, &ino, &rec.Source, &sessionID, &recordedAt); err != nil {
			return nil, fmt.Errorf("workspace: scan taint record: %w", err)
		}
		if path.Valid {
			rec.Path = path.String
		}
		if digest.Valid {
			rec.ContentSHA256 = digest.String
		}
		if dev.Valid {
			rec.Device = dev.Int64
		}
		if ino.Valid {
			rec.Inode = ino.Int64
		}
		if sessionID.Valid {
			rec.SessionID = sessionID.String
		}
		parsed, err := time.Parse(time.RFC3339Nano, recordedAt)
		if err != nil {
			return nil, fmt.Errorf("workspace: parse taint record time: %w", err)
		}
		rec.RecordedAt = parsed
		records = append(records, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("workspace: iterate taint records: %w", err)
	}
	return records, nil
}

func (s *TaintStore) DeleteIDs(ctx context.Context, ids []string) (int64, error) {
	if s == nil || s.db == nil || len(ids) == 0 {
		return 0, nil
	}
	encoded, err := json.Marshal(ids)
	if err != nil {
		return 0, fmt.Errorf("workspace: encode taint ids: %w", err)
	}
	res, err := sqlite.Conn(ctx, s.db).ExecContext(ctx,
		`DELETE FROM workspace_taint WHERE id IN (SELECT value FROM json_each(?))`,
		string(encoded),
	)
	if err != nil {
		return 0, fmt.Errorf("workspace: delete taint records: %w", err)
	}
	return res.RowsAffected()
}

func (s *TaintStore) NullSessionIDs(ctx context.Context, sessionIDs []string) (int64, error) {
	if s == nil || s.db == nil || len(sessionIDs) == 0 {
		return 0, nil
	}
	encoded, err := json.Marshal(sessionIDs)
	if err != nil {
		return 0, fmt.Errorf("workspace: encode session ids: %w", err)
	}
	res, err := sqlite.Conn(ctx, s.db).ExecContext(ctx,
		`UPDATE workspace_taint SET session_id = NULL WHERE session_id IN (SELECT value FROM json_each(?))`,
		string(encoded),
	)
	if err != nil {
		return 0, fmt.Errorf("workspace: null erased session ids: %w", err)
	}
	return res.RowsAffected()
}

type inodeKey struct {
	device int64
	inode  int64
}

func (s *TaintStore) ApplyWalkResults(ctx context.Context, walk WalkResult, source, sessionID string) (WalkStats, error) {
	if s == nil || s.db == nil {
		return WalkStats{}, nil
	}
	if walk.ExceededBound {
		created, err := s.RecordWorkspaceWide(ctx, source, sessionID)
		if err != nil {
			return WalkStats{}, err
		}
		stats := WalkStats{}
		if created {
			stats.Wide = 1
		}
		return stats, nil
	}

	var stats WalkStats
	src := boundSource(source)
	now := sqlite.FormatTime(s.now())
	byInode := make(map[inodeKey]WalkedFile, len(walk.Files))
	byPath := make(map[string]WalkedFile, len(walk.Files))
	for i := range walk.Files {
		f := walk.Files[i]
		f.Path = normalizeRelativePath(f.Path)
		walk.Files[i] = f
		byInode[inodeKey{device: f.Device, inode: f.Inode}] = f
		byPath[f.Path] = f
	}

	err := sqlite.RunInTx(ctx, s.db, func(ctx context.Context) error {
		q := sqlite.Conn(ctx, s.db)
		existing, err := s.listFileRowsTx(ctx, q)
		if err != nil {
			return err
		}
		coveredPaths := map[string]bool{}
		for i := range existing {
			row := &existing[i]
			if wf, foundByInode := byInode[inodeKey{device: row.Device, inode: row.Inode}]; foundByInode {
				if wf.Path != row.Path || wf.ContentSHA256 != row.ContentSHA256 {
					if _, err := q.ExecContext(ctx,
						`UPDATE workspace_taint SET path = ?, content_sha256 = ? WHERE id = ?`,
						wf.Path, wf.ContentSHA256, row.ID,
					); err != nil {
						return fmt.Errorf("workspace: move taint row: %w", err)
					}
					stats.Moved++
				}
				coveredPaths[wf.Path] = true
				continue
			}
			if wf, foundByPath := byPath[row.Path]; foundByPath {
				if wf.Changed {
					if _, err := q.ExecContext(ctx,
						`UPDATE workspace_taint SET content_sha256 = ?, device = ?, inode = ? WHERE id = ?`,
						wf.ContentSHA256, wf.Device, wf.Inode, row.ID,
					); err != nil {
						return fmt.Errorf("workspace: update taint row: %w", err)
					}
				}
				coveredPaths[wf.Path] = true
				continue
			}
			if _, err := q.ExecContext(ctx, `DELETE FROM workspace_taint WHERE id = ?`, row.ID); err != nil {
				return fmt.Errorf("workspace: delete missing file taint row: %w", err)
			}
			stats.Deleted++
		}

		for i := range walk.Files {
			wf := &walk.Files[i]
			if !wf.Changed || coveredPaths[wf.Path] {
				continue
			}
			id, err := newRecordID()
			if err != nil {
				return err
			}
			if _, err := q.ExecContext(ctx,
				`INSERT INTO workspace_taint (id, scope, path, content_sha256, device, inode, source, session_id, recorded_at)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				id, ScopeFile, wf.Path, wf.ContentSHA256, wf.Device, wf.Inode, src, nullableText(sessionID), now,
			); err != nil {
				return fmt.Errorf("workspace: insert walked taint row: %w", err)
			}
			coveredPaths[wf.Path] = true
			stats.Recorded++
		}
		return nil
	})
	return stats, err
}

func (s *TaintStore) listFileRowsTx(ctx context.Context, q sqlite.Querier) ([]TaintRecord, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT id, path, content_sha256, device, inode, source, session_id
		 FROM workspace_taint
		 WHERE scope = ?
		 ORDER BY recorded_at ASC, id ASC`,
		ScopeFile,
	)
	if err != nil {
		return nil, fmt.Errorf("workspace: list file taint rows: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []TaintRecord
	for rows.Next() {
		var rec TaintRecord
		rec.Scope = ScopeFile
		var sessionID sql.NullString
		if err := rows.Scan(&rec.ID, &rec.Path, &rec.ContentSHA256, &rec.Device, &rec.Inode, &rec.Source, &sessionID); err != nil {
			return nil, fmt.Errorf("workspace: scan file taint row: %w", err)
		}
		if sessionID.Valid {
			rec.SessionID = sessionID.String
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("workspace: iterate file taint rows: %w", err)
	}
	return out, nil
}
