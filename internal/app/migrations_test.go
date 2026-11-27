package app

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	appconfig "github.com/anggasct/aura/internal/app/config"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/sqlite"
	"github.com/anggasct/aura/internal/platform/storage"
)

var updateGolden = flag.Bool("update", false, "rewrite golden files")

const schemaSnapshotQuery = `
SELECT type, name, tbl_name, sql
FROM sqlite_schema
WHERE name NOT LIKE 'sqlite\_%' ESCAPE '\'
  AND name <> 'schema_migration'
  AND name NOT IN (SELECT name FROM pragma_table_list WHERE type = 'shadow')
  AND sql IS NOT NULL
ORDER BY CASE type WHEN 'table' THEN 0 WHEN 'index' THEN 1 WHEN 'trigger' THEN 2 ELSE 3 END, name`

func isASCIIWhitespace(b byte) bool {
	switch b {
	case ' ', '\t', '\n', '\r', '\f', '\v':
		return true
	default:
		return false
	}
}

func isSimpleSQLIdent(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		c := s[i]
		if i == 0 {
			if (c < 'A' || c > 'Z') && (c < 'a' || c > 'z') && c != '_' {
				return false
			}
		} else {
			if (c < 'A' || c > 'Z') && (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '_' {
				return false
			}
		}
	}
	return true
}

func canonicalizeSQL(text string) string {
	var out strings.Builder
	inQuote := false
	pendingSpace := false
	var lastByte byte
	for i := 0; i < len(text); i++ {
		b := text[i]
		if inQuote {
			out.WriteByte(b)
			lastByte = b
			if b == '\'' {
				if i+1 < len(text) && text[i+1] == '\'' {
					out.WriteByte('\'')
					i++
					continue
				}
				inQuote = false
			}
			continue
		}
		if isASCIIWhitespace(b) {
			pendingSpace = true
			continue
		}
		if b == ',' || b == ')' {
			pendingSpace = false
			out.WriteByte(b)
			lastByte = b
			continue
		}
		if pendingSpace && out.Len() > 0 && lastByte != '(' {
			out.WriteByte(' ')
		}
		pendingSpace = false
		out.WriteByte(b)
		lastByte = b
		if b == '\'' {
			inQuote = true
		}
	}
	s := out.String()
	const prefix = `CREATE TABLE "`
	if strings.HasPrefix(s, prefix) {
		rest := s[len(prefix):]
		if closeIdx := strings.IndexByte(rest, '"'); closeIdx > 0 {
			ident := rest[:closeIdx]
			if isSimpleSQLIdent(ident) {
				s = "CREATE TABLE " + ident + rest[closeIdx+1:]
			}
		}
	}
	return s
}

func renderCanonicalSchemaSnapshot(t *testing.T, db *sql.DB) string {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), schemaSnapshotQuery)
	if err != nil {
		t.Fatalf("query schema: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out strings.Builder
	for rows.Next() {
		var objectType, name, table, createSQL string
		if err := rows.Scan(&objectType, &name, &table, &createSQL); err != nil {
			t.Fatalf("scan schema row: %v", err)
		}
		fmt.Fprintf(&out, "%s %s on %s\n%s\n\n", objectType, name, table, canonicalizeSQL(createSQL))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate schema rows: %v", err)
	}
	return out.String()
}

func canonicalizeGoldenSnapshot(raw string) string {
	blocks := strings.Split(strings.TrimSpace(raw), "\n\n")
	var out strings.Builder
	for _, block := range blocks {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		header, body, ok := strings.Cut(block, "\n")
		if !ok {
			out.WriteString(block)
			out.WriteString("\n\n")
			continue
		}
		fmt.Fprintf(&out, "%s\n%s\n\n", header, canonicalizeSQL(body))
	}
	return out.String()
}

func firstLineDifference(got, want string) string {
	gotLines := strings.Split(got, "\n")
	wantLines := strings.Split(want, "\n")
	for i := 0; i < len(gotLines) || i < len(wantLines); i++ {
		var g, w string
		if i < len(gotLines) {
			g = gotLines[i]
		}
		if i < len(wantLines) {
			w = wantLines[i]
		}
		if g != w {
			return fmt.Sprintf("first difference at line %d (got %d lines, want %d lines)\n got: %s\nwant: %s", i+1, len(gotLines), len(wantLines), g, w)
		}
	}
	return "no line differs"
}

func newFreshMigratedDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sqlite.OpenAndMigrate(t.Context(), filepath.Join(t.TempDir(), "aura.db"), sqlite.Options{}, Migrations())
	if err != nil {
		t.Fatalf("OpenAndMigrate: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestFreshDatabaseMatchesTheSchemaSnapshot(t *testing.T) {
	db := newFreshMigratedDB(t)
	got := renderCanonicalSchemaSnapshot(t, db)

	goldenPath := filepath.Join("..", "..", "testdata", "golden", "schema.txt")
	wantRaw, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read %s: %v", goldenPath, err)
	}
	want := canonicalizeGoldenSnapshot(string(wantRaw))
	if got != want {
		t.Fatalf("%s differs from fresh squashed schema\n%s", goldenPath, firstLineDifference(got, want))
	}
}

type tableColumn struct {
	cid     int
	name    string
	colType string
	notNull int
	dflt    sql.NullString
	pk      int
	hidden  int
}

type foreignKey struct {
	table    string
	from     string
	to       string
	onUpdate string
	onDelete string
	match    string
}

type indexSpec struct {
	name    string
	unique  int
	origin  string
	partial int
	columns []indexColumn
}

type indexColumn struct {
	seqno int
	cid   int
	name  sql.NullString
	desc  int
	coll  string
	key   int
}

func readTableXInfo(t *testing.T, db *sql.DB, table string) []tableColumn {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), `PRAGMA table_xinfo(`+table+`)`)
	if err != nil {
		t.Fatalf("table_xinfo(%s): %v", table, err)
	}
	defer func() { _ = rows.Close() }()
	var cols []tableColumn
	for rows.Next() {
		var c tableColumn
		if err := rows.Scan(&c.cid, &c.name, &c.colType, &c.notNull, &c.dflt, &c.pk, &c.hidden); err != nil {
			t.Fatalf("scan table_xinfo(%s): %v", table, err)
		}
		cols = append(cols, c)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("table_xinfo(%s) rows: %v", table, err)
	}
	return cols
}

