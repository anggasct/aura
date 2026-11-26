package storage

import "testing"

func TestStorageConfigValidate(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{name: "default", mutate: func(*Config) {}},
		{name: "connections min accepted", mutate: func(c *Config) { c.MaxOpenConnections = 1 }},
		{name: "connections max accepted", mutate: func(c *Config) { c.MaxOpenConnections = 16 }},
		{name: "backup retention min accepted", mutate: func(c *Config) { c.BackupRetention = 1 }},
		{
			name:   "connections too low",
			mutate: func(c *Config) { c.MaxOpenConnections = 0 },
			want:   "max_open_connections: config_invalid: 0 is out of range (1-16)",
		},
		{
			name:   "connections too high",
			mutate: func(c *Config) { c.MaxOpenConnections = 17 },
			want:   "max_open_connections: config_invalid: 17 is out of range (1-16)",
		},
		{
			name:   "busy timeout zero",
			mutate: func(c *Config) { c.BusyTimeout = 0 },
			want:   "busy_timeout: config_invalid: must be positive",
		},
		{
			name:   "artifact quota zero",
			mutate: func(c *Config) { c.ArtifactQuota = 0 },
			want:   "artifact_quota: config_invalid: must be positive",
		},
		{
			name:   "backup interval zero",
			mutate: func(c *Config) { c.BackupInterval = 0 },
			want:   "backup_interval: config_invalid: must be positive",
		},
		{
			name:   "backup retention zero",
			mutate: func(c *Config) { c.BackupRetention = 0 },
			want:   "backup_retention: config_invalid: must be at least 1",
		},
		{
			name: "two broken fields report first failure only",
			mutate: func(c *Config) {
				c.MaxOpenConnections = 0
				c.BusyTimeout = 0
			},
			want: "max_open_connections: config_invalid: 0 is out of range (1-16)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			tc.mutate(&cfg)
			err := cfg.Validate()
			if tc.want == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || err.Error() != tc.want {
				t.Fatalf("Validate() = %v, want %q", err, tc.want)
			}
		})
	}
}
