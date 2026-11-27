package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"

	"github.com/anggasct/aura/internal/platform/fault"
)

func CheckCompatibility(ctx context.Context, path string) error {
	if path == "" || path == ":memory:" {
		return nil
	}
	walSize, empty, err := validateRegularPath(path)
	if err != nil {
		return err
	}
	if empty {
		return nil
	}
	if walSize > 0 {
		return checkCompatibilityFromTempCopy(ctx, path)
	}
	dsn := (&url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro&immutable=1"}).String()
	db, err := sql.Open(sqlDriverName, dsn)
	if err != nil {
		return unavailable("open sqlite database for compatibility check", err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	return checkDBCompatibility(ctx, db)
}

func OpenAndMigrate(ctx context.Context, path string, opts Options, history []Migration) (*sql.DB, error) {
	if len(history) == 0 {
		return nil, fault.New(ErrorCodeInvalidArgument, "migration history must not be empty")
	}
	if err := CheckCompatibility(ctx, path); err != nil {
		return nil, err
	}
	db, err := Open(ctx, path, opts)
	if err != nil {
		return nil, err
	}
	if err := Migrate(ctx, db, history); err != nil {
		return nil, errors.Join(err, db.Close())
	}
	return db, nil
}

func validateRegularPath(path string) (walSize int64, empty bool, err error) {
	dirInfo, err := os.Lstat(filepath.Dir(path))
	if errors.Is(err, fs.ErrNotExist) {
		return 0, true, nil
	}
	if err != nil {
		return 0, false, unavailable("inspect storage directory", err)
	}
	if dirInfo.Mode()&os.ModeSymlink != 0 || !dirInfo.IsDir() {
		return 0, false, unavailable("storage path is not a directory", errors.New("unsafe storage directory"))
	}

	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, true, nil
	}
	if err != nil {
		return 0, false, unavailable("inspect database", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return 0, false, unavailable("database is not a regular file", errors.New("unsafe storage file"))
	}

	walSize, err = inspectRegularSidecar(path + "-wal")
	if err != nil {
		return 0, false, err
	}
	if _, err := inspectRegularSidecar(path + "-shm"); err != nil {
		return 0, false, err
	}
	if info.Size() == 0 && walSize == 0 {
		return 0, true, nil
	}
	return walSize, false, nil
}

func inspectRegularSidecar(path string) (int64, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, unavailable("inspect database sidecar", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return 0, unavailable("database sidecar is not a regular file", errors.New("unsafe storage file"))
	}
	return info.Size(), nil
}

func checkCompatibilityFromTempCopy(ctx context.Context, path string) error {
	tmpDir, err := os.MkdirTemp("", "aura-sqlite-compat-*")
	if err != nil {
		return unavailable("create temporary compatibility directory", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	tmpDB := filepath.Join(tmpDir, "aura.db")
	if err := copyFileToTemp(path, tmpDB); err != nil {
		return unavailable("copy database for compatibility check", err)
	}
	if err := copyFileToTemp(path+"-wal", tmpDB+"-wal"); err != nil {
		return unavailable("copy database sidecar for compatibility check", err)
	}

	dsn := (&url.URL{Scheme: "file", Path: tmpDB}).String()
	db, err := sql.Open(sqlDriverName, dsn)
	if err != nil {
		return unavailable("open temporary compatibility database", err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	return checkDBCompatibility(ctx, db)
}

func copyFileToTemp(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

func checkDBCompatibility(ctx context.Context, db *sql.DB) error {
	var migrationTableCount int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_schema WHERE type = 'table' AND name = 'schema_migration'`,
	).Scan(&migrationTableCount); err != nil {
		return Classify(fmt.Errorf("inspect schema_migration table: %w", err))
	}
	if migrationTableCount > 0 {
		hasOwner, err := schemaMigrationHasOwnerColumn(ctx, db)
		if err != nil {
			return err
		}
		if hasOwner {
			return nil
		}
		return fault.New(ErrorCodeSchemaIncompatible, "database uses an incompatible pre-squash schema and must be recreated")
	}

	var userTableCount int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_schema WHERE type = 'table' AND name NOT LIKE 'sqlite\_%' ESCAPE '\'`,
	).Scan(&userTableCount); err != nil {
		return Classify(fmt.Errorf("inspect database tables: %w", err))
	}
	if userTableCount == 0 {
		return nil
	}
	return fault.New(ErrorCodeSchemaIncompatible, "database has tables without a compatible schema ledger and must be recreated")
}

func schemaMigrationHasOwnerColumn(ctx context.Context, db *sql.DB) (bool, error) {
	rows, err := db.QueryContext(ctx, `PRAGMA table_info(schema_migration)`)
	if err != nil {
		return false, Classify(fmt.Errorf("inspect schema_migration columns: %w", err))
	}
	defer func() { _ = rows.Close() }()

	hasOwner := false
	for rows.Next() {
		var (
			cid        int
			name       string
			colType    string
			notNull    int
			defaultVal sql.NullString
			pk         int
		)
		if err := rows.Scan(&cid, &name, &colType, &notNull, &defaultVal, &pk); err != nil {
			return false, Classify(fmt.Errorf("scan schema_migration column: %w", err))
		}
		if name == "owner" {
			hasOwner = true
		}
	}
	if err := rows.Err(); err != nil {
		return false, Classify(fmt.Errorf("iterate schema_migration columns: %w", err))
	}
	return hasOwner, nil
}