func readForeignKeys(t *testing.T, db *sql.DB, table string) []string {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), `PRAGMA foreign_key_list(`+table+`)`)
	if err != nil {
		t.Fatalf("foreign_key_list(%s): %v", table, err)
	}
	defer func() { _ = rows.Close() }()
	byID := map[int][]foreignKey{}
	for rows.Next() {
		var id, seq int
		var fk foreignKey
		if err := rows.Scan(&id, &seq, &fk.table, &fk.from, &fk.to, &fk.onUpdate, &fk.onDelete, &fk.match); err != nil {
			t.Fatalf("scan foreign_key_list(%s): %v", table, err)
		}
		byID[id] = append(byID[id], fk)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("foreign_key_list(%s) rows: %v", table, err)
	}
	sigs := make([]string, 0, len(byID))
	for _, group := range byID {
		sigs = append(sigs, fmt.Sprintf("%+v", group))
	}
	slices.Sort(sigs)
	return sigs
}

func readIndexColumns(t *testing.T, db *sql.DB, indexName string) []indexColumn {
	t.Helper()
	xrows, err := db.QueryContext(t.Context(), `PRAGMA index_xinfo(`+indexName+`)`)
	if err != nil {
		t.Fatalf("index_xinfo(%s): %v", indexName, err)
	}
	defer func() { _ = xrows.Close() }()
	var cols []indexColumn
	for xrows.Next() {
		var c indexColumn
		if err := xrows.Scan(&c.seqno, &c.cid, &c.name, &c.desc, &c.coll, &c.key); err != nil {
			t.Fatalf("scan index_xinfo(%s): %v", indexName, err)
		}
		cols = append(cols, c)
	}
	if err := xrows.Err(); err != nil {
		t.Fatalf("index_xinfo(%s) rows: %v", indexName, err)
	}
	return cols
}

func readIndexSpecs(t *testing.T, db *sql.DB, table string) []string {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), `PRAGMA index_list(`+table+`)`)
	if err != nil {
		t.Fatalf("index_list(%s): %v", table, err)
	}
	defer func() { _ = rows.Close() }()
	var specs []indexSpec
	for rows.Next() {
		var seq int
		var s indexSpec
		if err := rows.Scan(&seq, &s.name, &s.unique, &s.origin, &s.partial); err != nil {
			t.Fatalf("scan index_list(%s): %v", table, err)
		}
		specs = append(specs, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("index_list(%s) rows: %v", table, err)
	}
	sigs := make([]string, 0, len(specs))
	for i := range specs {
		specs[i].columns = readIndexColumns(t, db, specs[i].name)
		displayName := specs[i].name
		if strings.HasPrefix(displayName, "sqlite_autoindex_") {
			displayName = "<autoindex>"
		}
		sigs = append(sigs, fmt.Sprintf("name=%s unique=%d origin=%s partial=%d cols=%+v", displayName, specs[i].unique, specs[i].origin, specs[i].partial, specs[i].columns))
	}
	slices.Sort(sigs)
	return sigs
}

func listSchemaObjectsByType(t *testing.T, db *sql.DB, objType string) []string {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), `
SELECT name FROM sqlite_schema
WHERE type = ?
  AND name NOT LIKE 'sqlite\_%' ESCAPE '\'
  AND name <> 'schema_migration'
  AND name NOT IN (SELECT name FROM pragma_table_list WHERE type = 'shadow')
  AND sql IS NOT NULL
ORDER BY name`, objType)
	if err != nil {
		t.Fatalf("list %s: %v", objType, err)
	}
	defer func() { _ = rows.Close() }()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan %s: %v", objType, err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows %s: %v", objType, err)
	}
	return names
}

