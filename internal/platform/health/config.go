package health

import (
	"net"
	"strconv"
	"time"

	"github.com/anggasct/aura/internal/platform/configkit"
)

type Config struct {
	Listen                    string             `json:"listen"`
	CheckInterval             configkit.Duration `json:"check_interval"`
	CheckTimeout              configkit.Duration `json:"check_timeout"`
	DiskWarningPercent        int                `json:"disk_warning_percent"`
	DiskCriticalPercent       int                `json:"disk_critical_percent"`
	DiskCriticalFloorBytes    configkit.ByteSize `json:"disk_critical_floor_bytes"`
	BackupMaxAge              configkit.Duration `json:"backup_max_age"`
	RestoreVerificationMaxAge configkit.Duration `json:"restore_verification_max_age"`
}

func Default() Config {
	return Config{
		Listen:                    "127.0.0.1:8281",
		CheckInterval:             configkit.Duration(60 * time.Second),
		CheckTimeout:              configkit.Duration(5 * time.Second),
		DiskWarningPercent:        15,
		DiskCriticalPercent:       8,
		DiskCriticalFloorBytes:    configkit.ByteSize(512 << 20),
		BackupMaxAge:              configkit.Duration(24 * time.Hour),
		RestoreVerificationMaxAge: configkit.Duration(30 * 24 * time.Hour),
	}
}

func (c Config) Validate() error {
	if err := validateLoopbackListen(c.Listen, "listen"); err != nil {
		return err
	}
	if c.CheckInterval <= 0 {
		return configkit.Invalid("check_interval", "must be positive")
	}
	if c.CheckTimeout <= 0 {
		return configkit.Invalid("check_timeout", "must be positive")
	}
	if c.CheckTimeout > c.CheckInterval {
		return configkit.Invalid("check_timeout", "must not exceed health.check_interval")
	}
	if c.DiskWarningPercent < 1 || c.DiskWarningPercent > 99 {
		return configkit.Invalid("disk_warning_percent", "must be between 1 and 99")
	}
	if c.DiskCriticalPercent < 1 || c.DiskCriticalPercent > 99 {
		return configkit.Invalid("disk_critical_percent", "must be between 1 and 99")
	}
	if c.DiskCriticalPercent >= c.DiskWarningPercent {
		return configkit.Invalid("disk_critical_percent", "must be below health.disk_warning_percent")
	}
	if c.DiskCriticalFloorBytes <= 0 {
		return configkit.Invalid("disk_critical_floor_bytes", "must be positive")
	}
	if c.BackupMaxAge <= 0 {
		return configkit.Invalid("backup_max_age", "must be positive")
	}
	if c.RestoreVerificationMaxAge <= 0 {
		return configkit.Invalid("restore_verification_max_age", "must be positive")
	}
	return nil
}

func validateLoopbackListen(listen, field string) error {
	host, portText, err := net.SplitHostPort(listen)
	if err != nil {
		return configkit.Invalid(field, "must be host:port")
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return configkit.Invalid(field, "port must be between 1 and 65535")
	}
	if host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return configkit.Invalid(field, "must bind loopback; non-loopback probe exposure requires the authenticated admin surface")
}
