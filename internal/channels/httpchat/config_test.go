package httpchat

import (
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/configkit"
	"github.com/anggasct/aura/internal/platform/fault"
)

func TestHTTPChatConfigValidate(t *testing.T) {
	defaults := Default()
	if err := defaults.Validate(); err != nil {
		t.Fatalf("Default().Validate() = %v", err)
	}
	var nilCfg *Config
	if err := nilCfg.Validate(); !fault.HasCode(err, configkit.ErrorCodeInvalid) {
		t.Fatalf("nil Config.Validate() = %v, want %s", err, configkit.ErrorCodeInvalid)
	}
	cases := map[string]struct {
		mutate func(*Config)
		want   string
	}{
		"listen address without port":   {mutate: func(c *Config) { c.ListenAddress = "127.0.0.1" }, want: "listen_address: config_invalid: must be host:port"},
		"listen address port zero":      {mutate: func(c *Config) { c.ListenAddress = "127.0.0.1:0" }, want: "listen_address: config_invalid: port must be between 1 and 65535"},
		"listen address port too large": {mutate: func(c *Config) { c.ListenAddress = "127.0.0.1:70000" }, want: "listen_address: config_invalid: port must be between 1 and 65535"},
		"listen address without host":   {mutate: func(c *Config) { c.ListenAddress = ":8284" }, want: "listen_address: config_invalid: must include a host"},
		"body bound zero":               {mutate: func(c *Config) { c.MaxBodyBytes = 0 }, want: "max_body_bytes: config_invalid: must be between 1 and 10485760"},
		"body bound above ceiling":      {mutate: func(c *Config) { c.MaxBodyBytes = MaxBodyCeiling + 1 }, want: "max_body_bytes: config_invalid: must be between 1 and 10485760"},
		"rate zero":                     {mutate: func(c *Config) { c.RequestsPerMinute = 0 }, want: "requests_per_minute: config_invalid: must be between 1 and 6000"},
		"rate above ceiling":            {mutate: func(c *Config) { c.RequestsPerMinute = RequestsPerMinuteCeiling + 1 }, want: "requests_per_minute: config_invalid: must be between 1 and 6000"},
		"concurrency zero":              {mutate: func(c *Config) { c.MaxConcurrentPerKey = 0 }, want: "max_concurrent_per_key: config_invalid: must be between 1 and 64"},
		"concurrency above limit":       {mutate: func(c *Config) { c.MaxConcurrentPerKey = MaxConcurrentPerKeyLimit + 1 }, want: "max_concurrent_per_key: config_invalid: must be between 1 and 64"},
		"history messages negative":     {mutate: func(c *Config) { c.MaxHistoryMessages = -1 }, want: "max_history_messages: config_invalid: must be between 0 and 100"},
		"history messages above limit":  {mutate: func(c *Config) { c.MaxHistoryMessages = MaxHistoryMessagesLimit + 1 }, want: "max_history_messages: config_invalid: must be between 0 and 100"},
		"history bytes negative":        {mutate: func(c *Config) { c.MaxHistoryBytes = -1 }, want: "max_history_bytes: config_invalid: must be between 0 and 262144"},
		"history bytes above limit":     {mutate: func(c *Config) { c.MaxHistoryBytes = MaxHistoryBytesLimit + 1 }, want: "max_history_bytes: config_invalid: must be between 0 and 262144"},
		"keepalive below floor":         {mutate: func(c *Config) { c.KeepaliveInterval = configkit.Duration(999 * time.Millisecond) }, want: "keepalive_interval: config_invalid: must be between 1s and 1m0s"},
		"keepalive above ceiling":       {mutate: func(c *Config) { c.KeepaliveInterval = configkit.Duration(61 * time.Second) }, want: "keepalive_interval: config_invalid: must be between 1s and 1m0s"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := Default()
			tc.mutate(&cfg)
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate() = %v, want %q", err, tc.want)
			}
			if !fault.HasCode(err, configkit.ErrorCodeInvalid) {
				t.Errorf("Validate() code = %v, want %s", err, configkit.ErrorCodeInvalid)
			}
		})
	}
	bounds := Default()
	bounds.MaxBodyBytes = 1
	bounds.RequestsPerMinute = 1
	bounds.MaxConcurrentPerKey = 1
	bounds.MaxHistoryMessages = 0
	bounds.MaxHistoryBytes = 0
	bounds.KeepaliveInterval = configkit.Duration(KeepaliveIntervalFloor)
	if err := bounds.Validate(); err != nil {
		t.Errorf("lower bounds Validate() = %v", err)
	}
	bounds.MaxBodyBytes = MaxBodyCeiling
	bounds.RequestsPerMinute = RequestsPerMinuteCeiling
	bounds.MaxConcurrentPerKey = MaxConcurrentPerKeyLimit
	bounds.MaxHistoryMessages = MaxHistoryMessagesLimit
	bounds.MaxHistoryBytes = MaxHistoryBytesLimit
	bounds.KeepaliveInterval = configkit.Duration(KeepaliveIntervalCeiling)
	if err := bounds.Validate(); err != nil {
		t.Errorf("upper bounds Validate() = %v", err)
	}
}
