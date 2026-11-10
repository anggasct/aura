package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/platform/logging"
	"github.com/anggasct/aura/internal/store"
)

const (
	liveDatabaseFilename = "aura.db"
	defaultBackupDirName = "backups"
	collectGracePeriod   = 24 * time.Hour
)

func newStorageCmd(gf *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "storage",
		Short: "SQLite storage, backup, and reconciliation operations",
	}
	cmd.AddCommand(
		newStorageBackupCmd(gf),
		newStorageUpgradeCmd(gf),
		newStorageVerifyCmd(gf),
		newStorageRestoreCmd(gf),
		newStorageReconcileCmd(gf),
		newStorageGcCmd(gf),
	)
	return cmd
}

func resolveDataRoot(cfg *config.Config) (string, error) {
	if cfg.Storage.Path != "" {
		return cfg.Storage.Path, nil
	}
	return defaultDataRoot()
}

func storagePaths(cfg *config.Config) (dbPath, artifactRoot, backupDir string, err error) {
	dataRoot, err := resolveDataRoot(cfg)
	if err != nil {
		return "", "", "", err
	}
	dbPath = filepath.Join(dataRoot, liveDatabaseFilename)
	artifactRoot = dataRoot
	backupDir = cfg.Storage.BackupDirectory
	if backupDir == "" {
		backupDir = filepath.Join(dataRoot, defaultBackupDirName)
	}
	return dbPath, artifactRoot, backupDir, nil
}

