package sqlitetest

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"github.com/anggasct/aura/internal/platform/sqlite"
)

type onceTemplate struct {
	once sync.Once
	data []byte
	err  error
}

var templateCache sync.Map

func Seed(t testing.TB, dbPath string, owners ...sqlite.SchemaOwner) {
	t.Helper()
	migrations, err := sqlite.Baseline(owners...)
	if err != nil {
		t.Fatalf("build baseline migrations: %v", err)
	}
	SeedMigrations(t, dbPath, migrations)
}

func SeedMigrations(t testing.TB, dbPath string, migrations []sqlite.Migration) {
	t.Helper()
	template, err := cachedTemplate(migrations)
	if err != nil {
		t.Fatalf("build migrated template: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		t.Fatalf("create database directory: %v", err)
	}
	file, err := os.OpenFile(dbPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatalf("create seeded database: %v", err)
	}
	if _, err := file.Write(template); err != nil {
		_ = file.Close()
		_ = os.Remove(dbPath)
		t.Fatalf("write seeded database: %v", err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(dbPath)
		t.Fatalf("close seeded database: %v", err)
	}
}

func OpenMigrated(t testing.TB, owners ...sqlite.SchemaOwner) *sql.DB {
	t.Helper()
	migrations, err := sqlite.Baseline(owners...)
	if err != nil {
		t.Fatalf("build baseline migrations: %v", err)
	}
	return OpenMigrations(t, migrations)
}

func OpenMigrations(t testing.TB, migrations []sqlite.Migration) *sql.DB {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "aura.db")
	SeedMigrations(t, dbPath, migrations)
	db, err := sqlite.Open(t.Context(), dbPath, sqlite.Options{})
	if err != nil {
		t.Fatalf("open seeded database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func cachedTemplate(migrations []sqlite.Migration) ([]byte, error) {
	key := templateKey(migrations)
	actual, _ := templateCache.LoadOrStore(key, &onceTemplate{})
	entry, ok := actual.(*onceTemplate)
	if !ok {
		return nil, fmt.Errorf("unexpected template cache entry type %T", actual)
	}
	entry.once.Do(func() {
		entry.data, entry.err = buildTemplate(migrations)
	})
	return entry.data, entry.err
}

func templateKey(migrations []sqlite.Migration) string {
	h := sha256.New()
	for _, m := range migrations {
		_, _ = h.Write([]byte(strconv.Itoa(m.Version)))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(m.Owner))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(sqlite.Checksum(m.SQL)))
		_, _ = h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func buildTemplate(migrations []sqlite.Migration) ([]byte, error) {
	dir, err := os.MkdirTemp("", "aura-sqlitetest-")
	if err != nil {
		return nil, fmt.Errorf("create template directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	ctx := context.Background()
	source, err := sqlite.Open(ctx, filepath.Join(dir, "source.db"), sqlite.Options{})
	if err != nil {
		return nil, fmt.Errorf("open source database: %w", err)
	}
	defer func() { _ = source.Close() }()
	if err := sqlite.Migrate(ctx, source, migrations); err != nil {
		return nil, fmt.Errorf("migrate source database: %w", err)
	}
	snapshot := filepath.Join(dir, "template.db")
	if _, err := source.ExecContext(ctx, "VACUUM INTO ?", snapshot); err != nil {
		return nil, fmt.Errorf("snapshot migrated database: %w", err)
	}
	data, err := os.ReadFile(snapshot)
	if err != nil {
		return nil, fmt.Errorf("read migrated snapshot: %w", err)
	}
	return data, nil
}
