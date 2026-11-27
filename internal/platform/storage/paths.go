package storage

import (
	"fmt"
	"os"
	"path/filepath"
)

const (
	LiveDatabaseFilename = "aura.db"
	DefaultBackupDirName = "backups"
)

func DefaultDataRoot() (string, error) {
	if base := os.Getenv("XDG_DATA_HOME"); base != "" {
		return filepath.Join(base, "aura"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot resolve data directory: set XDG_DATA_HOME or HOME: %w", err)
	}
	return filepath.Join(home, ".local", "share", "aura"), nil
}

func ResolveDataRoot(cfg *Config) (string, error) {
	if cfg != nil && cfg.Path != "" {
		return cfg.Path, nil
	}
	return DefaultDataRoot()
}

func ResolvePaths(cfg *Config) (dbPath, artifactRoot, backupDir string, err error) {
	dataRoot, err := ResolveDataRoot(cfg)
	if err != nil {
		return "", "", "", err
	}
	dbPath = filepath.Join(dataRoot, LiveDatabaseFilename)
	artifactRoot = dataRoot
	if cfg != nil {
		backupDir = cfg.BackupDirectory
	}
	if backupDir == "" {
		backupDir = filepath.Join(dataRoot, DefaultBackupDirName)
	}
	return dbPath, artifactRoot, backupDir, nil
}
