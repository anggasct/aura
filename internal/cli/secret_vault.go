package cli

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/secret"
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

func configuredSecrets(cfg *config.Config) []configuredSecret {
	var secrets []configuredSecret
	for i := range cfg.Channels.Discord.Accounts {
		account := &cfg.Channels.Discord.Accounts[i]
		if ref := parseSecretRef(account.BotTokenRef); ref.Env != "" {
			secrets = append(secrets, referenceSecret("discord_bot_token_"+account.Name, ref))
		}
	}
	for i := range cfg.Channels.Email.Accounts {
		account := &cfg.Channels.Email.Accounts[i]
		if ref := parseSecretRef(account.IMAP.PasswordRef); ref.Env != "" || ref.File != "" {
			secrets = append(secrets, referenceSecret("email_imap_password_"+account.Name, ref))
		}
		if ref := parseSecretRef(account.SMTP.PasswordRef); ref.Env != "" || ref.File != "" {
			secrets = append(secrets, referenceSecret("email_smtp_password_"+account.Name, ref))
		}
	}
	for i := range cfg.Channels.Telegram.Accounts {
		account := &cfg.Channels.Telegram.Accounts[i]
		if ref := parseSecretRef(account.BotTokenRef); ref.Env != "" || ref.File != "" {
			secrets = append(secrets, referenceSecret("telegram_bot_token_"+account.Name, ref))
		}
		if ref := parseSecretRef(account.WebhookSecretRef); ref.Env != "" || ref.File != "" {
			secrets = append(secrets, referenceSecret("telegram_webhook_secret_"+account.Name, ref))
		}
	}
	if cfg.Sync != nil {
		if ref := parseSecretRef(cfg.Sync.TransportSecretRef); ref.Env != "" {
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
	for name := range cfg.Models.Definitions {
		def := cfg.Models.Definitions[name]
		if def.APIKeyEnv != "" {
			secrets = append(secrets, environmentSecret("model_"+def.Provider, def.APIKeyEnv))
		}
		if def.APIKeyFile != "" {
			secrets = append(secrets, fileSecret("model_"+def.Provider, def.APIKeyFile))
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

func additionalConfiguredSecrets(cfg *config.Config) []configuredSecret {
	var secrets []configuredSecret
	if cfg.Tools != nil {
		if ref := parseSecretRef(cfg.Tools.WebSearch.CredentialRef); ref.Env != "" || ref.File != "" {
			entry := referenceSecret("web_search_credential", ref)
			entry.optional = cfg.Tools.WebSearch.CredentialRef == config.DefaultWebSearchCredentialRef(cfg.Tools.WebSearch.Provider)
			secrets = append(secrets, entry)
		}
	}
	if ref := parseSecretRef(cfg.Telemetry.CredentialRef); ref.Env != "" || ref.File != "" {
		secrets = append(secrets, referenceSecret("telemetry_credential", ref))
	}
	if embeddingConfig := configuredEmbedding(cfg); embeddingConfig != nil {
		if ref := parseSecretRef(embeddingConfig.CredentialRef); ref.Env != "" || ref.File != "" {
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
				if ref := parseSecretRef(static.CredentialRef); ref.Env != "" || ref.File != "" {
					secrets = append(secrets, referenceSecret("mcp_credential_"+server.Name, ref))
				}
			}
			if oauth := server.Auth.OAuth; oauth != nil && oauth.ClientSecretEnv != "" {
				secrets = append(secrets, environmentSecret("mcp_client_secret_"+server.Name, oauth.ClientSecretEnv))
			}
		}
	}
	return secrets
}

func builtinDefaults() config.Config {
	return config.Default()
}

func buildStrictSecretVault(cfg *config.Config) (*secret.Vault, error) {
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
