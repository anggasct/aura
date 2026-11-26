package storage

import (
	"fmt"
	"time"

	"github.com/anggasct/aura/internal/platform/configkit"
)

type Config struct {
	Path               string             `json:"path"`
	BusyTimeout        configkit.Duration `json:"busy_timeout"`
	MaxOpenConnections int                `json:"max_open_connections"`
	ArtifactQuota      configkit.ByteSize `json:"artifact_quota"`
	BackupDirectory    string             `json:"backup_directory"`
	BackupInterval     configkit.Duration `json:"backup_interval"`
	BackupRetention    int                `json:"backup_retention"`
}

func Default() Config {
	return Config{
		BusyTimeout:        configkit.Duration(5 * time.Second),
		MaxOpenConnections: 4,
		ArtifactQuota:      configkit.ByteSize(5 << 30),
		BackupInterval:     configkit.Duration(24 * time.Hour),
		BackupRetention:    7,
	}
}

func (c Config) Validate() error {
	if c.MaxOpenConnections < 1 || c.MaxOpenConnections > 16 {
		return configkit.Invalid("max_open_connections", fmt.Sprintf("%d is out of range (1-16)", c.MaxOpenConnections))
	}
	if c.BusyTimeout <= 0 {
		return configkit.Invalid("busy_timeout", "must be positive")
	}
	if c.ArtifactQuota <= 0 {
		return configkit.Invalid("artifact_quota", "must be positive")
	}
	if c.BackupInterval <= 0 {
		return configkit.Invalid("backup_interval", "must be positive")
	}
	if c.BackupRetention < 1 {
		return configkit.Invalid("backup_retention", "must be at least 1")
	}
	return nil
}