func buildReferenceDBFromGoldenSchema(t *testing.T) *sql.DB {
	t.Helper()
	goldenPath := filepath.Join("..", "..", "testdata", "golden", "schema.txt")
	raw, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read %s: %v", goldenPath, err)
	}
	db, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "reference.db"), sqlite.Options{})
	if err != nil {
		t.Fatalf("Open reference db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.ExecContext(t.Context(), `PRAGMA foreign_keys = OFF`); err != nil {
		t.Fatalf("disable FKs for reference load: %v", err)
	}
	for _, block := range strings.Split(strings.TrimSpace(string(raw)), "\n\n") {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		_, stmt, ok := strings.Cut(block, "\n")
		if !ok {
			t.Fatalf("malformed schema block: %q", block)
		}
		if _, err := db.ExecContext(t.Context(), stmt); err != nil {
			t.Fatalf("exec reference DDL %q: %v", stmt, err)
		}
	}
	if _, err := db.ExecContext(t.Context(), `PRAGMA foreign_keys = ON`); err != nil {
		t.Fatalf("re-enable FKs for reference db: %v", err)
	}
	return db
}

func TestFreshDatabaseStructuralParity(t *testing.T) {
	gotDB := newFreshMigratedDB(t)
	wantDB := buildReferenceDBFromGoldenSchema(t)

	gotTables := listSchemaObjectsByType(t, gotDB, "table")
	wantTables := listSchemaObjectsByType(t, wantDB, "table")
	if len(gotTables) != 54 || !slices.Equal(gotTables, wantTables) {
		t.Fatalf("tables (%d vs %d): got=%v want=%v", len(gotTables), len(wantTables), gotTables, wantTables)
	}
	gotIndexes := listSchemaObjectsByType(t, gotDB, "index")
	wantIndexes := listSchemaObjectsByType(t, wantDB, "index")
	if len(gotIndexes) != 30 || !slices.Equal(gotIndexes, wantIndexes) {
		t.Fatalf("indexes (%d vs %d): got=%v want=%v", len(gotIndexes), len(wantIndexes), gotIndexes, wantIndexes)
	}
	gotTriggers := listSchemaObjectsByType(t, gotDB, "trigger")
	wantTriggers := listSchemaObjectsByType(t, wantDB, "trigger")
	if len(gotTriggers) != 12 || !slices.Equal(gotTriggers, wantTriggers) {
		t.Fatalf("triggers (%d vs %d): got=%v want=%v", len(gotTriggers), len(wantTriggers), gotTriggers, wantTriggers)
	}

	for _, table := range wantTables {
		t.Run(table, func(t *testing.T) {
			gotCols := readTableXInfo(t, gotDB, table)
			wantCols := readTableXInfo(t, wantDB, table)
			if !slices.Equal(gotCols, wantCols) {
				t.Errorf("table_xinfo(%s) mismatch:\n got: %+v\nwant: %+v", table, gotCols, wantCols)
			}
			gotFKs := readForeignKeys(t, gotDB, table)
			wantFKs := readForeignKeys(t, wantDB, table)
			if !slices.Equal(gotFKs, wantFKs) {
				t.Errorf("foreign_key_list(%s) mismatch:\n got: %v\nwant: %v", table, gotFKs, wantFKs)
			}
			gotIdx := readIndexSpecs(t, gotDB, table)
			wantIdx := readIndexSpecs(t, wantDB, table)
			if !slices.Equal(gotIdx, wantIdx) {
				t.Errorf("index_list/xinfo(%s) mismatch:\n got: %v\nwant: %v", table, gotIdx, wantIdx)
			}
		})
	}
}

func renderMigrationChecksums() string {
	var out strings.Builder
	for _, m := range Migrations() {
		fmt.Fprintf(&out, "%d %s %s\n", m.Version, m.Owner, sqlite.Checksum(m.SQL))
	}
	return out.String()
}

func TestMigrationHistoryMatchesItsChecksums(t *testing.T) {
	goldenPath := filepath.Join("..", "..", "testdata", "golden", "migration-checksums.txt")
	got := renderMigrationChecksums()
	if *updateGolden {
		if err := os.WriteFile(goldenPath, []byte(got), 0o600); err != nil {
			t.Fatalf("write %s: %v", goldenPath, err)
		}
		return
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read %s: %v", goldenPath, err)
	}
	if got != string(want) {
		t.Fatalf("%s differs from the rendering; rerun with -update to regenerate\n%s", goldenPath, firstLineDifference(got, string(want)))
	}
}

func assertTableFKParentsExist(t *testing.T, db *sql.DB, version int, owner, table string, present map[string]bool) {
	t.Helper()
	fkRows, err := db.QueryContext(t.Context(), `PRAGMA foreign_key_list(`+table+`)`)
	if err != nil {
		t.Fatalf("foreign_key_list(%s): %v", table, err)
	}
	defer func() { _ = fkRows.Close() }()
	for fkRows.Next() {
		var id, seq int
		var parent, from, to, onUpdate, onDelete, match string
		if err := fkRows.Scan(&id, &seq, &parent, &from, &to, &onUpdate, &onDelete, &match); err != nil {
			t.Fatalf("scan foreign_key_list(%s): %v", table, err)
		}
		if !present[parent] {
			t.Errorf("v%d (%s) table %s references %s before %s is created", version, owner, table, parent, parent)
		}
	}
	if err := fkRows.Err(); err != nil {
		t.Fatalf("foreign_key_list(%s) rows: %v", table, err)
	}
}

