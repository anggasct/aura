package config

import (
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/anggasct/aura/internal/platform/configkit"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/secret"
)

func ValidateSync(syncConfig *Sync) error {
	if syncConfig == nil {
		return nil
	}
	var problems []error
	switch {
	case strings.TrimSpace(syncConfig.Remote) == "" && syncConfig.Enabled:
		problems = append(problems, configkit.Invalid("sync.enabled", "requires sync.remote"))
	case strings.TrimSpace(syncConfig.Remote) != "":
		if err := validateSyncRemote(syncConfig.Remote); err != nil {
			problems = append(problems, err)
		}
	}
	if !syncBranchPattern.MatchString(syncConfig.Branch) {
		problems = append(problems, configkit.Invalid("sync.branch", "must match [A-Za-z0-9][A-Za-z0-9._/-]*"))
	}
	if syncConfig.Interval <= 0 || time.Duration(syncConfig.Interval) > 24*time.Hour {
		problems = append(problems, configkit.Invalid("sync.interval", "must be positive and at most 24h"))
	}
	for _, ref := range []struct {
		name  string
		value secret.Ref
	}{
		{"transport_secret_ref", syncConfig.TransportSecretRef},
		{"known_hosts_ref", syncConfig.KnownHostsRef},
	} {
		if ref.value.IsZero() {
			problems = append(problems, configkit.Invalid("sync."+ref.name, "must not be empty"))
		}
	}
	for _, binary := range []struct {
		name  string
		value string
	}{
		{"git_binary", syncConfig.GitBinary},
		{"ssh_binary", syncConfig.SSHBinary},
	} {
		switch {
		case strings.TrimSpace(binary.value) == "":
			problems = append(problems, configkit.Invalid("sync."+binary.name, "must not be empty"))
		case !filepath.IsAbs(binary.value):
			problems = append(problems, configkit.Invalid("sync."+binary.name, "must be an absolute path"))
		case filepath.Clean(binary.value) != binary.value:
			problems = append(problems, configkit.Invalid("sync."+binary.name, "must be clean"))
		}
	}
	if len(syncConfig.Include) == 0 {
		problems = append(problems, configkit.Invalid("sync.include", "must list at least one pattern"))
	}
	for _, pattern := range syncConfig.Include {
		if pattern != "skills/**" && pattern != "config-templates/**" {
			problems = append(problems, configkit.Invalid("sync.include", "entries must be one of skills/**, config-templates/**"))
		}
	}
	return errors.Join(problems...)
}

func validateSyncRemote(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return configkit.Invalid("sync.remote", "is not a valid URL")
	}
	if u.Scheme != "ssh" && u.Scheme != "https" {
		return configkit.Invalid("sync.remote", "scheme must be ssh or https")
	}
	if u.Host == "" {
		return configkit.Invalid("sync.remote", "has no host")
	}
	if u.User != nil {
		if u.Scheme != "ssh" {
			return configkit.Invalid("sync.remote", "must not embed credentials")
		}
		if _, hasPassword := u.User.Password(); hasPassword {
			return configkit.Invalid("sync.remote", "must not embed a password")
		}
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return configkit.Invalid("sync.remote", "must not carry query or fragment")
	}
	return nil
}

func ValidateTerminal(t Terminal) error {
	if t.RenderHz <= 0 {
		return configkit.Invalid("terminal.render_hz", "must be positive")
	}
	if t.MaxInputBytes <= 0 {
		return configkit.Invalid("terminal.max_input_bytes", "must be positive")
	}
	if t.InMemoryHistory < 0 {
		return configkit.Invalid("terminal.in_memory_history", "must not be negative")
	}
	if t.SecondInterruptTime <= 0 {
		return configkit.Invalid("terminal.second_interrupt_window", "must be positive")
	}
	switch t.PlainApproval {
	case "deny":
	default:
		return configkit.Invalid("terminal.plain_approval", "must be \"deny\"")
	}
	return nil
}

