package history

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/platform/sqlite"
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

func collapseWhitespaceOutsideQuotes(text string) string {
	var out strings.Builder
	var quote rune
	pendingSpace := false
	for _, r := range text {
		if quote != 0 {
			out.WriteRune(r)
			if r == quote {
				quote = 0
			}
			continue
		}
		switch r {
		case ' ', '\t', '\n', '\r', '\f', '\v':
			pendingSpace = true
			continue
		}
		if pendingSpace && out.Len() > 0 {
			out.WriteByte(' ')
		}
		pendingSpace = false
		out.WriteRune(r)
		if r == '\'' || r == '"' {
			quote = r
		}
	}
	return out.String()
}

func renderSchemaSnapshot(ctx context.Context, t *testing.T) string {
	t.Helper()
	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "aura.db"), sqlite.Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := sqlite.Migrate(ctx, db, Migrations()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	rows, err := db.QueryContext(ctx, schemaSnapshotQuery)
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
		fmt.Fprintf(&out, "%s %s on %s\n%s\n\n", objectType, name, table, collapseWhitespaceOutsideQuotes(createSQL))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate schema rows: %v", err)
	}
	return out.String()
}

func renderMigrationChecksums() string {
	var out strings.Builder
	for _, m := range Migrations() {
		fmt.Fprintf(&out, "%d %s\n", m.Version, sqlite.Checksum(m.SQL))
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

func compareWithGolden(t *testing.T, goldenPath, got string) {
	t.Helper()
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

func TestFreshDatabaseMatchesTheSchemaSnapshot(t *testing.T) {
	got := renderSchemaSnapshot(context.Background(), t)
	compareWithGolden(t, filepath.Join("..", "..", "..", "testdata", "golden", "schema.txt"), got)
}

func TestMigrationHistoryMatchesItsChecksums(t *testing.T) {
	compareWithGolden(t, filepath.Join("..", "..", "..", "testdata", "golden", "migration-checksums.txt"), renderMigrationChecksums())
}
