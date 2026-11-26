package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/platform/clikit"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/sqlite"
)

const CollectGracePeriod = 24 * time.Hour

type CommandEnv struct {
	Logger     *slog.Logger
	Config     *Config
	DB         *sql.DB
	Migrations []sqlite.Migration
}

type CommandOpener func(cmd *cobra.Command, needDB bool, fn func(ctx context.Context, env *CommandEnv) error) error

func Commands(open CommandOpener) []*cobra.Command {
	cmd := &cobra.Command{
		Use:   "storage",
		Short: "SQLite storage, backup, and reconciliation operations",
	}
	cmd.AddCommand(
		newStorageBackupCmd(open),
		newStorageUpgradeCmd(open),
		newStorageVerifyCmd(open),
		newStorageRestoreCmd(open),
		newStorageReconcileCmd(open),
		newStorageGcCmd(open),
	)
	return []*cobra.Command{cmd}
}

func runWithOpener(cmd *cobra.Command, open CommandOpener, needDB bool, fn func(ctx context.Context, env *CommandEnv) error) error {
	if open == nil {
		return fault.New(ErrorCodeInvalidArgument, "storage command opener must not be nil")
	}
	return open(cmd, needDB, func(ctx context.Context, env *CommandEnv) error {
		if env == nil || env.Config == nil {
			return fault.New(ErrorCodeInvalidArgument, "storage command requires a configuration")
		}
		if needDB && env.DB == nil {
			return fault.New(ErrorCodeInvalidArgument, "storage command requires an open database")
		}
		if env.Logger == nil {
			env.Logger = slog.New(slog.DiscardHandler)
		}
		return fn(ctx, env)
	})
}

func OpenAndMigrate(ctx context.Context, cfg *Config, migrations []sqlite.Migration) (*sql.DB, error) {
	if cfg == nil {
		return nil, errNilArgument("storage config")
	}
	dbPath, _, _, err := ResolvePaths(cfg)
	if err != nil {
		return nil, err
	}
	db, err := sqlite.Open(ctx, dbPath, sqlite.Options{BusyTimeout: time.Duration(cfg.BusyTimeout), MaxOpenConnections: cfg.MaxOpenConnections})
	if err != nil {
		return nil, err
	}
	if err := sqlite.Migrate(ctx, db, migrations); err != nil {
		return nil, errors.Join(err, db.Close())
	}
	return db, nil
}

func RunUpgrade(ctx context.Context, logger *slog.Logger, cfg *Config, migrations []sqlite.Migration, now time.Time) (retErr error) {
	if cfg == nil {
		return errNilArgument("storage config")
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	dbPath, artifactRoot, backupDir, err := ResolvePaths(cfg)
	if err != nil {
		return err
	}
	if _, err := os.Stat(dbPath); errors.Is(err, os.ErrNotExist) {
		db, err := OpenAndMigrate(ctx, cfg, migrations)
		if err != nil {
			return err
		}
		logger.InfoContext(ctx, "storage initialized", "component", "storage", "operation", "upgrade")
		return db.Close()
	} else if err != nil {
		return fmt.Errorf("stat live database: %w", err)
	}
	db, err := sqlite.Open(ctx, dbPath, sqlite.Options{BusyTimeout: time.Duration(cfg.BusyTimeout)})
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, db.Close()) }()
	start := time.Now()
	result, err := UpgradeWithBackup(ctx, db, migrations, backupDir, artifactRoot, now)
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