func ValidateDiscord(discord *Discord) error {
	if discord == nil {
		return configkit.Invalid("channels.discord", "must not be nil")
	}
	var problems []error
	names := make(map[string]bool, len(discord.Accounts))
	tokenRefs := make(map[secret.Ref]bool, len(discord.Accounts))
	for i := range discord.Accounts {
		account := &discord.Accounts[i]
		field := fmt.Sprintf("channels.discord.accounts[%d]", i)
		if err := configkit.ValidateAccountName(field+".name", account.Name); err != nil {
			problems = append(problems, err)
		} else if names[account.Name] {
			problems = append(problems, configkit.Invalid(field+".name", fmt.Sprintf("%q is configured more than once", account.Name)))
		} else {
			names[account.Name] = true
		}
		switch {
		case account.BotTokenRef.IsZero():
			problems = append(problems, configkit.Invalid(field+".bot_token_ref", "must not be empty"))
		case tokenRefs[account.BotTokenRef]:
			problems = append(problems, configkit.Invalid(field+".bot_token_ref", "is shared with another account"))
		default:
			tokenRefs[account.BotTokenRef] = true
		}
		problems = append(problems, snowflakeListProblems(field+".allowed_user_ids", account.AllowedUserIDs)...)
		problems = append(problems, snowflakeListProblems(field+".allowed_guild_ids", account.AllowedGuildIDs)...)
		problems = append(problems, snowflakeListProblems(field+".allowed_channel_ids", account.AllowedChannelIDs)...)
		if discord.Enabled && len(account.AllowedUserIDs) == 0 {
			problems = append(problems, configkit.Invalid(field, "requires at least one allowed_user_ids entry when channels.discord is enabled"))
		}
	}
	if discord.Enabled && len(discord.Accounts) == 0 {
		problems = append(problems, configkit.Invalid("channels.discord.enabled", "requires at least one channels.discord.accounts entry"))
	}
	if discord.MinEditInterval <= 0 {
		problems = append(problems, configkit.Invalid("channels.discord.min_edit_interval", "must be positive"))
	}
	if discord.MaxAttachmentBytes <= 0 {
		problems = append(problems, configkit.Invalid("channels.discord.max_attachment_bytes", "must be positive"))
	}
	if discord.ThreadAutoArchiveMinutes <= 0 {
		problems = append(problems, configkit.Invalid("channels.discord.thread_auto_archive_minutes", "must be positive"))
	}
	return errors.Join(problems...)
}

func ValidateHTTP(http *HTTP) error {
	var problems []error
	if err := configkit.ValidateListenAddress("channels.http.listen_address", http.ListenAddress); err != nil {
		problems = append(problems, err)
	}
	if http.MaxBodyBytes < 1 || http.MaxBodyBytes > HTTPMaxBodyCeiling {
		problems = append(problems, configkit.Invalid("channels.http.max_body_bytes", fmt.Sprintf("must be between 1 and %d", HTTPMaxBodyCeiling)))
	}
	if http.RequestsPerMinute < 1 || http.RequestsPerMinute > HTTPRequestsPerMinuteCeiling {
		problems = append(problems, configkit.Invalid("channels.http.requests_per_minute", fmt.Sprintf("must be between 1 and %d", HTTPRequestsPerMinuteCeiling)))
	}
	if http.MaxConcurrentPerKey < 1 || http.MaxConcurrentPerKey > HTTPMaxConcurrentPerKeyLimit {
		problems = append(problems, configkit.Invalid("channels.http.max_concurrent_per_key", fmt.Sprintf("must be between 1 and %d", HTTPMaxConcurrentPerKeyLimit)))
	}
	if http.MaxHistoryMessages < 0 || http.MaxHistoryMessages > HTTPMaxHistoryMessagesLimit {
		problems = append(problems, configkit.Invalid("channels.http.max_history_messages", fmt.Sprintf("must be between 0 and %d", HTTPMaxHistoryMessagesLimit)))
	}
	if http.MaxHistoryBytes < 0 || http.MaxHistoryBytes > HTTPMaxHistoryBytesLimit {
		problems = append(problems, configkit.Invalid("channels.http.max_history_bytes", fmt.Sprintf("must be between 0 and %d", HTTPMaxHistoryBytesLimit)))
	}
	if keepalive := time.Duration(http.KeepaliveInterval); keepalive < HTTPKeepaliveIntervalFloor || keepalive > HTTPKeepaliveIntervalCeiling {
		problems = append(problems, configkit.Invalid("channels.http.keepalive_interval", fmt.Sprintf("must be between %s and %s", HTTPKeepaliveIntervalFloor, HTTPKeepaliveIntervalCeiling)))
	}
	return errors.Join(problems...)
}