func TestAll22OwnersRegisteredAndForeignKeysValid(t *testing.T) {
	owners := Schemas()
	wantOwners := []string{
		"turn", "durable", "effect", "usage", "principal", "model", "storage", "webhook", "knowledge", "memory", "profile",
		"schedule", "broadcast", "workflow", "handoff", "skills", "plugin", "child", "persona_eval", "channel_resume", "telegram", "email",
	}
	gotOwners := make([]string, 0, len(owners))
	for _, owner := range owners {
		gotOwners = append(gotOwners, owner.SchemaName())
	}
	if !slices.Equal(gotOwners, wantOwners) {
		t.Fatalf("Schemas() owners = %v, want %v", gotOwners, wantOwners)
	}
	migrations := Migrations()
	if want := len(wantOwners) + len(incrementalSteps(t)); len(migrations) != want {
		t.Fatalf("len(Migrations()) = %d, want %d", len(migrations), want)
	}

	ctx := t.Context()
	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "owners.db"), sqlite.Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	tableOwners := map[string]string{}
	for i, m := range migrations {
		if m.Version != i+1 {
			t.Fatalf("migration[%d].Version = %d, want %d", i, m.Version, i+1)
		}
		if err := sqlite.Migrate(ctx, db, migrations[:i+1]); err != nil {
			t.Fatalf("Migrate through v%d (%s): %v", m.Version, m.Owner, err)
		}
		currentTables := listSchemaObjectsByType(t, db, "table")
		present := make(map[string]bool, len(currentTables))
		for _, tbl := range currentTables {
			present[tbl] = true
		}
		for _, tbl := range currentTables {
			if _, seen := tableOwners[tbl]; !seen {
				tableOwners[tbl] = m.Owner
				assertTableFKParentsExist(t, db, m.Version, m.Owner, tbl, present)
			}
		}
	}
	if len(tableOwners) != 54 {
		t.Fatalf("owned tables = %d, want 54", len(tableOwners))
	}
	finalTables := listSchemaObjectsByType(t, db, "table")
	for _, tbl := range finalTables {
		if tableOwners[tbl] == "" {
			t.Errorf("table %s has no owner", tbl)
		}
	}

	rows, err := db.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatalf("PRAGMA foreign_key_check: %v", err)
	}
	defer func() { _ = rows.Close() }()
	if rows.Next() {
		t.Fatal("PRAGMA foreign_key_check reported foreign key violations")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("PRAGMA foreign_key_check rows: %v", err)
	}
}