func defaultDataRoot() (string, error) {
	if base := os.Getenv("XDG_DATA_HOME"); base != "" {
		return filepath.Join(base, "aura"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot resolve data directory: set XDG_DATA_HOME or HOME: %w", err)
	}
	return filepath.Join(home, ".local", "share", "aura"), nil
}

func openStorage(ctx context.Context, cfg *config.Config) (*sql.DB, error) {
	dbPath, _, _, err := storagePaths(cfg)
	if err != nil {
		return nil, err
	}
	db, err := store.OpenDBWithOptions(ctx, dbPath, store.OpenOptions{BusyTimeout: time.Duration(cfg.Storage.BusyTimeout)})
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(cfg.Storage.MaxOpenConnections)
	if err := store.Migrate(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

func newStorageBackupCmd(gf *globalFlags) *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:   "backup",
		Short: "Create and verify an online database and artifact-manifest backup",
		RunE: func(cmd *cobra.Command, args []string) error {
			return withStorage(cmd, gf, true, func(ctx context.Context, logger *slog.Logger, cfg *config.Config, opened *sql.DB) error {
				_, artifactRoot, backupDir, err := storagePaths(cfg)
				if err != nil {
					return err
				}
				destDir := output
				if destDir == "" {
					destDir = backupDir
				}
				start := time.Now()
				manifest, err := store.Backup(ctx, opened, destDir)
				if err != nil {
					return err
				}
				report, err := store.VerifyRestore(ctx, destDir, artifactRoot)
				if err != nil {
					return err
				}
				logger.InfoContext(ctx, "storage backup complete",
					"component", "storage",
					"operation", "backup",
					"duration_ms", time.Since(start).Milliseconds(),
					"blobs", len(manifest.Blobs),
					"verified_blobs", report.VerifiedBlobs,
					"sessions", report.Sessions,
					"events", report.Events,
				)
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&output, "output", "", "backup destination directory (default: storage.backup_directory)")
	return cmd
}

func newStorageUpgradeCmd(gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "upgrade",
		Short: "Back up and verify the database, then apply pending migrations",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withStorage(cmd, gf, false, func(ctx context.Context, logger *slog.Logger, cfg *config.Config, _ *sql.DB) error {
				return runStorageUpgrade(ctx, logger, cfg, time.Now())
			})
		},
	}
}

func runStorageUpgrade(ctx context.Context, logger *slog.Logger, cfg *config.Config, now time.Time) error {
	dbPath, artifactRoot, backupDir, err := storagePaths(cfg)
	if err != nil {
		return err
	}
	if _, err := os.Stat(dbPath); errors.Is(err, os.ErrNotExist) {
		db, err := openStorage(ctx, cfg)
		if err != nil {
			return err
		}
		logger.InfoContext(ctx, "storage initialized", "component", "storage", "operation", "upgrade")
		return db.Close()
	} else if err != nil {
		return fmt.Errorf("stat live database: %w", err)
	}
	db, err := store.OpenDBWithOptions(ctx, dbPath, store.OpenOptions{BusyTimeout: time.Duration(cfg.Storage.BusyTimeout)})
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	start := time.Now()
	result, err := store.UpgradeWithBackup(ctx, db, backupDir, artifactRoot, now)
	if err != nil {
		if result.Backup != "" {
			return fmt.Errorf("storage upgrade from schema %d to %d failed; restore the pre-upgrade backup %s with aura storage restore: %w", result.Applied, result.Latest, result.Backup, err)
		}
		return err
	}
	logger.InfoContext(ctx, "storage upgrade complete",
		"component", "storage",
		"operation", "upgrade",
		"duration_ms", time.Since(start).Milliseconds(),
		"from_schema", result.Applied,
		"to_schema", result.Latest,
		"backup", result.Backup,
		"sessions", result.Report.Sessions,
		"verified_blobs", result.Report.VerifiedBlobs,
	)
	return nil
}

func newStorageVerifyCmd(gf *globalFlags) *cobra.Command {
	var input string
	cmd := &cobra.Command{
		Use:   "verify",
		Short: "Verify a backup manifest, SQLite integrity, migrations, and referenced blobs",
		Args: func(cmd *cobra.Command, args []string) error {
			if input == "" {
				return &usageError{errors.New("storage verify requires --input <backup directory>")}
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return withStorage(cmd, gf, false, func(ctx context.Context, logger *slog.Logger, cfg *config.Config, opened *sql.DB) error {
				_, artifactRoot, _, err := storagePaths(cfg)
				if err != nil {
					return err
				}
				start := time.Now()
				report, err := store.VerifyRestore(ctx, input, artifactRoot)
				if err != nil {
					if _, ok := store.CodeOf(err); ok {
						return err
					}
					return store.Errorf(store.ErrorCodeBackupInvalid, "storage verification failed: %v", err)
				}
				logger.InfoContext(ctx, "storage verification complete",
					"component", "storage",
					"operation", "verify",
					"duration_ms", time.Since(start).Milliseconds(),
					"sessions", report.Sessions,
					"events", report.Events,
					"dedupe_keys", report.DedupeKeys,
					"artifact_refs", report.ArtifactRefs,
					"verified_blobs", report.VerifiedBlobs,
					"checksum_mismatches", len(report.ChecksumMismatches),
					"missing_blobs", len(report.MissingBlobFiles),
				)
				if len(report.ChecksumMismatches) > 0 || len(report.MissingBlobFiles) > 0 {
					return store.Errorf(store.ErrorCodeBackupInvalid, "storage verification failed: %d checksum mismatches, %d missing blobs", len(report.ChecksumMismatches), len(report.MissingBlobFiles))
				}
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&input, "input", "", "backup directory to verify")
	return cmd
}

func newStorageRestoreCmd(gf *globalFlags) *cobra.Command {
	var input string
	var force bool
	cmd := &cobra.Command{
		Use:   "restore",
		Short: "Offline restore of a verified backup; requires the daemon to be stopped",
		Args: func(cmd *cobra.Command, args []string) error {
			if input == "" {
				return &usageError{errors.New("storage restore requires --input <backup directory>")}
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return withStorage(cmd, gf, false, func(ctx context.Context, logger *slog.Logger, cfg *config.Config, opened *sql.DB) error {
				dbPath, artifactRoot, _, err := storagePaths(cfg)
				if err != nil {
					return err
				}
				start := time.Now()
				report, err := store.Restore(ctx, input, artifactRoot, dbPath, store.RestoreOptions{Force: force})
				if err != nil {
					return err
				}
				logger.InfoContext(ctx, "storage restore complete",
					"component", "storage",
					"operation", "restore",
					"duration_ms", time.Since(start).Milliseconds(),
					"sessions", report.Sessions,
					"events", report.Events,
					"verified_blobs", report.VerifiedBlobs,
				)
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&input, "input", "", "backup directory to restore")
	cmd.Flags().BoolVar(&force, "force", false, "overwrite an existing live database")
	return cmd
}

func newStorageReconcileCmd(gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "reconcile",
		Short: "Report missing or orphaned blobs; never deletes data",
		RunE: func(cmd *cobra.Command, args []string) error {
			return withStorage(cmd, gf, true, func(ctx context.Context, logger *slog.Logger, cfg *config.Config, opened *sql.DB) error {
				_, artifactRoot, _, err := storagePaths(cfg)
				if err != nil {
					return err
				}
				start := time.Now()
				report, err := store.Reconcile(ctx, opened, artifactRoot)
				if err != nil {
					return err
				}
				logger.InfoContext(ctx, "storage reconcile complete",
					"component", "storage",
					"operation", "reconcile",
					"duration_ms", time.Since(start).Milliseconds(),
					"missing_blobs", len(report.MissingBlobs),
					"corrupted_blobs", len(report.CorruptedBlobs),
					"orphan_files", len(report.OrphanFiles),
				)
				return nil
			})
		},
	}
}

func newStorageGcCmd(gf *globalFlags) *cobra.Command {
	var before string
	cmd := &cobra.Command{
		Use:   "gc",
		Short: "Delete only unreferenced blobs older than the grace period",
		RunE: func(cmd *cobra.Command, args []string) error {
			return withStorage(cmd, gf, true, func(ctx context.Context, logger *slog.Logger, cfg *config.Config, opened *sql.DB) error {
				_, artifactRoot, _, err := storagePaths(cfg)
				if err != nil {
					return err
				}
				cutoff, err := collectCutoff(before)
				if err != nil {
					return err
				}
				start := time.Now()
				report, err := store.Collect(ctx, opened, artifactRoot, cutoff)
				if err != nil {
					return err
				}
				logger.InfoContext(ctx, "storage gc complete",
					"component", "storage",
					"operation", "gc",
					"duration_ms", time.Since(start).Milliseconds(),
					"deleted_blobs", report.DeletedBlobs,
					"freed_bytes", report.FreedBytes,
				)
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&before, "before", "", "delete unreferenced blobs created before this RFC3339 time (default: 24h ago)")
	return cmd
}

func collectCutoff(before string) (time.Time, error) {
	if before == "" {
		return time.Now().Add(-collectGracePeriod), nil
	}
	cutoff, err := time.Parse(time.RFC3339, before)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid --before %q: %w", before, err)
	}
	return cutoff, nil
}

func withStorage(cmd *cobra.Command, gf *globalFlags, needDB bool, fn func(context.Context, *slog.Logger, *config.Config, *sql.DB) error) error {
	return withLoadedStorage(cmd, gf, needDB, func(ctx context.Context, logger *slog.Logger, result *config.LoadResult, db *sql.DB) error {
		return fn(ctx, logger, result.Config, db)
	})
}

func withLoadedStorage(cmd *cobra.Command, gf *globalFlags, needDB bool, fn func(context.Context, *slog.Logger, *config.LoadResult, *sql.DB) error) error {
	result, err := config.Load(gf.configPath)
	if err != nil {
		return err
	}
	cfg := result.Config
	level, format := resolveLogging(cmd, cfg)
	logger, err := logging.Setup(level, format, os.Stderr)
	if err != nil {
		return err
	}
	logConfigResult(cmd.Context(), logger, &result)
	var opened *sql.DB
	if needDB {
		opened, err = openStorage(cmd.Context(), cfg)
		if err != nil {
			return err
		}
		defer func() { _ = opened.Close() }()
	}
	return fn(cmd.Context(), logger, &result, opened)
}