func newStorageBackupCmd(open CommandOpener) *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:   "backup",
		Short: "Create and verify an online database and artifact-manifest backup",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runWithOpener(cmd, open, true, func(ctx context.Context, env *CommandEnv) error {
				_, artifactRoot, backupDir, err := ResolvePaths(env.Config)
				if err != nil {
					return err
				}
				destDir := output
				if destDir == "" {
					destDir = backupDir
				}
				start := time.Now()
				manifest, err := Backup(ctx, env.DB, destDir)
				if err != nil {
					return err
				}
				report, err := VerifyRestore(ctx, destDir, artifactRoot)
				if err != nil {
					return err
				}
				env.Logger.InfoContext(ctx, "storage backup complete",
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

func newStorageUpgradeCmd(open CommandOpener) *cobra.Command {
	return &cobra.Command{
		Use:   "upgrade",
		Short: "Back up and verify the database, then apply pending migrations",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runWithOpener(cmd, open, false, func(ctx context.Context, env *CommandEnv) error {
				return RunUpgrade(ctx, env.Logger, env.Config, env.Migrations, time.Now())
			})
		},
	}
}

func newStorageVerifyCmd(open CommandOpener) *cobra.Command {
	var input string
	cmd := &cobra.Command{
		Use:   "verify",
		Short: "Verify a backup manifest, SQLite integrity, migrations, and referenced blobs",
		Args: func(_ *cobra.Command, _ []string) error {
			if input == "" {
				return clikit.NewUsageError(errors.New("storage verify requires --input <backup directory>"))
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runWithOpener(cmd, open, false, func(ctx context.Context, env *CommandEnv) error {
				_, artifactRoot, _, err := ResolvePaths(env.Config)
				if err != nil {
					return err
				}
				start := time.Now()
				report, err := VerifyRestore(ctx, input, artifactRoot)
				if err != nil {
					if _, ok := fault.CodeOf[ErrorCode](err); ok {
						return err
					}
					return fault.Wrap(ErrorCodeBackupInvalid, "storage verification failed", err)
				}
				env.Logger.InfoContext(ctx, "storage verification complete",
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
					return Errorf(ErrorCodeBackupInvalid, "storage verification failed: %d checksum mismatches, %d missing blobs", len(report.ChecksumMismatches), len(report.MissingBlobFiles))
				}
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&input, "input", "", "backup directory to verify")
	return cmd
}

func newStorageRestoreCmd(open CommandOpener) *cobra.Command {
	var input string
	var force bool
	cmd := &cobra.Command{
		Use:   "restore",
		Short: "Offline restore of a verified backup; requires the daemon to be stopped",
		Args: func(_ *cobra.Command, _ []string) error {
			if input == "" {
				return clikit.NewUsageError(errors.New("storage restore requires --input <backup directory>"))
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runWithOpener(cmd, open, false, func(ctx context.Context, env *CommandEnv) error {
				dbPath, artifactRoot, _, err := ResolvePaths(env.Config)
				if err != nil {
					return err
				}
				start := time.Now()
				report, err := Restore(ctx, input, artifactRoot, dbPath, RestoreOptions{Force: force})
				if err != nil {
					return err
				}
				env.Logger.InfoContext(ctx, "storage restore complete",
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

func newStorageReconcileCmd(open CommandOpener) *cobra.Command {
	return &cobra.Command{
		Use:   "reconcile",
		Short: "Report missing or orphaned blobs; never deletes data",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runWithOpener(cmd, open, true, func(ctx context.Context, env *CommandEnv) error {
				_, artifactRoot, _, err := ResolvePaths(env.Config)
				if err != nil {
					return err
				}
				start := time.Now()
				report, err := Reconcile(ctx, env.DB, artifactRoot)
				if err != nil {
					return err
				}
				env.Logger.InfoContext(ctx, "storage reconcile complete",
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

func newStorageGcCmd(open CommandOpener) *cobra.Command {
	var before string
	cmd := &cobra.Command{
		Use:   "gc",
		Short: "Delete only unreferenced blobs older than the grace period",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runWithOpener(cmd, open, true, func(ctx context.Context, env *CommandEnv) error {
				_, artifactRoot, _, err := ResolvePaths(env.Config)
				if err != nil {
					return err
				}
				cutoff, err := collectCutoff(before)
				if err != nil {
					return err
				}
				start := time.Now()
				report, err := Collect(ctx, env.DB, artifactRoot, cutoff)
				if err != nil {
					return err
				}
				env.Logger.InfoContext(ctx, "storage gc complete",
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
		return time.Now().Add(-CollectGracePeriod), nil
	}
	cutoff, err := time.Parse(time.RFC3339, before)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid --before %q: %w", before, err)
	}
	return cutoff, nil
}
