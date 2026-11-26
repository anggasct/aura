package config

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/anggasct/aura/internal/modules/github"
	"github.com/anggasct/aura/internal/modules/search"
	"github.com/anggasct/aura/internal/platform/secret"
)

type ConfiguredSecret struct {
	Name     string
	Optional bool
	Resolve  func() (string, error)
}

func referenceSecret(name string, ref secret.Reference) ConfiguredSecret {
	return ConfiguredSecret{Name: name, Resolve: ref.Resolve}
}

func environmentSecret(name, variable string) ConfiguredSecret {
	return ConfiguredSecret{Name: name, Resolve: func() (string, error) {
		value, ok := os.LookupEnv(variable)
		if !ok {
			return "", errors.New("environment variable is not set")
		}
		return value, nil
	}}
}

func fileSecret(name, path string) ConfiguredSecret {
	ref := secret.Reference{File: path}
	return ConfiguredSecret{Name: name, Resolve: func() (string, error) {
		value, err := ref.Resolve()
		return strings.TrimSpace(value), err
	}}
}

func literalSecret(name, value string) ConfiguredSecret {
	return ConfiguredSecret{Name: name, Resolve: func() (string, error) { return value, nil }}
}

func githubCredentials(cfg *Config) []github.Credential {
	if cfg == nil || cfg.Tools == nil {
		return nil
	}
	return cfg.Tools.GitHub.Credentials
}

func ConfiguredSecrets(cfg *Config) []ConfiguredSecret {
	var secrets []ConfiguredSecret
	for i := range cfg.Channels.Discord.Accounts {
		account := &cfg.Channels.Discord.Accounts[i]
		if ref := account.BotTokenRef.Reference(); ref.Env != "" {
			secrets = append(secrets, referenceSecret("discord_bot_token_"+account.Name, ref))
		}
	}
	for i := range cfg.Channels.Email.Accounts {
		account := &cfg.Channels.Email.Accounts[i]
		if ref := account.IMAP.PasswordRef.Reference(); ref.Env != "" || ref.File != "" {
			secrets = append(secrets, referenceSecret("email_imap_password_"+account.Name, ref))
		}
		if ref := account.SMTP.PasswordRef.Reference(); ref.Env != "" || ref.File != "" {
			secrets = append(secrets, referenceSecret("email_smtp_password_"+account.Name, ref))
		}
	}
	for i := range cfg.Channels.Telegram.Accounts {
		account := &cfg.Channels.Telegram.Accounts[i]
		if ref := account.BotTokenRef.Reference(); ref.Env != "" || ref.File != "" {
			secrets = append(secrets, referenceSecret("telegram_bot_token_"+account.Name, ref))
		}
		if ref := account.WebhookSecretRef.Reference(); ref.Env != "" || ref.File != "" {
			secrets = append(secrets, referenceSecret("telegram_webhook_secret_"+account.Name, ref))
		}
	}
	if cfg.Sync != nil {
		if ref := cfg.Sync.TransportSecretRef.Reference(); ref.Env != "" {
			entry := referenceSecret("sync_transport_secret", ref)
			entry.Optional = !cfg.Sync.Enabled || cfg.Sync.TransportSecretRef == Default().Sync.TransportSecretRef
			secrets = append(secrets, entry)
		}
	}
	for _, key := range cfg.Webhook.Keys {
		if key.SecretEnv != "" {
			secrets = append(secrets, environmentSecret("webhook_"+key.ID, key.SecretEnv))
		}
	}
	for _, credential := range githubCredentials(cfg) {
		secrets = append(secrets, ConfiguredSecret{Name: "github_credential_" + credential.Name, Optional: true, Resolve: credential.Ref.Reference().Resolve})
	}
	for name := range cfg.Models.Definitions {
		def := cfg.Models.Definitions[name]
		source := def.APIKeyRef.Reference()
		if source.Env != "" {
			secrets = append(secrets, environmentSecret("model_"+def.Provider, source.Env))
		}
		if source.File != "" {
			secrets = append(secrets, fileSecret("model_"+def.Provider, source.File))
		}
	}
	for name, preset := range cfg.Models.Providers {
		for hname, hval := range preset.Headers {
			if strings.TrimSpace(hval) == "" {
				continue
			}
			lowered := strings.ToLower(hname)
			if strings.Contains(lowered, "authorization") || strings.Contains(lowered, "api") ||
				strings.Contains(lowered, "token") || strings.Contains(lowered, "secret") {
				secrets = append(secrets, literalSecret("provider_header_"+name+"_"+hname, hval))
			}
		}
	}
	return secrets
}

func AdditionalConfiguredSecrets(cfg *Config) []ConfiguredSecret {
	var secrets []ConfiguredSecret
	if cfg.Tools != nil {
		if ref := cfg.Tools.WebSearch.CredentialRef.Reference(); ref.Env != "" || ref.File != "" {
			entry := referenceSecret("web_search_credential", ref)
			entry.Optional = cfg.Tools.WebSearch.CredentialRef == search.DefaultCredentialRef(cfg.Tools.WebSearch.Provider)
			secrets = append(secrets, entry)
		}
	}
	if ref := cfg.Telemetry.CredentialRef.Reference(); ref.Env != "" || ref.File != "" {
		secrets = append(secrets, referenceSecret("telemetry_credential", ref))
	}
	if cfg.Knowledge != nil && cfg.Knowledge.Embedding != nil {
		embeddingConfig := cfg.Knowledge.Embedding
		if ref := embeddingConfig.CredentialRef.Reference(); ref.Env != "" || ref.File != "" {
			entry := referenceSecret("knowledge_embedding_credential", ref)
			entry.Optional = true
			secrets = append(secrets, entry)
		}
	}
	if cfg.MCP != nil {
		for i := range cfg.MCP.Servers {
			server := &cfg.MCP.Servers[i]
			if server.Auth == nil {
				continue
			}
			if static := server.Auth.Static; static != nil {
				if ref := static.CredentialRef.Reference(); ref.Env != "" || ref.File != "" {
					secrets = append(secrets, referenceSecret("mcp_credential_"+server.Name, ref))
				}
			}
			if oauth := server.Auth.OAuth; oauth != nil {
				if ref := oauth.ClientSecretRef.Reference(); ref.Env != "" || ref.File != "" {
					secrets = append(secrets, referenceSecret("mcp_client_secret_"+server.Name, ref))
				}
			}
		}
	}
	return secrets
}

func BuildStrictSecretVault(cfg *Config) (*secret.Vault, error) {
	vault := secret.New()
	var unresolved []string
	for _, entry := range append(ConfiguredSecrets(cfg), AdditionalConfiguredSecrets(cfg)...) {
		value, err := entry.Resolve()
		if err != nil || strings.TrimSpace(value) == "" {
			if !entry.Optional {
				unresolved = append(unresolved, entry.Name)
			}
			continue
		}
		vault.Set(entry.Name, value)
	}
	if len(unresolved) > 0 {
		slices.Sort(unresolved)
		return nil, fmt.Errorf("unresolved secret references: %s", strings.Join(slices.Compact(unresolved), ", "))
	}
	return vault, nil
}

func BuildSecretVault(cfg *Config) *secret.Vault {
	vault := secret.New()
	for _, entry := range ConfiguredSecrets(cfg) {
		if value, err := entry.Resolve(); err == nil && strings.TrimSpace(value) != "" {
			vault.Set(entry.Name, value)
		}
	}
	return vault
}
