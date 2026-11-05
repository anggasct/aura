package config

import (
	"strings"
	"testing"
	"time"
)

func TestLoad_HTTPDefaults(t *testing.T) {
	res, err := Load(writeDiscordConfig(t, "version: 1\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := HTTP{ListenAddress: "127.0.0.1:8284", MaxBodyBytes: 1048576, RequestsPerMinute: 60, MaxConcurrentPerKey: 4, MaxHistoryMessages: 20, MaxHistoryBytes: 32768, KeepaliveInterval: Duration(15 * time.Second)}
	if got := res.Config.Channels.HTTP; got != want {
		t.Errorf("channels.http = %+v, want %+v", got, want)
	}
}

func TestLoad_HTTPValid(t *testing.T) {
	cases := map[string]struct {
		block string
		want  HTTP
	}{
		"enabled with overrides": {
			block: "    enabled: true\n    listen_address: 0.0.0.0:9000\n    max_body_bytes: 2048\n    requests_per_minute: 120\n    max_concurrent_per_key: 8\n    max_history_messages: 5\n    max_history_bytes: 4096\n    keepalive_interval: 30s\n",
			want:  HTTP{Enabled: true, ListenAddress: "0.0.0.0:9000", MaxBodyBytes: 2048, RequestsPerMinute: 120, MaxConcurrentPerKey: 8, MaxHistoryMessages: 5, MaxHistoryBytes: 4096, KeepaliveInterval: Duration(30 * time.Second)},
		},
		"lower bounds": {
			block: "    max_body_bytes: 1\n    requests_per_minute: 1\n    max_concurrent_per_key: 1\n    keepalive_interval: 1s\n",
			want:  HTTP{ListenAddress: "127.0.0.1:8284", MaxBodyBytes: 1, RequestsPerMinute: 1, MaxConcurrentPerKey: 1, MaxHistoryMessages: 20, MaxHistoryBytes: 32768, KeepaliveInterval: Duration(time.Second)},
		},
		"history disabled": {
			block: "    max_history_messages: 0\n    max_history_bytes: 0\n",
			want:  HTTP{ListenAddress: "127.0.0.1:8284", MaxBodyBytes: 1048576, RequestsPerMinute: 60, MaxConcurrentPerKey: 4, KeepaliveInterval: Duration(15 * time.Second)},
		},
		"upper bounds": {
			block: "    max_body_bytes: 10485760\n    requests_per_minute: 6000\n    max_concurrent_per_key: 64\n    max_history_messages: 100\n    max_history_bytes: 262144\n    keepalive_interval: 60s\n",
			want:  HTTP{ListenAddress: "127.0.0.1:8284", MaxBodyBytes: 10485760, RequestsPerMinute: 6000, MaxConcurrentPerKey: 64, MaxHistoryMessages: 100, MaxHistoryBytes: 262144, KeepaliveInterval: Duration(time.Minute)},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			res, err := Load(writeDiscordConfig(t, "version: 1\nchannels:\n  http:\n"+tc.block))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got := res.Config.Channels.HTTP; got != tc.want {
				t.Errorf("channels.http = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestLoad_HTTPInvalid(t *testing.T) {
	cases := map[string]struct {
		block string
		want  string
		shape bool
	}{
		"listen address without port":        {block: "    listen_address: 127.0.0.1\n", want: "channels.http.listen_address must be host:port"},
		"listen address port zero":           {block: "    listen_address: 127.0.0.1:0\n", want: "channels.http.listen_address port must be between 1 and 65535"},
		"listen address port too large":      {block: "    listen_address: 127.0.0.1:70000\n", want: "channels.http.listen_address port must be between 1 and 65535"},
		"listen address without host":        {block: "    listen_address: \":8284\"\n", want: "channels.http.listen_address must include a host"},
		"body bound zero":                    {block: "    max_body_bytes: 0\n", want: "channels.http.max_body_bytes must be between 1 and 10485760"},
		"body bound negative":                {block: "    max_body_bytes: -1\n", want: "channels.http.max_body_bytes must be between 1 and 10485760"},
		"body bound above the ceiling":       {block: "    max_body_bytes: 10485761\n", want: "channels.http.max_body_bytes must be between 1 and 10485760"},
		"rate zero":                          {block: "    requests_per_minute: 0\n", want: "channels.http.requests_per_minute must be between 1 and 6000"},
		"rate above the ceiling":             {block: "    requests_per_minute: 6001\n", want: "channels.http.requests_per_minute must be between 1 and 6000"},
		"concurrency zero":                   {block: "    max_concurrent_per_key: 0\n", want: "channels.http.max_concurrent_per_key must be between 1 and 64"},
		"concurrency above the ceiling":      {block: "    max_concurrent_per_key: 65\n", want: "channels.http.max_concurrent_per_key must be between 1 and 64"},
		"enabled not a boolean":              {block: "    enabled: \"yes\"\n", want: "channels.http.enabled must be a boolean", shape: true},
		"listen address not a string":        {block: "    listen_address: 8284\n", want: "channels.http.listen_address must be a string", shape: true},
		"body bound not an integer":          {block: "    max_body_bytes: \"1MB\"\n", want: "channels.http.max_body_bytes must be an integer", shape: true},
		"rate not an integer":                {block: "    requests_per_minute: fast\n", want: "channels.http.requests_per_minute must be an integer", shape: true},
		"concurrency not an integer":         {block: "    max_concurrent_per_key: 2.5\n", want: "channels.http.max_concurrent_per_key must be an integer", shape: true},
		"unknown key":                        {block: "    route: /x\n", want: "unknown key", shape: true},
		"keepalive below the floor":          {block: "    keepalive_interval: 999ms\n", want: "channels.http.keepalive_interval must be between 1s and 1m0s"},
		"keepalive zero":                     {block: "    keepalive_interval: 0s\n", want: "channels.http.keepalive_interval must be between 1s and 1m0s"},
		"keepalive negative":                 {block: "    keepalive_interval: -5s\n", want: "channels.http.keepalive_interval must be between 1s and 1m0s"},
		"keepalive above the ceiling":        {block: "    keepalive_interval: 61s\n", want: "channels.http.keepalive_interval must be between 1s and 1m0s"},
		"keepalive without a unit":           {block: "    keepalive_interval: 15\n", want: "keepalive_interval", shape: true},
		"history messages negative":          {block: "    max_history_messages: -1\n", want: "channels.http.max_history_messages must be between 0 and 100"},
		"history messages above the ceiling": {block: "    max_history_messages: 101\n", want: "channels.http.max_history_messages must be between 0 and 100"},
		"history bytes negative":             {block: "    max_history_bytes: -1\n", want: "channels.http.max_history_bytes must be between 0 and 262144"},
		"history bytes above the ceiling":    {block: "    max_history_bytes: 262145\n", want: "channels.http.max_history_bytes must be between 0 and 262144"},
		"history messages not an integer":    {block: "    max_history_messages: many\n", want: "channels.http.max_history_messages must be an integer", shape: true},
		"history bytes not an integer":       {block: "    max_history_bytes: \"1KB\"\n", want: "channels.http.max_history_bytes must be an integer", shape: true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Load(writeDiscordConfig(t, "version: 1\nchannels:\n  http:\n"+tc.block))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Load error = %v, want it to mention %q", err, tc.want)
			}
			if code, ok := CodeOf(err); !tc.shape && (!ok || code != ErrorCodeConfigInvalid) {
				t.Errorf("error code = %q, want %q", code, ErrorCodeConfigInvalid)
			}
		})
	}
}

func TestLoad_HTTPBlockMustBeAMapping(t *testing.T) {
	_, err := Load(writeDiscordConfig(t, "version: 1\nchannels:\n  http: yes\n"))
	if err == nil || !strings.Contains(err.Error(), "channels.http must be a mapping") {
		t.Fatalf("Load error = %v, want a mapping error", err)
	}
}

func TestLoad_HTTPEnvOverride(t *testing.T) {
	t.Setenv("AURA_CHANNELS_HTTP_REQUESTS_PER_MINUTE", "5")
	res, err := Load(writeDiscordConfig(t, "version: 1\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := res.Config.Channels.HTTP.RequestsPerMinute; got != 5 {
		t.Errorf("requests_per_minute = %d, want 5", got)
	}
}