func snowflakeListProblems(field string, ids []string) []error {
	var problems []error
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		switch {
		case id == "":
			problems = append(problems, configkit.Invalid(field, "must not contain empty ids"))
		case !principal.IsDecimalSnowflake(id):
			problems = append(problems, configkit.Invalid(field, fmt.Sprintf("id %q is not a decimal snowflake", id)))
		case seen[id]:
			problems = append(problems, configkit.Invalid(field, fmt.Sprintf("id %q is configured more than once", id)))
		default:
			seen[id] = true
		}
	}
	return problems
}

func ValidateTools(toolsConfig *Tools) error {
	if toolsConfig == nil {
		return nil
	}
	if strings.TrimSpace(toolsConfig.Workspace) == "" {
		return configkit.Invalid("tools.workspace", "must not be empty")
	}
	if !filepath.IsAbs(toolsConfig.Workspace) {
		return configkit.Invalid("tools.workspace", "must be an absolute path")
	}
	if toolsConfig.MaxInlineResultBytes <= 0 {
		return configkit.Invalid("tools.max_inline_result_bytes", "must be positive")
	}
	if err := configkit.Under("tools.exec", toolsConfig.Exec.Validate()); err != nil {
		return err
	}
	if err := configkit.Under("tools.fetch", toolsConfig.Fetch.Validate()); err != nil {
		return err
	}
	if err := configkit.Under("tools.github", toolsConfig.GitHub.Validate()); err != nil {
		return err
	}
	return configkit.Under("tools.web_search", toolsConfig.WebSearch.Validate())
}

func ValidateChildren(childrenConfig *Children) error {
	if childrenConfig == nil {
		return nil
	}
	var problems []error
	if childrenConfig.MaxDepth != 1 {
		problems = append(problems, configkit.Invalid("children.max_depth", "must be 1"))
	}
	if childrenConfig.MaxActivePerParent <= 0 || childrenConfig.MaxActivePerParent > 64 {
		problems = append(problems, configkit.Invalid("children.max_active_per_parent", "must be between 1 and 64"))
	}
	if childrenConfig.MaxActiveGlobal <= 0 || childrenConfig.MaxActiveGlobal > 256 {
		problems = append(problems, configkit.Invalid("children.max_active_global", "must be between 1 and 256"))
	}
	if time.Duration(childrenConfig.DefaultTimeout) <= 0 || time.Duration(childrenConfig.DefaultTimeout) > 24*time.Hour {
		problems = append(problems, configkit.Invalid("children.default_timeout", "must be positive and at most 24h"))
	}
	if time.Duration(childrenConfig.MaxTimeout) <= 0 || time.Duration(childrenConfig.MaxTimeout) > 24*time.Hour {
		problems = append(problems, configkit.Invalid("children.max_timeout", "must be positive and at most 24h"))
	}
	if time.Duration(childrenConfig.DefaultTimeout) > time.Duration(childrenConfig.MaxTimeout) {
		problems = append(problems, configkit.Invalid("children.default_timeout", "must not exceed children.max_timeout"))
	}
	if childrenConfig.Recovery != "interrupt" {
		problems = append(problems, configkit.Invalid("children.recovery", "must be \"interrupt\""))
	}
	return errors.Join(problems...)
}
