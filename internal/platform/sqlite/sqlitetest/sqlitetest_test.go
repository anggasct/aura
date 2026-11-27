package sqlitetest

import (
	"path/filepath"
	"sync"
	"testing"

	"github.com/anggasct/aura/internal/platform/sqlite"
)

func TestOpenMigratedAndSeedCacheTemplatesConcurrently(t *testing.T) {
	ownerA := sqlite.StaticSchema{Name: "alpha", SQL: `CREATE TABLE alpha (id TEXT PRIMARY KEY);`}
	ownerB := sqlite.StaticSchema{Name: "beta", SQL: `CREATE TABLE beta (id TEXT PRIMARY KEY, alpha_id TEXT REFERENCES alpha(id));`}

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			db := OpenMigrated(t, ownerA, ownerB)
			if _, err := db.ExecContext(t.Context(), `INSERT INTO alpha (id) VALUES ('a1')`); err != nil {
				t.Errorf("insert alpha: %v", err)
				return
			}
			if _, err := db.ExecContext(t.Context(), `INSERT INTO beta (id, alpha_id) VALUES ('b1', 'a1')`); err != nil {
				t.Errorf("insert beta: %v", err)
			}
		}()
	}
	wg.Wait()

	dbPath := filepath.Join(t.TempDir(), "seeded.db")
	Seed(t, dbPath, ownerA)
	db, err := sqlite.Open(t.Context(), dbPath, sqlite.Options{})
	if err != nil {
		t.Fatalf("Open seeded: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	var count int
	if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM sqlite_schema WHERE type = 'table' AND name = 'alpha'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("alpha table count = %d, %v; want 1", count, err)
	}
}

func TestOpenMigrationsCachesDistinctKeys(t *testing.T) {
	m1 := []sqlite.Migration{{Version: 1, Owner: "first", SQL: `CREATE TABLE first_tbl (id TEXT PRIMARY KEY);`}}
	m2 := []sqlite.Migration{{Version: 1, Owner: "second", SQL: `CREATE TABLE second_tbl (id TEXT PRIMARY KEY);`}}
	if templateKey(m1) == templateKey(m2) {
		t.Fatal("distinct migration sets collided on template key")
	}

	db1 := OpenMigrations(t, m1)
	db2 := OpenMigrations(t, m2)

	var count int
	if err := db1.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM sqlite_schema WHERE type = 'table' AND name = 'first_tbl'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("first_tbl missing: %d %v", count, err)
	}
	if err := db2.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM sqlite_schema WHERE type = 'table' AND name = 'second_tbl'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("second_tbl missing: %d %v", count, err)
	}
}
