package app

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	appconfig "github.com/anggasct/aura/internal/app/config"
	"github.com/anggasct/aura/internal/modules/search"
	"github.com/anggasct/aura/internal/platform/secret"
)

type configuredSecret struct {
	name     string
	optional bool
	resolve  func() (string, error)
}

func referenceSecret(name string, ref secret.Reference) configuredSecret {
	return configuredSecret{name: name, resolve: ref.Resolve}
}

func environmentSecret(name, variable string) configuredSecret {
	return configuredSecret{name: name, resolve: func() (string, error) {
		value, ok := os.LookupEnv(variable)
		if !ok {
			return "", errors.New("environment variable is not set")
		}
		return value, nil
	}}
}

func fileSecret(name, path string) configuredSecret {
	ref := secret.Reference{File: path}
	return configuredSecret{name: name, resolve: func() (string, error) {
		value, err := ref.Resolve()
		return strings.TrimSpace(value), err
	}}
}

func literalSecret(name, value string) configuredSecret {
	return configuredSecret{name: name, resolve: func() (string, error) { return value, nil }}
}

func configuredSecrets(cfg *appconfig.Config) []configuredSecret {
	var secrets []configuredSecret
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
			entry.optional = !cfg.Sync.Enabled || cfg.Sync.TransportSecretRef == builtinDefaults().Sync.TransportSecretRef
			secrets = append(secrets, entry)
		}
	}
	for _, key := range cfg.Webhook.Keys {
		if key.SecretEnv != "" {
			secrets = append(secrets, environmentSecret("webhook_"+key.ID, key.SecretEnv))
		}
	}
	for _, credential := range githubCredentials(cfg) {
		secrets = append(secrets, configuredSecret{name: "github_credential_" + credential.Name, optional: true, resolve: credential.Ref.Reference().Resolve})
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

func additionalConfiguredSecrets(cfg *appconfig.Config) []configuredSecret {
	var secrets []configuredSecret
	if cfg.Tools != nil {
		if ref := cfg.Tools.WebSearch.CredentialRef.Reference(); ref.Env != "" || ref.File != "" {
			entry := referenceSecret("web_search_credential", ref)
			entry.optional = cfg.Tools.WebSearch.CredentialRef == search.DefaultCredentialRef(cfg.Tools.WebSearch.Provider)
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
			entry.optional = true
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

func builtinDefaults() appconfig.Config {
	return appconfig.Default()
}

func buildStrictSecretVault(cfg *appconfig.Config) (*secret.Vault, error) {
	vault := secret.New()
	var unresolved []string
	for _, entry := range append(configuredSecrets(cfg), additionalConfiguredSecrets(cfg)...) {
		value, err := entry.resolve()
		if err != nil || strings.TrimSpace(value) == "" {
			if !entry.optional {
				unresolved = append(unresolved, entry.name)
			}
			continue
		}
		vault.Set(entry.name, value)
	}
	if len(unresolved) > 0 {
		slices.Sort(unresolved)
		return nil, fmt.Errorf("unresolved secret references: %s", strings.Join(slices.Compact(unresolved), ", "))
	}
	return vault, nil
}

func buildSecretVault(cfg *appconfig.Config) *secret.Vault {
	vault := secret.New()
	for _, entry := range configuredSecrets(cfg) {
		if value, err := entry.resolve(); err == nil && strings.TrimSpace(value) != "" {
			vault.Set(entry.name, value)
		}
	}
	return vault
}