func sha256FileApp(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

type dirSnapshot struct {
	mode    os.FileMode
	entries map[string]string
	modes   map[string]os.FileMode
}

func snapshotDir(t *testing.T, dir string) dirSnapshot {
	t.Helper()
	dirInfo, err := os.Lstat(dir)
	if err != nil {
		t.Fatalf("lstat %s: %v", dir, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir %s: %v", dir, err)
	}
	snap := dirSnapshot{
		mode:    dirInfo.Mode().Perm(),
		entries: make(map[string]string, len(entries)),
		modes:   make(map[string]os.FileMode, len(entries)),
	}
	for _, e := range entries {
		full := filepath.Join(dir, e.Name())
		info, err := os.Lstat(full)
		if err != nil {
			t.Fatalf("lstat %s: %v", full, err)
		}
		snap.modes[e.Name()] = info.Mode().Perm()
		if info.Mode().IsRegular() {
			snap.entries[e.Name()] = sha256FileApp(t, full)
		} else {
			snap.entries[e.Name()] = "<dir>"
		}
	}
	return snap
}

func assertDirUnchanged(t *testing.T, dir string, before dirSnapshot) {
	t.Helper()
	after := snapshotDir(t, dir)
	if after.mode != before.mode {
		t.Fatalf("dir mode mutated: %o != %o", after.mode, before.mode)
	}
	if len(after.entries) != len(before.entries) {
		t.Fatalf("dir entries mutated: after=%v before=%v", after.entries, before.entries)
	}
	for name, hash := range before.entries {
		if after.entries[name] != hash {
			t.Fatalf("file %s content mutated: %s != %s", name, after.entries[name], hash)
		}
		if after.modes[name] != before.modes[name] {
			t.Fatalf("file %s mode mutated: %o != %o", name, after.modes[name], before.modes[name])
		}
	}
}

func seedPreSquashDB(ctx context.Context, t *testing.T, path string, maxVersion int, walNoSHM, unversioned bool) {
	t.Helper()
	workDir := t.TempDir()
	workPath := filepath.Join(workDir, "work.db")
	dsn := (&url.URL{Scheme: "file", Path: workPath}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if walNoSHM {
		if _, err := db.ExecContext(ctx, `PRAGMA journal_mode = WAL; PRAGMA wal_autocheckpoint = 0;`); err != nil {
			_ = db.Close()
			t.Fatalf("set WAL: %v", err)
		}
	} else {
		if _, err := db.ExecContext(ctx, `PRAGMA journal_mode = DELETE;`); err != nil {
			_ = db.Close()
			t.Fatalf("set DELETE: %v", err)
		}
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE session (id TEXT PRIMARY KEY, owner_id TEXT NOT NULL);`); err != nil {
		_ = db.Close()
		t.Fatalf("create session: %v", err)
	}
	if !unversioned {
		if _, err := db.ExecContext(ctx, `CREATE TABLE schema_migration (version INTEGER PRIMARY KEY, checksum TEXT NOT NULL, applied_at TEXT NOT NULL);`); err != nil {
			_ = db.Close()
			t.Fatalf("create schema_migration: %v", err)
		}
		for v := 1; v <= maxVersion; v++ {
			if _, err := db.ExecContext(ctx, `INSERT INTO schema_migration (version, checksum, applied_at) VALUES (?, ?, '2026-08-01T00:00:00Z')`, v, fmt.Sprintf("legacy-%d", v)); err != nil {
				_ = db.Close()
				t.Fatalf("insert v%d: %v", v, err)
			}
		}
	}

	if walNoSHM {
		dbBytes, err := os.ReadFile(workPath)
		if err != nil {
			_ = db.Close()
			t.Fatalf("read work db: %v", err)
		}
		walBytes, err := os.ReadFile(workPath + "-wal")
		if err != nil {
			_ = db.Close()
			t.Fatalf("read work wal: %v", err)
		}
		if len(walBytes) == 0 {
			_ = db.Close()
			t.Fatal("expected non-empty -wal in fixture")
		}
		if err := db.Close(); err != nil {
			t.Fatalf("close work db: %v", err)
		}
		if err := os.WriteFile(path, dbBytes, 0o600); err != nil {
			t.Fatalf("write target db: %v", err)
		}
		if err := os.WriteFile(path+"-wal", walBytes, 0o600); err != nil {
			t.Fatalf("write target wal: %v", err)
		}
		return
	}

	if _, err := db.ExecContext(ctx, `VACUUM INTO ?`, path); err != nil {
		_ = db.Close()
		t.Fatalf("VACUUM INTO: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close work db: %v", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatalf("chmod target db: %v", err)
	}
}

func TestPreSquashDatabaseIsRejectedAndLeftByteIdentical(t *testing.T) {
	ctx := t.Context()

	fixtures := []struct {
		name        string
		maxVersion  int
		walNoSHM    bool
		unversioned bool
		dirMode     os.FileMode
		fileMode    os.FileMode
	}{
		{name: "rollback-v1", maxVersion: 1, dirMode: 0o700, fileMode: 0o600},
		{name: "rollback-v48", maxVersion: 48, dirMode: 0o700, fileMode: 0o600},
		{name: "wal-v48-no-shm", maxVersion: 48, walNoSHM: true, dirMode: 0o700, fileMode: 0o600},
		{name: "unversioned", unversioned: true, dirMode: 0o700, fileMode: 0o600},
		{name: "relaxed-perms-0644-0755", maxVersion: 48, dirMode: 0o755, fileMode: 0o644},
	}

	for _, tc := range fixtures {
		t.Run(tc.name, func(t *testing.T) {
			dataRoot := filepath.Join(t.TempDir(), "data")
			if err := os.MkdirAll(dataRoot, tc.dirMode); err != nil {
				t.Fatalf("mkdir dataRoot: %v", err)
			}
			dbPath := filepath.Join(dataRoot, "aura.db")
			seedPreSquashDB(ctx, t, dbPath, tc.maxVersion, tc.walNoSHM, tc.unversioned)
			if err := os.Chmod(dbPath, tc.fileMode); err != nil {
				t.Fatalf("chmod db: %v", err)
			}
			if err := os.Chmod(dataRoot, tc.dirMode); err != nil {
				t.Fatalf("chmod dir: %v", err)
			}

			before := snapshotDir(t, dataRoot)

			if err := sqlite.CheckCompatibility(ctx, dbPath); !fault.HasCode(err, sqlite.ErrorCodeSchemaIncompatible) {
				t.Fatalf("CheckCompatibility = %v, want %s", err, sqlite.ErrorCodeSchemaIncompatible)
			}
			assertDirUnchanged(t, dataRoot, before)

			if _, err := sqlite.OpenAndMigrate(ctx, dbPath, sqlite.Options{}, Migrations()); !fault.HasCode(err, sqlite.ErrorCodeSchemaIncompatible) {
				t.Fatalf("OpenAndMigrate = %v, want %s", err, sqlite.ErrorCodeSchemaIncompatible)
			}
			assertDirUnchanged(t, dataRoot, before)

			cfg := appconfig.Default()
			cfg.Storage.Path = dataRoot
			if _, err := openStorage(ctx, &cfg); !fault.HasCode(err, sqlite.ErrorCodeSchemaIncompatible) {
				t.Fatalf("openStorage = %v, want %s", err, sqlite.ErrorCodeSchemaIncompatible)
			}
			assertDirUnchanged(t, dataRoot, before)

			if _, err := Build(ctx, &Options{
				Surface:   SurfaceCommand,
				Config:    &appconfig.LoadResult{Config: &cfg},
				LogLevel:  "warn",
				LogFormat: "text",
				LogOutput: io.Discard,
			}); !fault.HasCode(err, sqlite.ErrorCodeSchemaIncompatible) {
				t.Fatalf("app.Build(SurfaceCommand) = %v, want %s", err, sqlite.ErrorCodeSchemaIncompatible)
			}
			assertDirUnchanged(t, dataRoot, before)

			backupRoot := filepath.Join(t.TempDir(), "backups")
			if _, err := storage.UpgradePath(ctx, dbPath, sqlite.Options{}, Migrations(), backupRoot, dataRoot, time.Now()); !fault.HasCode(err, sqlite.ErrorCodeSchemaIncompatible) {
				t.Fatalf("storage.UpgradePath = %v, want %s", err, sqlite.ErrorCodeSchemaIncompatible)
			}
			if _, err := os.Stat(backupRoot); !os.IsNotExist(err) {
				t.Fatalf("storage.UpgradePath created backup directory: %v", err)
			}
			assertDirUnchanged(t, dataRoot, before)

			backupDir := filepath.Join(t.TempDir(), "pre-squash-backup")
			if err := os.MkdirAll(backupDir, 0o700); err != nil {
				t.Fatalf("mkdir backupDir: %v", err)
			}
			rawDB, err := os.ReadFile(dbPath)
			if err != nil {
				t.Fatalf("read db: %v", err)
			}
			if err := os.WriteFile(filepath.Join(backupDir, "aura.db"), rawDB, 0o600); err != nil {
				t.Fatalf("write backup db: %v", err)
			}
			if tc.walNoSHM {
				rawWAL, err := os.ReadFile(dbPath + "-wal")
				if err != nil {
					t.Fatalf("read wal: %v", err)
				}
				if err := os.WriteFile(filepath.Join(backupDir, "aura.db-wal"), rawWAL, 0o600); err != nil {
					t.Fatalf("write backup wal: %v", err)
				}
			}
			manifestBytes, err := json.Marshal(storage.BackupManifest{CreatedAt: time.Now().UTC()})
			if err != nil {
				t.Fatalf("marshal manifest: %v", err)
			}
			if err := os.WriteFile(filepath.Join(backupDir, "manifest.json"), manifestBytes, 0o600); err != nil {
				t.Fatalf("write manifest: %v", err)
			}
			backupBefore := snapshotDir(t, backupDir)

			if _, err := storage.VerifyRestore(ctx, backupDir, dataRoot); !fault.HasCode(err, sqlite.ErrorCodeSchemaIncompatible) {
				t.Fatalf("storage.VerifyRestore = %v, want %s", err, sqlite.ErrorCodeSchemaIncompatible)
			}
			assertDirUnchanged(t, backupDir, backupBefore)

			restoreTarget := filepath.Join(t.TempDir(), "restored.db")
			if _, err := storage.Restore(ctx, backupDir, dataRoot, restoreTarget, storage.RestoreOptions{Force: true}); !fault.HasCode(err, sqlite.ErrorCodeSchemaIncompatible) {
				t.Fatalf("storage.Restore = %v, want %s", err, sqlite.ErrorCodeSchemaIncompatible)
			}
			if _, err := os.Stat(restoreTarget); !os.IsNotExist(err) {
				t.Fatalf("storage.Restore wrote live db despite schema_incompatible: %v", err)
			}
			assertDirUnchanged(t, backupDir, backupBefore)
			assertDirUnchanged(t, dataRoot, before)
		})
	}

	t.Run("in-memory pre-squash UpgradeWithBackup", func(t *testing.T) {
		memDB, err := sqlite.Open(ctx, ":memory:", sqlite.Options{})
		if err != nil {
			t.Fatalf("Open memory: %v", err)
		}
		t.Cleanup(func() { _ = memDB.Close() })
		if _, err := memDB.ExecContext(ctx, `
CREATE TABLE schema_migration (version INTEGER PRIMARY KEY, checksum TEXT NOT NULL, applied_at TEXT NOT NULL);
INSERT INTO schema_migration (version, checksum, applied_at) VALUES (1, 'legacy', '2026-08-01T00:00:00Z');
`); err != nil {
			t.Fatalf("seed memory pre-squash: %v", err)
		}
		backupRoot := filepath.Join(t.TempDir(), "backups")
		if _, err := storage.UpgradeWithBackup(ctx, memDB, Migrations(), backupRoot, t.TempDir(), time.Now()); !fault.HasCode(err, sqlite.ErrorCodeSchemaIncompatible) {
			t.Fatalf("UpgradeWithBackup = %v, want %s", err, sqlite.ErrorCodeSchemaIncompatible)
		}
		if _, err := os.Stat(backupRoot); !os.IsNotExist(err) {
			t.Fatalf("UpgradeWithBackup created backup directory: %v", err)
		}
	})
}

func TestMigrateAppliesFoundationalSchema(t *testing.T) {
	db := newFreshMigratedDB(t)
	for _, table := range []string{"schema_migration", "session", "runtime_event", "blob", "artifact_ref", "ingress_dedupe"} {
		var name string
		err := db.QueryRowContext(context.Background(),
			`SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, table,
		).Scan(&name)
		if err != nil {
			t.Errorf("table %s missing: %v", table, err)
		}
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	db := newFreshMigratedDB(t)
	if err := sqlite.Migrate(t.Context(), db, Migrations()); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
}

func TestMigrateChecksumMismatch(t *testing.T) {
	db := newFreshMigratedDB(t)
	if _, err := db.ExecContext(t.Context(), `UPDATE schema_migration SET checksum = 'tampered' WHERE version = 1`); err != nil {
		t.Fatalf("tamper checksum: %v", err)
	}
	err := sqlite.Migrate(t.Context(), db, Migrations())
	if !fault.HasCode(err, sqlite.ErrorCodeMigrationChecksumMismatch) {
		t.Fatalf("Migrate = %v, want %s", err, sqlite.ErrorCodeMigrationChecksumMismatch)
	}
}

func TestMigrateRejectsVersionAboveBinary(t *testing.T) {
	db := newFreshMigratedDB(t)
	migrations := Migrations()
	if _, err := db.ExecContext(t.Context(),
		`INSERT INTO schema_migration (version, owner, checksum, applied_at) VALUES (?, ?, ?, ?)`,
		migrations[len(migrations)-1].Version+1, "email", "future", "2026-08-01T00:00:00Z",
	); err != nil {
		t.Fatalf("insert future migration: %v", err)
	}
	err := sqlite.Migrate(t.Context(), db, migrations)
	if !fault.HasCode(err, sqlite.ErrorCodeMigrationSequenceInvalid) {
		t.Fatalf("Migrate = %v, want %s", err, sqlite.ErrorCodeMigrationSequenceInvalid)
	}
}

func incrementalSteps(t *testing.T) []sqlite.Migration {
	t.Helper()
	var steps []sqlite.Migration
	for _, owner := range Schemas() {
		for _, step := range owner.SchemaSteps() {
			steps = append(steps, sqlite.Migration{Version: step.Version, Owner: owner.SchemaName(), SQL: step.SQL})
		}
	}
	slices.SortFunc(steps, func(a, b sqlite.Migration) int { return a.Version - b.Version })
	return steps
}

func TestIncrementalStepsAreUniqueAndContiguous(t *testing.T) {
	steps := incrementalSteps(t)
	if len(steps) < 1 {
		t.Fatal("no owner declares an incremental step")
	}
	baselines := len(Schemas())
	for i, step := range steps {
		if want := baselines + 1 + i; step.Version != want {
			t.Fatalf("step %d (%s) has version %d, want %d: pinned versions must be unique and contiguous from %d", i, step.Owner, step.Version, want, baselines+1)
		}
	}
	history := Migrations()
	if tail := history[baselines:]; !slices.Equal(tail, steps) {
		t.Fatalf("Migrations() does not end with the owners' steps in version order: %+v", tail)
	}
}

type replyTargetRow struct {
	channel, account, channelID, threadID sql.NullString
}

const (
	runTargetQuery      = `SELECT reply_channel, reply_account, reply_channel_id, reply_thread_id FROM workflow_run WHERE id = ?`
	proposalTargetQuery = `SELECT reply_channel, reply_account, reply_channel_id, reply_thread_id FROM workflow_proposal WHERE id = ?`
)

func readReplyTarget(t *testing.T, row *sql.Row) replyTargetRow {
	t.Helper()
	var target replyTargetRow
	if err := row.Scan(&target.channel, &target.account, &target.channelID, &target.threadID); err != nil {
		t.Fatalf("read reply target: %v", err)
	}
	return target
}

func TestReplyTargetStepBackfillsAndConstrains(t *testing.T) {
	ctx := t.Context()
	history := Migrations()
	const baselines = 22
	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "reply-target.db"), sqlite.Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := sqlite.Migrate(ctx, db, history[:baselines]); err != nil {
		t.Fatalf("Migrate baselines: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO workflow_definition (id, version, goal, source, spec_json, spec_sha256, created_at) VALUES ('wf', 1, 'goal', 'defined', '{}', 'digest', '2026-11-27T09:00:00Z')`); err != nil {
		t.Fatalf("seed definition: %v", err)
	}
	for _, run := range []struct{ id, account, channelID string }{
		{"run-full", "front", "333"},
		{"run-account-only", "front", ""},
		{"run-empty", "", ""},
	} {
		if _, err := db.ExecContext(ctx, `INSERT INTO workflow_run (id, definition_id, definition_version, goal, input_json, status, created_at, updated_at, reply_account, reply_channel_id)
			VALUES (?1, 'wf', 1, 'goal', '{}', 'succeeded', '2026-11-27T09:00:00Z', '2026-11-27T09:00:00Z', NULLIF(?2, ''), NULLIF(?3, ''))`, run.id, run.account, run.channelID); err != nil {
			t.Fatalf("seed %s: %v", run.id, err)
		}
	}
	if err := sqlite.Migrate(ctx, db, history); err != nil {
		t.Fatalf("Migrate step: %v", err)
	}

	backfilled := readReplyTarget(t, db.QueryRowContext(ctx, runTargetQuery, "run-full"))
	if backfilled.channel.String != "discord" || backfilled.account.String != "front" || backfilled.channelID.String != "333" || backfilled.threadID.Valid {
		t.Fatalf("a Discord target was not backfilled: %+v", backfilled)
	}
	for _, id := range []string{"run-account-only", "run-empty"} {
		if got := readReplyTarget(t, db.QueryRowContext(ctx, runTargetQuery, id)); got != (replyTargetRow{}) {
			t.Fatalf("%s kept a partial or empty target: %+v", id, got)
		}
	}

	seedPersonaSession(t, db, "ops/session", "local-owner", "ops")
	const insertRun = `INSERT INTO workflow_run (id, definition_id, definition_version, goal, input_json, status, created_at, updated_at, reply_channel, reply_account, reply_channel_id, reply_thread_id)
		VALUES (?1, 'wf', 1, 'goal', '{}', 'queued', '2026-11-27T09:00:00Z', '2026-11-27T09:00:00Z', NULLIF(?2, ''), NULLIF(?3, ''), NULLIF(?4, ''), NULLIF(?5, ''))`
	const insertProposal = `INSERT INTO workflow_proposal (id, session_id, agent_id, principal_id, spec_json, spec_sha256, status, created_at, reply_channel, reply_account, reply_channel_id, reply_thread_id)
		VALUES (?1, 'ops/session', 'ops', 'local-owner', '{}', 'digest', 'proposed', '2026-11-27T09:00:00Z', NULLIF(?2, ''), NULLIF(?3, ''), NULLIF(?4, ''), NULLIF(?5, ''))`

	partial := []struct {
		name                                  string
		channel, account, channelID, threadID string
		runCode                               sqlite.ErrorCode
	}{
		{"channel without account", "discord", "", "333", "", sqlite.ErrorCodeConstraintCheck},
		{"channel without channel id", "telegram", "pocket", "", "", sqlite.ErrorCodeConstraintCheck},
		{"thread without channel", "", "", "", "7", sqlite.ErrorCodeConstraintCheck},
		{"account without channel", "", "pocket", "", "", sqlite.ErrorCodeConstraint},
		{"channel id without channel", "", "", "333", "", sqlite.ErrorCodeConstraint},
		{"unsupported channel", "email", "mail", "guest", "", sqlite.ErrorCodeConstraintCheck},
	}
	for _, tc := range partial {
		t.Run("run insert refuses "+tc.name, func(t *testing.T) {
			_, err := db.ExecContext(ctx, insertRun, "run-bad", tc.channel, tc.account, tc.channelID, tc.threadID)
			if !fault.HasCode(sqlite.Classify(err), tc.runCode) {
				t.Fatalf("insert = %v, want %s", err, tc.runCode)
			}
		})
		t.Run("proposal insert refuses "+tc.name, func(t *testing.T) {
			_, err := db.ExecContext(ctx, insertProposal, "prop-bad", tc.channel, tc.account, tc.channelID, tc.threadID)
			if !fault.HasCode(sqlite.Classify(err), sqlite.ErrorCodeConstraintCheck) {
				t.Fatalf("insert = %v, want %s", err, sqlite.ErrorCodeConstraintCheck)
			}
		})
	}

	updates := []struct {
		name, statement string
		code            sqlite.ErrorCode
	}{
		{"channel without account", `UPDATE workflow_run SET reply_channel = 'discord', reply_channel_id = '333' WHERE id = 'run-empty'`, sqlite.ErrorCodeConstraintCheck},
		{"channel without channel id", `UPDATE workflow_run SET reply_channel = 'telegram', reply_account = 'pocket' WHERE id = 'run-empty'`, sqlite.ErrorCodeConstraintCheck},
		{"thread without channel", `UPDATE workflow_run SET reply_thread_id = '7' WHERE id = 'run-empty'`, sqlite.ErrorCodeConstraintCheck},
		{"account without channel", `UPDATE workflow_run SET reply_account = 'pocket' WHERE id = 'run-empty'`, sqlite.ErrorCodeConstraint},
		{"channel id without channel", `UPDATE workflow_run SET reply_channel_id = '333' WHERE id = 'run-empty'`, sqlite.ErrorCodeConstraint},
		{"channel cleared under an account", `UPDATE workflow_run SET reply_channel = NULL WHERE id = 'run-full'`, sqlite.ErrorCodeConstraint},
		{"unsupported channel", `UPDATE workflow_run SET reply_channel = 'email' WHERE id = 'run-full'`, sqlite.ErrorCodeConstraintCheck},
	}
	for _, tc := range updates {
		t.Run("run update refuses "+tc.name, func(t *testing.T) {
			_, err := db.ExecContext(ctx, tc.statement)
			if !fault.HasCode(sqlite.Classify(err), tc.code) {
				t.Fatalf("update = %v, want %s", err, tc.code)
			}
		})
	}
	if got := readReplyTarget(t, db.QueryRowContext(ctx, runTargetQuery, "run-empty")); got != (replyTargetRow{}) {
		t.Fatalf("a refused update changed the run: %+v", got)
	}

	for _, tc := range []struct {
		name, channel, account, channelID, threadID string
	}{
		{"without a thread", "discord", "front", "333", ""},
		{"with a topic", "telegram", "pocket", "-1001234567890", "7"},
		{"empty", "", "", "", ""},
	} {
		t.Run("complete target "+tc.name, func(t *testing.T) {
			id := "ok-" + strings.ReplaceAll(tc.name, " ", "-")
			if _, err := db.ExecContext(ctx, insertRun, "run-"+id, tc.channel, tc.account, tc.channelID, tc.threadID); err != nil {
				t.Fatalf("run insert: %v", err)
			}
			if _, err := db.ExecContext(ctx, insertProposal, "prop-"+id, tc.channel, tc.account, tc.channelID, tc.threadID); err != nil {
				t.Fatalf("proposal insert: %v", err)
			}
			want := readReplyTarget(t, db.QueryRowContext(ctx, runTargetQuery, "run-"+id))
			if got := readReplyTarget(t, db.QueryRowContext(ctx, proposalTargetQuery, "prop-"+id)); got != want || want.channel.String != tc.channel || want.threadID.String != tc.threadID {
				t.Fatalf("stored targets differ: run %+v proposal %+v", want, got)
			}
		})
	}
}
