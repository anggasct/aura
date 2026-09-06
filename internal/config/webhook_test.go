package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeWebhookConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

func TestLoad_WebhookDefaults(t *testing.T) {
	path := writeWebhookConfig(t, "version: 1\n")
	res, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	w := res.Config.Webhook
	if w.Enabled {
		t.Error("webhook must be disabled by default")
	}
	if w.Listen != "127.0.0.1:8282" {
		t.Errorf("listen = %q", w.Listen)
	}
	if w.MaxBodySize != ByteSize(1<<20) {
		t.Errorf("max_body_size = %d", w.MaxBodySize)
	}
	if w.TimestampTolerance != Duration(5*time.Minute) {
		t.Errorf("timestamp_tolerance = %v", w.TimestampTolerance)
	}
	if w.ReplayRetention != Duration(24*time.Hour) {
		t.Errorf("replay_retention = %v", w.ReplayRetention)
	}
	if w.RequestsPerMinute != 60 {
		t.Errorf("requests_per_minute = %d", w.RequestsPerMinute)
	}
	if len(w.Keys) != 0 {
		t.Errorf("keys = %+v", w.Keys)
	}
}

func TestLoad_WebhookEnabledValid(t *testing.T) {
	path := writeWebhookConfig(t, `version: 1
webhook:
  enabled: true
  listen_address: 127.0.0.1:8299
  keys:
    - id: primary
      secret_env: AURA_WEBHOOK_SECRET
      accept_until: "2075-01-01T00:00:00Z"
    - id: legacy
      secret_env: AURA_WEBHOOK_LEGACY
`)
	res, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	w := res.Config.Webhook
	if !w.Enabled || len(w.Keys) != 2 {
		t.Fatalf("webhook = %+v", w)
	}
	if w.Keys[0].ID != "primary" || w.Keys[0].AcceptUntil != "2075-01-01T00:00:00Z" {
		t.Errorf("primary key = %+v", w.Keys[0])
	}
	if w.MaxBodySize != ByteSize(1<<20) {
		t.Errorf("max_body_size default not applied: %d", w.MaxBodySize)
	}
}

func TestLoad_WebhookInvalid(t *testing.T) {
	cases := map[string]string{
		"enabled without keys": `version: 1
webhook:
  enabled: true
  listen_address: 127.0.0.1:8299
`,
		"all keys expired": `version: 1
webhook:
  enabled: true
  listen_address: 127.0.0.1:8299
  keys:
    - id: old
      secret_env: AURA_OLD
      accept_until: "2020-01-01T00:00:00Z"
`,
		"duplicate key ids": `version: 1
webhook:
  keys:
    - id: dup
      secret_env: AURA_A
    - id: dup
      secret_env: AURA_B
`,
		"bad secret env name": `version: 1
webhook:
  keys:
    - id: k
      secret_env: not a name!
`,
		"bad accept_until format": `version: 1
webhook:
  keys:
    - id: k
      secret_env: AURA_A
      accept_until: tomorrow
`,
		"bad listen address": `version: 1
webhook:
  listen_address: no-port-here
`,
		"port out of range": `version: 1
webhook:
  listen_address: 127.0.0.1:99999
`,
		"zero body size": `version: 1
webhook:
  max_body_size: 0B
`,
		"non-integer rate": `version: 1
webhook:
  requests_per_minute: many
`,
		"quoted enabled": `version: 1
webhook:
  enabled: "true"
`,
		"unknown key in key entry": `version: 1
webhook:
  keys:
    - id: k
      secret_env: AURA_A
      secret: inline-value
`,
		"zero timestamp tolerance": `version: 1
webhook:
  timestamp_tolerance: 0s
`,
		"non-string accept_until": `version: 1
webhook:
  keys:
    - id: k
      secret_env: AURA_A
      accept_until: 42
`,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			path := writeWebhookConfig(t, content)
			if _, err := Load(path); err == nil {
				t.Fatal("invalid webhook config accepted")
			}
		})
	}
}

func TestLoad_WebhookListenCollidesWithHealth(t *testing.T) {
	path := writeWebhookConfig(t, `version: 1
webhook:
  enabled: true
  listen_address: 127.0.0.1:8281
  keys:
    - id: k
      secret_env: AURA_WEBHOOK_SECRET
`)
	// The webhook listener must not share the health listen address; the
	// collision is rejected at load time instead of failing at bind time.
	if _, err := Load(path); err == nil {
		t.Fatal("colliding listen addresses accepted")
	}
}

func TestLoad_WebhookEnvOverride(t *testing.T) {
	t.Setenv("AURA_WEBHOOK_REQUESTS_PER_MINUTE", "120")
	path := writeWebhookConfig(t, `version: 1
webhook:
  listen_address: 127.0.0.1:8299
`)
	res, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if res.Config.Webhook.RequestsPerMinute != 120 {
		t.Fatalf("requests_per_minute = %d, want 120", res.Config.Webhook.RequestsPerMinute)
	}
}
