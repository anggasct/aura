package httpchat

import (
	"errors"
	"fmt"
	"time"

	"github.com/anggasct/aura/internal/platform/configkit"
)

const (
	DefaultListenAddress       = "127.0.0.1:8284"
	DefaultMaxBodyBytes        = 1048576
	MaxBodyCeiling             = 10485760
	DefaultRequestsPerMinute   = 60
	RequestsPerMinuteCeiling   = 6000
	DefaultMaxConcurrentPerKey = 4
	MaxConcurrentPerKeyLimit   = 64
	DefaultMaxHistoryMessages  = 20
	MaxHistoryMessagesLimit    = 100
	DefaultMaxHistoryBytes     = 32768
	MaxHistoryBytesLimit       = 262144
	DefaultKeepaliveInterval   = 15 * time.Second
	KeepaliveIntervalFloor     = time.Second
	KeepaliveIntervalCeiling   = time.Minute
)

type Config struct {
	Enabled             bool               `json:"enabled"`
	ListenAddress       string             `json:"listen_address"`
	MaxBodyBytes        int64              `json:"max_body_bytes"`
	RequestsPerMinute   int                `json:"requests_per_minute"`
	MaxConcurrentPerKey int                `json:"max_concurrent_per_key"`
	MaxHistoryMessages  int                `json:"max_history_messages"`
	MaxHistoryBytes     int                `json:"max_history_bytes"`
	KeepaliveInterval   configkit.Duration `json:"keepalive_interval"`
}

func Default() Config {
	return Config{
		ListenAddress:       DefaultListenAddress,
		MaxBodyBytes:        DefaultMaxBodyBytes,
		RequestsPerMinute:   DefaultRequestsPerMinute,
		MaxConcurrentPerKey: DefaultMaxConcurrentPerKey,
		MaxHistoryMessages:  DefaultMaxHistoryMessages,
		MaxHistoryBytes:     DefaultMaxHistoryBytes,
		KeepaliveInterval:   configkit.Duration(DefaultKeepaliveInterval),
	}
}

func (c *Config) Validate() error {
	if c == nil {
		return configkit.Invalid("", "must not be nil")
	}
	var problems []error
	if err := configkit.ValidateListenAddress("listen_address", c.ListenAddress); err != nil {
		problems = append(problems, err)
	}
	if c.MaxBodyBytes < 1 || c.MaxBodyBytes > MaxBodyCeiling {
		problems = append(problems, configkit.Invalid("max_body_bytes", fmt.Sprintf("must be between 1 and %d", MaxBodyCeiling)))
	}
	if c.RequestsPerMinute < 1 || c.RequestsPerMinute > RequestsPerMinuteCeiling {
		problems = append(problems, configkit.Invalid("requests_per_minute", fmt.Sprintf("must be between 1 and %d", RequestsPerMinuteCeiling)))
	}
	if c.MaxConcurrentPerKey < 1 || c.MaxConcurrentPerKey > MaxConcurrentPerKeyLimit {
		problems = append(problems, configkit.Invalid("max_concurrent_per_key", fmt.Sprintf("must be between 1 and %d", MaxConcurrentPerKeyLimit)))
	}
	if c.MaxHistoryMessages < 0 || c.MaxHistoryMessages > MaxHistoryMessagesLimit {
		problems = append(problems, configkit.Invalid("max_history_messages", fmt.Sprintf("must be between 0 and %d", MaxHistoryMessagesLimit)))
	}
	if c.MaxHistoryBytes < 0 || c.MaxHistoryBytes > MaxHistoryBytesLimit {
		problems = append(problems, configkit.Invalid("max_history_bytes", fmt.Sprintf("must be between 0 and %d", MaxHistoryBytesLimit)))
	}
	if keepalive := time.Duration(c.KeepaliveInterval); keepalive < KeepaliveIntervalFloor || keepalive > KeepaliveIntervalCeiling {
		problems = append(problems, configkit.Invalid("keepalive_interval", fmt.Sprintf("must be between %s and %s", KeepaliveIntervalFloor, KeepaliveIntervalCeiling)))
	}
	return errors.Join(problems...)
}
