package health

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const StorageMinFreeBytes = 64 << 20

func DiskTargets(dbPath, artifactRoot, backupDir string) []FilesystemTarget {
	return []FilesystemTarget{
		{Name: DiskTargetDatabase, Path: existingAncestor(dbPath)},
		{Name: DiskTargetArtifacts, Path: existingAncestor(artifactRoot)},
		{Name: DiskTargetBackups, Path: existingAncestor(backupDir)},
		{Name: DiskTargetTemp, Path: os.TempDir()},
	}
}

func existingAncestor(path string) string {
	current := path
	for range 16 {
		if _, err := os.Stat(current); err == nil {
			return current
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	return path
}

func LastBackupTime(backupDir string) (time.Time, error) {
	entries, err := os.ReadDir(backupDir)
	if err != nil {
		return time.Time{}, err
	}
	newest := time.Time{}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if info.ModTime().After(newest) {
			newest = info.ModTime()
		}
	}
	if newest.IsZero() {
		return time.Time{}, errors.New("no backup directories found")
	}
	return newest, nil
}

func StorageIntakeProbe(dbPath string, writable func(string) error, isReadOnly func(error) bool, freeBytes func(string) (int64, error)) func(context.Context) (StorageIntakeState, string) {
	return func(context.Context) (StorageIntakeState, string) {
		if writable == nil || isReadOnly == nil || freeBytes == nil {
			return StorageIntakeUnknown, "storage probe dependencies are unavailable"
		}
		if _, err := os.Stat(dbPath); err != nil {
			return StorageIntakeUnreachable, "storage database is unreachable"
		}
		if err := writable(dbPath); err != nil {
			if isReadOnly(err) {
				return StorageIntakeReadOnly, "storage filesystem is read-only"
			}
			return StorageIntakeUnknown, "storage writability could not be confirmed"
		}
		free, err := freeBytes(filepath.Dir(dbPath))
		if err != nil {
			return StorageIntakeUnknown, "storage free space could not be determined"
		}
		if free < StorageMinFreeBytes {
			return StorageIntakeFull, fmt.Sprintf("storage has %d MiB free, below the %d MiB intake floor", free>>20, StorageMinFreeBytes>>20)
		}
		return StorageIntakeOK, "storage accepts durable writes"
	}
}
