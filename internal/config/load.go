package config

import (
	"errors"
	"fmt"
	"maps"
	"net"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	broadcastpkg "github.com/anggasct/aura/internal/modules/broadcast"
	"github.com/anggasct/aura/internal/platform/capability"
	"github.com/anggasct/aura/internal/platform/configkit"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/logging"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/secret"
)

func ValidateCapabilities(capabilities Capabilities) error {
	var problems []error
	seen := make(map[string]bool, len(capabilities.Enabled))
	for i, name := range capabilities.Enabled {
		if seen[name] {
			problems = append(problems, configkit.Invalid("capabilities.enabled["+strconv.Itoa(i)+"]", "is listed more than once"))
		}
		seen[name] = true
	}
	return errors.Join(problems...)
}

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

func ValidateRuntime(runtime Runtime) error {
	if runtime.MaxActiveTurns <= 0 {
		return configkit.Invalid("runtime.max_active_turns", "must be positive")
	}
	if runtime.MaxPendingTurns < runtime.MaxActiveTurns {
		return configkit.Invalid("runtime.max_pending_turns", "must be at least runtime.max_active_turns")
	}
	if runtime.TurnTimeout <= 0 {
		return configkit.Invalid("runtime.turn_timeout", "must be positive")
	}
	if runtime.ShutdownTimeout <= 0 {
		return configkit.Invalid("runtime.shutdown_timeout", "must be positive")
	}
	if time.Duration(runtime.RunRetention) < MinRetention {
		return configkit.Invalid("runtime.run_retention", "must be at least 24h")
	}
	return nil
}

func ValidateServer(server Server) error {
	if server.Port < 1 || server.Port > 65535 {
		return configkit.Invalid("server.port", fmt.Sprintf("%d is out of range (1-65535)", server.Port))
	}
	return nil
}

func ValidateWorkflows(workflows *Workflows) error {
	if workflows == nil {
		return nil
	}
	if workflows.MaxConcurrentSteps < 1 {
		return configkit.Invalid("workflows.max_concurrent_steps", "must be at least 1")
	}
	if workflows.Generated.MaxSteps < 1 || workflows.Generated.MaxTokens < 1 {
		return configkit.Invalid("workflows.generated", "max_steps and max_tokens must be positive")
	}
	return nil
}

func ValidateMCP(m *MCP) error {
	if m == nil {
		return nil
	}
	seen := make(map[string]struct{}, len(m.Servers))
	for i := range m.Servers {
		s := &m.Servers[i]
		if !mcpServerNamePattern.MatchString(s.Name) {
			return configkit.Invalid(fmt.Sprintf("mcp.servers[%d].name", i), "must match [a-z][a-z0-9_-]{0,62}")
		}
		if _, exists := seen[s.Name]; exists {
			return configkit.Invalid(fmt.Sprintf("mcp.servers[%d].name", i), fmt.Sprintf("%q is configured more than once", s.Name))
		}
		seen[s.Name] = struct{}{}
		if err := ValidateMCPServer(s, fmt.Sprintf("mcp.servers[%s]", s.Name)); err != nil {
			return err
		}
	}
	return nil
}

func ValidateMCPServer(s *MCPServer, field string) error {
	switch s.Transport {
	case "stdio":
		if s.Command == "" {
			return configkit.Invalid(field+".command", "is required for stdio transport")
		}
		if !filepath.IsAbs(s.Command) {
			return configkit.Invalid(field+".command", "must be an absolute path")
		}
		if s.URL != "" {
			return configkit.Invalid(field+".url", "is not allowed for stdio transport")
		}
		if s.Auth != nil {
			return configkit.Invalid(field+".auth", "is not allowed for stdio transport")
		}
	case "streamable_http":
		if s.URL == "" {
			return configkit.Invalid(field+".url", "is required for streamable_http transport")
		}
		if s.Command != "" || len(s.Args) > 0 {
			return configkit.Invalid(field+".command", "and args are not allowed for streamable_http transport")
		}
		if err := ValidateBaseURL(s.URL); err != nil {
			return configkit.Invalid(field+".url", "is invalid: "+err.Error())
		}
	case "legacy_sse":
		if !s.CompatibilityAck {
			return configkit.Invalid(field, "legacy_sse requires compatibility_ack: true")
		}
		if s.URL == "" {
			return configkit.Invalid(field+".url", "is required for legacy_sse transport")
		}
		if err := ValidateBaseURL(s.URL); err != nil {
			return configkit.Invalid(field+".url", "is invalid: "+err.Error())
		}
	default:
		return configkit.Invalid(field+".transport", "must be stdio, streamable_http, or legacy_sse")
	}

	if s.StartupTimeout < 0 {
		return configkit.Invalid(field+".startup_timeout", "must not be negative")
	}
	if s.RequestTimeout < 0 {
		return configkit.Invalid(field+".request_timeout", "must not be negative")
	}
	if s.ConnectTimeout < 0 {
		return configkit.Invalid(field+".connect_timeout", "must not be negative")
	}
	if s.MaxMessageSize < 0 {
		return configkit.Invalid(field+".max_message_size", "must not be negative")
	}

	if s.Restart != nil {
		if s.Restart.MaxAttempts < 0 {
			return configkit.Invalid(field+".restart.max_attempts", "must not be negative")
		}
		if s.Restart.Window < 0 {
			return configkit.Invalid(field+".restart.window", "must not be negative")
		}
	}

	if s.Auth != nil {
		if s.Auth.OAuth != nil && s.Auth.Static != nil {
			return configkit.Invalid(field+".auth", "must declare either oauth or static, not both")
		}
		if s.Auth.OAuth == nil && s.Auth.Static == nil {
			return configkit.Invalid(field+".auth", "must declare either oauth or static")
		}
		if s.Auth.OAuth != nil {
			if s.Auth.OAuth.ClientIDEnv == "" {
				return configkit.Invalid(field+".auth.oauth.client_id_env", "is required")
			}
			if s.Auth.OAuth.ClientSecretEnv == "" {
				return configkit.Invalid(field+".auth.oauth.client_secret_env", "is required")
			}
		}
		if s.Auth.Static != nil {
			if s.Auth.Static.CredentialRef == "" {
				return configkit.Invalid(field+".auth.static.credential_ref", "is required")
			}
		}
	}
	return nil
}

func ApplyMCPServerDefaults(s *MCPServer) {
	if s.StartupTimeout == 0 {
		s.StartupTimeout = configkit.Duration(10 * time.Second)
	}
	if s.RequestTimeout == 0 {
		s.RequestTimeout = configkit.Duration(30 * time.Second)
	}
	if s.MaxMessageSize == 0 {
		s.MaxMessageSize = 1024 * 1024
	}
	if s.Transport == "streamable_http" && s.ConnectTimeout == 0 {
		s.ConnectTimeout = configkit.Duration(10 * time.Second)
	}
}

func ValidateLogging(loggingConfig Logging) error {
	var problems []error
	if _, err := logging.ParseLevel(loggingConfig.Level); err != nil {
		problems = append(problems, configkit.Invalid("logging.level", "must be one of debug, info, warn, error"))
	}
	if _, err := logging.ParseFormat(loggingConfig.Format); err != nil {
		problems = append(problems, configkit.Invalid("logging.format", "must be one of text, json"))
	}
	return errors.Join(problems...)
}

func ValidateStorage(storage Storage) error {
	if storage.MaxOpenConnections < 1 || storage.MaxOpenConnections > 16 {
		return configkit.Invalid("storage.max_open_connections", fmt.Sprintf("%d is out of range (1-16)", storage.MaxOpenConnections))
	}
	if storage.BusyTimeout <= 0 {
		return configkit.Invalid("storage.busy_timeout", "must be positive")
	}
	if storage.ArtifactQuota <= 0 {
		return configkit.Invalid("storage.artifact_quota", "must be positive")
	}
	if storage.BackupInterval <= 0 {
		return configkit.Invalid("storage.backup_interval", "must be positive")
	}
	if storage.BackupRetention < 1 {
		return configkit.Invalid("storage.backup_retention", "must be at least 1")
	}
	return nil
}

func ValidateTelemetry(t Telemetry) error {
	switch t.Exporter {
	case "", "none", "stdout", "otlp_grpc", "otlp_http":
	default:
		return configkit.Invalid("telemetry.exporter", "must be one of none, stdout, otlp_grpc, otlp_http")
	}
	if t.SampleRatio < 0 || t.SampleRatio > 1 {
		return configkit.Invalid("telemetry.sample_ratio", fmt.Sprintf("%f is out of range (0-1)", t.SampleRatio))
	}
	if t.QueueSize < 0 {
		return configkit.Invalid("telemetry.queue_size", "must not be negative")
	}
	if t.ExportTimeout < 0 {
		return configkit.Invalid("telemetry.export_timeout", "must not be negative")
	}
	if (t.Exporter == "otlp_grpc" || t.Exporter == "otlp_http") && t.Endpoint == "" {
		return configkit.Invalid("telemetry.endpoint", fmt.Sprintf("is required for exporter %q", t.Exporter))
	}
	return nil
}

func ValidateUsage(u Usage) error {
	if u.Currency == "" {
		return configkit.Invalid("usage.currency", "must not be empty")
	}
	if u.DailyBudgetMicros < 0 {
		return configkit.Invalid("usage.daily_budget_micros", "must not be negative")
	}
	if u.MonthlyBudgetMicros < 0 {
		return configkit.Invalid("usage.monthly_budget_micros", "must not be negative")
	}
	if u.ReservationTTL <= 0 {
		return configkit.Invalid("usage.reservation_ttl", "must be positive")
	}
	return nil
}

func ValidateHealth(h Health) error {
	if err := validateLoopbackListen(h.Listen, "health.listen"); err != nil {
		return err
	}
	if h.CheckInterval <= 0 {
		return configkit.Invalid("health.check_interval", "must be positive")
	}
	if h.CheckTimeout <= 0 {
		return configkit.Invalid("health.check_timeout", "must be positive")
	}
	if h.CheckTimeout > h.CheckInterval {
		return configkit.Invalid("health.check_timeout", "must not exceed health.check_interval")
	}
	if h.DiskWarningPercent < 1 || h.DiskWarningPercent > 99 {
		return configkit.Invalid("health.disk_warning_percent", "must be between 1 and 99")
	}
	if h.DiskCriticalPercent < 1 || h.DiskCriticalPercent > 99 {
		return configkit.Invalid("health.disk_critical_percent", "must be between 1 and 99")
	}
	if h.DiskCriticalPercent >= h.DiskWarningPercent {
		return configkit.Invalid("health.disk_critical_percent", "must be below health.disk_warning_percent")
	}
	if h.DiskCriticalFloorBytes <= 0 {
		return configkit.Invalid("health.disk_critical_floor_bytes", "must be positive")
	}
	if h.BackupMaxAge <= 0 {
		return configkit.Invalid("health.backup_max_age", "must be positive")
	}
	if h.RestoreVerificationMaxAge <= 0 {
		return configkit.Invalid("health.restore_verification_max_age", "must be positive")
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

func listenAddressProblem(field, address string) error {
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return configkit.Invalid(field, "must be host:port")
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return configkit.Invalid(field, "port must be between 1 and 65535")
	}
	if host == "" {
		return configkit.Invalid(field, "must include a host")
	}
	return nil
}

func ValidateWebhook(w *Webhook, healthListen string) error {
	if err := listenAddressProblem("webhook.listen_address", w.Listen); err != nil {
		return err
	}
	if w.MaxBodySize <= 0 {
		return configkit.Invalid("webhook.max_body_size", "must be positive")
	}
	if w.TimestampTolerance <= 0 {
		return configkit.Invalid("webhook.timestamp_tolerance", "must be positive")
	}
	if w.ReplayRetention <= 0 {
		return configkit.Invalid("webhook.replay_retention", "must be positive")
	}
	if w.RequestsPerMinute < 1 {
		return configkit.Invalid("webhook.requests_per_minute", "must be at least 1")
	}
	seen := make(map[string]bool, len(w.Keys))
	var problems []error
	for i, key := range w.Keys {
		field := fmt.Sprintf("webhook.keys[%d]", i)
		switch {
		case key.ID == "":
			problems = append(problems, configkit.Invalid(field+".id", "must not be empty"))
			continue
		case seen[key.ID]:
			problems = append(problems, configkit.Invalid(field+".id", fmt.Sprintf("%q is configured more than once", key.ID)))
			continue
		}
		seen[key.ID] = true
		if !envNamePattern.MatchString(key.SecretEnv) {
			problems = append(problems, configkit.Invalid(field+".secret_env", fmt.Sprintf("%q is not a valid environment variable name", key.SecretEnv)))
		}
		if key.AcceptUntil != "" {
			if _, err := time.Parse(time.RFC3339, key.AcceptUntil); err != nil {
				problems = append(problems, configkit.Invalid(field+".accept_until", "must be an RFC3339 timestamp or empty"))
			}
		}
	}
	if err := errors.Join(problems...); err != nil {
		return err
	}
	if !w.Enabled {
		return nil
	}
	if len(w.Keys) == 0 {
		return configkit.Invalid("webhook.enabled", "requires at least one key")
	}
	now := time.Now().UTC()
	active := false
	for _, key := range w.Keys {
		if key.AcceptUntil == "" {
			active = true
			break
		}
		until, err := time.Parse(time.RFC3339, key.AcceptUntil)
		if err == nil && until.After(now) {
			active = true
			break
		}
	}
	if !active {
		return configkit.Invalid("webhook.enabled", "requires at least one key whose accept_until has not passed")
	}
	if w.Listen == healthListen {
		return configkit.Invalid("webhook.listen_address", "must differ from health.listen")
	}
	return nil
}

var discordAccountNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

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
		switch {
		case !discordAccountNamePattern.MatchString(account.Name):
			problems = append(problems, configkit.Invalid(field+".name", fmt.Sprintf("%q must match %s", account.Name, discordAccountNamePattern.String())))
		case names[account.Name]:
			problems = append(problems, configkit.Invalid(field+".name", fmt.Sprintf("%q is configured more than once", account.Name)))
		default:
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

const (
	telegramMaxPollTimeout = 50 * time.Second
	telegramMaxBackoffCap  = 10 * time.Minute
	telegramMinEditFloor   = time.Second
	telegramMinEditCeiling = time.Minute
)

func ValidateTelegram(telegram *Telegram, discord *Discord, owner *Owner) error {
	if telegram == nil {
		return configkit.Invalid("channels.telegram", "must not be nil")
	}
	var problems []error
	names := make(map[string]bool, len(telegram.Accounts))
	tokenRefs := make(map[secret.Ref]bool, len(telegram.Accounts)+len(discord.Accounts))
	for i := range discord.Accounts {
		tokenRefs[discord.Accounts[i].BotTokenRef] = true
	}
	for i := range telegram.Accounts {
		account := &telegram.Accounts[i]
		field := fmt.Sprintf("channels.telegram.accounts[%d]", i)
		switch {
		case !discordAccountNamePattern.MatchString(account.Name):
			problems = append(problems, configkit.Invalid(field+".name", fmt.Sprintf("%q must match %s", account.Name, discordAccountNamePattern.String())))
		case names[account.Name]:
			problems = append(problems, configkit.Invalid(field+".name", fmt.Sprintf("%q is configured more than once", account.Name)))
		default:
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
		problems = append(problems, telegramUserListProblems(field+".allowed_user_ids", account.AllowedUserIDs)...)
		problems = append(problems, telegramChatListProblems(field+".allowed_chat_ids", account.AllowedChatIDs)...)
	}
	for i := range telegram.Accounts {
		problems = append(problems, telegramModeProblems(fmt.Sprintf("channels.telegram.accounts[%d]", i), &telegram.Accounts[i], tokenRefs)...)
	}
	problems = append(problems, telegramWebhookProblems(telegram)...)
	if telegram.Enabled && len(telegram.Accounts) == 0 {
		problems = append(problems, configkit.Invalid("channels.telegram.enabled", "requires at least one channels.telegram.accounts entry"))
	}
	if telegram.Enabled && !hasTelegramOwner(owner) {
		for i := range telegram.Accounts {
			if len(telegram.Accounts[i].AllowedUserIDs) == 0 {
				problems = append(problems, configkit.Invalid("channels.telegram.enabled", "requires a telegram:<user_id> entry in owner.identities or an allowed_user_ids entry on every account"))
				break
			}
		}
	}
	timeout := time.Duration(telegram.Polling.Timeout)
	if timeout < time.Second || timeout > telegramMaxPollTimeout || timeout%time.Second != 0 {
		problems = append(problems, configkit.Invalid("channels.telegram.polling.timeout", fmt.Sprintf("must be a whole number of seconds between 1s and %s", telegramMaxPollTimeout)))
	}
	backoff := time.Duration(telegram.Polling.MaxBackoff)
	if backoff < time.Second || backoff > telegramMaxBackoffCap {
		problems = append(problems, configkit.Invalid("channels.telegram.polling.max_backoff", fmt.Sprintf("must be between 1s and %s", telegramMaxBackoffCap)))
	}
	editInterval := time.Duration(telegram.MinEditInterval)
	if editInterval < telegramMinEditFloor || editInterval > telegramMinEditCeiling {
		problems = append(problems, configkit.Invalid("channels.telegram.min_edit_interval", fmt.Sprintf("must be between %s and %s", telegramMinEditFloor, telegramMinEditCeiling)))
	}
	if telegram.MaxAttachmentBytes < 1 || telegram.MaxAttachmentBytes > TelegramMaxAttachmentBytes {
		problems = append(problems, configkit.Invalid("channels.telegram.max_attachment_bytes", fmt.Sprintf("must be between 1 and %d", TelegramMaxAttachmentBytes)))
	}
	return errors.Join(problems...)
}

func ValidateHTTP(http *HTTP) error {
	var problems []error
	if err := listenAddressProblem("channels.http.listen_address", http.ListenAddress); err != nil {
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

func telegramModeProblems(field string, account *TelegramAccount, tokenRefs map[secret.Ref]bool) []error {
	switch account.Mode {
	case TelegramModePolling:
		if !account.WebhookSecretRef.IsZero() {
			return []error{configkit.Invalid(field+".webhook_secret_ref", "is only valid when mode is "+TelegramModeWebhook)}
		}
		return nil
	case TelegramModeWebhook:
		switch {
		case account.WebhookSecretRef.IsZero():
			return []error{configkit.Invalid(field+".webhook_secret_ref", "must not be empty in webhook mode")}
		case tokenRefs[account.WebhookSecretRef]:
			return []error{configkit.Invalid(field+".webhook_secret_ref", "must differ from every bot token reference")}
		}
		return nil
	default:
		return []error{configkit.Invalid(field+".mode", fmt.Sprintf("%q must be %s or %s", account.Mode, TelegramModePolling, TelegramModeWebhook))}
	}
}

func telegramWebhookProblems(telegram *Telegram) []error {
	var problems []error
	webhook := &telegram.Webhook
	if err := listenAddressProblem("channels.telegram.webhook.listen_address", webhook.ListenAddress); err != nil {
		problems = append(problems, err)
	}
	if webhook.MaxBodyBytes < 1 || webhook.MaxBodyBytes > TelegramWebhookMaxBodyCeiling {
		problems = append(problems, configkit.Invalid("channels.telegram.webhook.max_body_bytes", fmt.Sprintf("must be between 1 and %d", TelegramWebhookMaxBodyCeiling)))
	}
	if webhook.MaxConnections < 1 || webhook.MaxConnections > TelegramWebhookConnectionLimit {
		problems = append(problems, configkit.Invalid("channels.telegram.webhook.max_connections", fmt.Sprintf("must be between 1 and %d", TelegramWebhookConnectionLimit)))
	}
	if webhook.PublicBaseURL == "" {
		if telegram.UsesWebhook() {
			problems = append(problems, configkit.Invalid(telegramWebhookPublicBaseURLField, "is required when an account uses webhook mode"))
		}
		return problems
	}
	if err := validatePublicWebhookURL(webhook.PublicBaseURL); err != nil {
		problems = append(problems, err)
	}
	return problems
}

const telegramWebhookPublicBaseURLField = "channels.telegram.webhook.public_base_url"

func validatePublicWebhookURL(raw string) error {
	if strings.ContainsAny(raw, "?#") {
		return configkit.Invalid(telegramWebhookPublicBaseURLField, "must not carry a query or fragment")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return configkit.Invalid(telegramWebhookPublicBaseURLField, "is not a valid URL")
	}
	if u.Scheme != "https" {
		return configkit.Invalid(telegramWebhookPublicBaseURLField, "must use https")
	}
	if err := ValidateBaseURL(raw); err != nil {
		return configkit.Invalid(telegramWebhookPublicBaseURLField, "is not valid: "+err.Error())
	}
	return nil
}

func telegramUserListProblems(field string, ids []string) []error {
	var problems []error
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		switch {
		case id == "":
			problems = append(problems, configkit.Invalid(field, "must not contain empty ids"))
		case !principal.IsTelegramUserID(id):
			problems = append(problems, configkit.Invalid(field, fmt.Sprintf("id %q is not a positive decimal telegram user id", id)))
		case seen[id]:
			problems = append(problems, configkit.Invalid(field, fmt.Sprintf("id %q is configured more than once", id)))
		default:
			seen[id] = true
		}
	}
	return problems
}

func telegramChatListProblems(field string, ids []string) []error {
	var problems []error
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		switch {
		case id == "":
			problems = append(problems, configkit.Invalid(field, "must not contain empty ids"))
		case !principal.IsTelegramGroupChatID(id):
			problems = append(problems, configkit.Invalid(field, fmt.Sprintf("id %q is not a negative decimal telegram group chat id", id)))
		case seen[id]:
			problems = append(problems, configkit.Invalid(field, fmt.Sprintf("id %q is configured more than once", id)))
		default:
			seen[id] = true
		}
	}
	return problems
}

func hasTelegramOwner(owner *Owner) bool {
	if owner == nil {
		return false
	}
	for _, identity := range owner.Identities {
		if channel, _, found := strings.Cut(identity, ":"); found && channel == principal.ChannelTelegram {
			return true
		}
	}
	return false
}

func snowflakeListProblems(field string, ids []string) []error {
	var problems []error
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		switch {
		case id == "":
			problems = append(problems, configkit.Invalid(field, "must not contain empty ids"))
		case !isSnowflake(id):
			problems = append(problems, configkit.Invalid(field, fmt.Sprintf("id %q is not a decimal snowflake", id)))
		case seen[id]:
			problems = append(problems, configkit.Invalid(field, fmt.Sprintf("id %q is configured more than once", id)))
		default:
			seen[id] = true
		}
	}
	return problems
}

func ValidateBindings(bindings []Binding, channels *Channels) error {
	if channels == nil {
		return configkit.Invalid("channels", "must not be nil")
	}
	accounts := map[string]map[string]bool{
		BindingChannelDiscord:  make(map[string]bool, len(channels.Discord.Accounts)),
		BindingChannelTelegram: make(map[string]bool, len(channels.Telegram.Accounts)),
	}
	for i := range channels.Discord.Accounts {
		accounts[BindingChannelDiscord][channels.Discord.Accounts[i].Name] = true
	}
	for i := range channels.Telegram.Accounts {
		accounts[BindingChannelTelegram][channels.Telegram.Accounts[i].Name] = true
	}
	var problems []error
	seen := make(map[Binding]int, len(bindings))
	for i := range bindings {
		binding := bindings[i]
		field := fmt.Sprintf("bindings[%d]", i)
		if strings.TrimSpace(binding.Persona) == "" {
			problems = append(problems, configkit.Field(field+".persona", fault.New(ErrorCodeBindingInvalid, "must not be empty")))
		}
		switch binding.Channel {
		case BindingChannelDiscord:
			problems = append(problems, discordBindingProblems(field, &binding, accounts[BindingChannelDiscord])...)
		case BindingChannelTelegram:
			problems = append(problems, telegramBindingProblems(field, &binding, accounts[BindingChannelTelegram])...)
		default:
			problems = append(problems, configkit.Field(field+".channel", fault.New(ErrorCodeBindingInvalid, fmt.Sprintf("%q is not supported; use %q or %q", binding.Channel, BindingChannelDiscord, BindingChannelTelegram))))
		}
		key := binding
		key.Persona = ""
		if first, ok := seen[key]; ok {
			problems = append(problems, configkit.Field(field, fault.New(ErrorCodeBindingInvalid, fmt.Sprintf("has the same match fields as bindings[%d]", first))))
			continue
		}
		seen[key] = i
	}
	return errors.Join(problems...)
}

func discordBindingProblems(field string, binding *Binding, accounts map[string]bool) []error {
	var problems []error
	if !accounts[binding.Account] {
		problems = append(problems, configkit.Field(field+".account", fault.New(ErrorCodeBindingInvalid, fmt.Sprintf("%q is not a configured channels.discord.accounts name", binding.Account))))
	}
	if binding.ChatID != "" {
		problems = append(problems, configkit.Field(field+".chat_id", fault.New(ErrorCodeBindingInvalid, "is only valid on telegram bindings")))
	}
	if binding.GuildID == "" && binding.ChannelID == "" && binding.ThreadID == "" && binding.UserID == "" {
		problems = append(problems, configkit.Field(field, fault.New(ErrorCodeBindingInvalid, "must set at least one of guild_id, channel_id, thread_id, user_id")))
	}
	for _, match := range []struct{ name, id string }{
		{"guild_id", binding.GuildID},
		{"channel_id", binding.ChannelID},
		{"thread_id", binding.ThreadID},
		{"user_id", binding.UserID},
	} {
		if match.id != "" && !isSnowflake(match.id) {
			problems = append(problems, configkit.Field(field+"."+match.name, fault.New(ErrorCodeBindingInvalid, fmt.Sprintf("%q is not a decimal snowflake", match.id))))
		}
	}
	return problems
}

func telegramBindingProblems(field string, binding *Binding, accounts map[string]bool) []error {
	var problems []error
	if !accounts[binding.Account] {
		problems = append(problems, configkit.Field(field+".account", fault.New(ErrorCodeBindingInvalid, fmt.Sprintf("%q is not a configured channels.telegram.accounts name", binding.Account))))
	}
	if binding.GuildID != "" || binding.ChannelID != "" {
		problems = append(problems, configkit.Field(field, fault.New(ErrorCodeBindingInvalid, "matches telegram chats by chat_id, thread_id and user_id only")))
	}
	if binding.ChatID == "" && binding.UserID == "" {
		problems = append(problems, configkit.Field(field, fault.New(ErrorCodeBindingInvalid, "must set chat_id or user_id")))
	}
	if binding.ChatID != "" && !principal.IsTelegramGroupChatID(binding.ChatID) {
		problems = append(problems, configkit.Field(field+".chat_id", fault.New(ErrorCodeBindingInvalid, fmt.Sprintf("%q is not a negative decimal telegram group chat id", binding.ChatID))))
	}
	if binding.ThreadID != "" {
		switch {
		case binding.ChatID == "":
			problems = append(problems, configkit.Field(field+".thread_id", fault.New(ErrorCodeBindingInvalid, "requires chat_id")))
		case !principal.IsTelegramTopicID(binding.ThreadID):
			problems = append(problems, configkit.Field(field+".thread_id", fault.New(ErrorCodeBindingInvalid, fmt.Sprintf("%q is not a positive decimal telegram topic id", binding.ThreadID))))
		}
	}
	if binding.UserID != "" && !principal.IsTelegramUserID(binding.UserID) {
		problems = append(problems, configkit.Field(field+".user_id", fault.New(ErrorCodeBindingInvalid, fmt.Sprintf("%q is not a positive decimal telegram user id", binding.UserID))))
	}
	return problems
}

func ValidateBroadcast(broadcast *Broadcast) error {
	if broadcast == nil {
		return configkit.Invalid("broadcast", "must not be nil")
	}
	var problems []error
	if broadcast.Timezone == "" {
		problems = append(problems, configkit.Invalid("broadcast.timezone", "must not be empty"))
	} else if _, err := time.LoadLocation(broadcast.Timezone); err != nil {
		problems = append(problems, configkit.Invalid("broadcast.timezone", "is not a valid IANA timezone"))
	}
	if broadcast.QuietHours.Start == "" || !validHourMinute(broadcast.QuietHours.Start) {
		problems = append(problems, configkit.Invalid("broadcast.quiet_hours.start", "must be HH:MM"))
	}
	if broadcast.QuietHours.End == "" || !validHourMinute(broadcast.QuietHours.End) {
		problems = append(problems, configkit.Invalid("broadcast.quiet_hours.end", "must be HH:MM"))
	}
	if broadcast.MaxDigestItems <= 0 {
		problems = append(problems, configkit.Invalid("broadcast.max_digest_items", "must be positive"))
	}
	if broadcast.MaxDigestBytes <= 0 {
		problems = append(problems, configkit.Invalid("broadcast.max_digest_bytes", "must be positive"))
	}
	if broadcast.MaxAttempts <= 0 {
		problems = append(problems, configkit.Invalid("broadcast.max_attempts", "must be positive"))
	}
	if broadcast.MaxDeliveryAge <= 0 {
		problems = append(problems, configkit.Invalid("broadcast.max_delivery_age", "must be positive"))
	}
	if broadcast.MinDispatchGap < 0 {
		problems = append(problems, configkit.Invalid("broadcast.min_dispatch_gap", "must not be negative"))
	}
	for alias, route := range broadcast.Destinations {
		if !validBroadcastAlias(alias) {
			problems = append(problems, configkit.Invalid("broadcast.destinations", fmt.Sprintf("alias %q is not valid", alias)))
		}
		if !ValidBroadcastRoute(route) {
			problems = append(problems, configkit.Invalid("broadcast.destinations["+alias+"]", "route is not a channel destination"))
		}
	}
	for alias, route := range broadcast.Fallback {
		if !validBroadcastAlias(alias) {
			problems = append(problems, configkit.Invalid("broadcast.fallback", fmt.Sprintf("alias %q is not valid", alias)))
		}
		if route == "" {
			continue
		}
		if !ValidBroadcastRoute(route) {
			problems = append(problems, configkit.Invalid("broadcast.fallback["+alias+"]", "route is not a channel destination"))
		}
	}
	return errors.Join(problems...)
}

func ValidateScheduler(scheduler *Scheduler) error {
	if scheduler == nil {
		return configkit.Invalid("scheduler", "must not be nil")
	}
	var problems []error
	if scheduler.DefaultTimezone == "" {
		problems = append(problems, configkit.Invalid("scheduler.default_timezone", "must not be empty"))
	} else if _, err := time.LoadLocation(scheduler.DefaultTimezone); err != nil {
		problems = append(problems, configkit.Invalid("scheduler.default_timezone", "is not a valid IANA timezone"))
	}
	if scheduler.DefaultCatchUpGrace < 0 {
		problems = append(problems, configkit.Invalid("scheduler.default_catch_up_grace", "must not be negative"))
	}
	if scheduler.OccurrenceRetention <= 0 {
		problems = append(problems, configkit.Invalid("scheduler.occurrence_retention", "must be positive"))
	}
	return errors.Join(problems...)
}

func ValidateMemory(memory *Memory) error {
	if memory == nil {
		return configkit.Invalid("memory", "must not be nil")
	}
	var problems []error
	if memory.MaxDocuments <= 0 || memory.MaxDocuments > 50 {
		problems = append(problems, configkit.Invalid("memory.max_documents", "must be between 1 and 50"))
	}
	if memory.RecallTokenBudget <= 0 || memory.RecallTokenBudget > 8000 {
		problems = append(problems, configkit.Invalid("memory.recall_token_budget", "must be between 1 and 8000"))
	}
	if strings.TrimSpace(memory.SummaryPromptVersion) == "" {
		problems = append(problems, configkit.Invalid("memory.summary_prompt_version", "must not be empty"))
	}
	if memory.SummaryTTL <= 0 {
		problems = append(problems, configkit.Invalid("memory.summary_ttl", "must be positive"))
	}
	if memory.EnglishStemming {
		problems = append(problems, configkit.Invalid("memory.english_stemming", "is not supported by this build"))
	}
	if strings.TrimSpace(memory.Locale) != "" {
		problems = append(problems, configkit.Invalid("memory.locale", "is not supported by this build"))
	}
	return errors.Join(problems...)
}

func ValidateSkills(skillsConfig *Skills, profile capability.Profile) error {
	if skillsConfig == nil {
		if profile != capability.ProfileCore {
			return configkit.Invalid("skills", fmt.Sprintf("section is required for build profile %q", profile))
		}
		return nil
	}
	var problems []error
	if len(skillsConfig.Roots) == 0 {
		problems = append(problems, configkit.Invalid("skills.roots", "must list at least one directory"))
	}
	for _, root := range skillsConfig.Roots {
		switch {
		case strings.TrimSpace(root) == "":
			problems = append(problems, configkit.Invalid("skills.roots", "entries must not be empty"))
		case !filepath.IsAbs(root):
			problems = append(problems, configkit.Invalid("skills.roots", "entries must be absolute paths"))
		case filepath.Clean(root) != root:
			problems = append(problems, configkit.Invalid("skills.roots", "entries must be clean paths"))
		}
	}
	if skillsConfig.MaxIndexedSkills <= 0 || skillsConfig.MaxIndexedSkills > 4096 {
		problems = append(problems, configkit.Invalid("skills.max_indexed_skills", "must be between 1 and 4096"))
	}
	if skillsConfig.MaxInstructionTokens <= 0 || skillsConfig.MaxInstructionTokens > 128000 {
		problems = append(problems, configkit.Invalid("skills.max_instruction_tokens", "must be between 1 and 128000"))
	}
	if skillsConfig.MaxSessionContextTokens <= 0 {
		problems = append(problems, configkit.Invalid("skills.max_session_context_tokens", "must be positive"))
	} else if skillsConfig.MaxSessionContextTokens < skillsConfig.MaxInstructionTokens {
		problems = append(problems, configkit.Invalid("skills.max_session_context_tokens", "must be at least skills.max_instruction_tokens"))
	}
	if skillsConfig.MaxResourceBytes <= 0 || skillsConfig.MaxResourceBytes > 1<<30 {
		problems = append(problems, configkit.Invalid("skills.max_resource_bytes", "must be between 1 and 1073741824"))
	}
	if skillsConfig.QuarantineRetention <= 0 {
		problems = append(problems, configkit.Invalid("skills.quarantine_retention", "must be positive"))
	}
	return errors.Join(problems...)
}

func ValidateContext(contextConfig *Context, profile capability.Profile) error {
	if contextConfig == nil {
		if profile != capability.ProfileCore {
			return configkit.Invalid("context", fmt.Sprintf("section is required for build profile %q", profile))
		}
		return nil
	}
	var problems []error
	if contextConfig.RecentCompleteTurns <= 0 {
		problems = append(problems, configkit.Invalid("context.recent_complete_turns", "must be positive"))
	}
	if contextConfig.SafetyMarginTokens <= 0 {
		problems = append(problems, configkit.Invalid("context.safety_margin_tokens", "must be positive"))
	}
	if contextConfig.ConservativeEstimatorRatio <= 0 || contextConfig.ConservativeEstimatorRatio >= 1 {
		problems = append(problems, configkit.Invalid("context.conservative_estimator_ratio", "must be in (0,1)"))
	}
	if contextConfig.HighWaterRatio <= 0 || contextConfig.HighWaterRatio >= 1 {
		problems = append(problems, configkit.Invalid("context.high_water_ratio", "must be in (0,1)"))
	}
	if contextConfig.ConservativeEstimatorRatio > contextConfig.HighWaterRatio {
		problems = append(problems, configkit.Invalid("context.conservative_estimator_ratio", "must not exceed context.high_water_ratio"))
	}
	if contextConfig.MaxToolExcerptBytes <= 0 {
		problems = append(problems, configkit.Invalid("context.max_tool_excerpt_bytes", "must be positive"))
	}
	if strings.TrimSpace(contextConfig.Summary.Task) == "" {
		problems = append(problems, configkit.Invalid("context.summary.task", "must not be empty"))
	}
	if contextConfig.Summary.MaxSourceTokens <= 0 {
		problems = append(problems, configkit.Invalid("context.summary.max_source_tokens", "must be positive"))
	}
	if contextConfig.Summary.MaxOutputTokens <= 0 {
		problems = append(problems, configkit.Invalid("context.summary.max_output_tokens", "must be positive"))
	}
	if strings.TrimSpace(contextConfig.Summary.PromptVersion) == "" {
		problems = append(problems, configkit.Invalid("context.summary.prompt_version", "must not be empty"))
	}
	return errors.Join(problems...)
}

func validHourMinute(value string) bool {
	if len(value) != 5 || value[2] != ':' {
		return false
	}
	hour, err := strconv.Atoi(value[:2])
	if err != nil {
		return false
	}
	minute, err := strconv.Atoi(value[3:])
	if err != nil {
		return false
	}
	return hour >= 0 && hour < 24 && minute >= 0 && minute < 60
}

func ValidateProfile(profileConfig *Profile, buildProfile capability.Profile) error {
	if profileConfig == nil {
		if buildProfile != capability.ProfileCore {
			return configkit.Invalid("profile", fmt.Sprintf("section is required for build profile %q", buildProfile))
		}
		return nil
	}
	var problems []error
	if profileConfig.MaxContextFacts <= 0 || profileConfig.MaxContextFacts > 1000 {
		problems = append(problems, configkit.Invalid("profile.max_context_facts", "must be between 1 and 1000"))
	}
	if profileConfig.MaxContextTokens <= 0 || profileConfig.MaxContextTokens > 128000 {
		problems = append(problems, configkit.Invalid("profile.max_context_tokens", "must be between 1 and 128000"))
	}
	if profileConfig.ExtractionQueueCapacity <= 0 || profileConfig.ExtractionQueueCapacity > 10000 {
		problems = append(problems, configkit.Invalid("profile.extraction_queue_capacity", "must be between 1 and 10000"))
	}
	if profileConfig.CandidateMinConfidence < 0 || profileConfig.CandidateMinConfidence > 1 {
		problems = append(problems, configkit.Invalid("profile.candidate_min_confidence", "must be in [0,1]"))
	}
	if strings.TrimSpace(profileConfig.PromptVersion) == "" {
		problems = append(problems, configkit.Invalid("profile.prompt_version", "must not be empty"))
	}
	return errors.Join(problems...)
}

var ownerFixedKeys = []string{"name", "language", "timezone"}

func ValidateOwner(owner *Owner) error {
	if owner == nil {
		return nil
	}
	var problems []error
	for _, field := range []struct {
		path  string
		value string
	}{
		{"owner.name", owner.Name},
		{"owner.language", owner.Language},
		{"owner.timezone", owner.Timezone},
	} {
		if err := validateOwnerValue(field.path, field.value); err != nil {
			problems = append(problems, err)
		}
	}
	if owner.Timezone != "" {
		if _, err := time.LoadLocation(owner.Timezone); err != nil {
			problems = append(problems, configkit.Invalid("owner.timezone", "is not a valid IANA timezone"))
		}
	}
	for _, key := range slices.Sorted(maps.Keys(owner.Preferences)) {
		switch {
		case key == "":
			problems = append(problems, configkit.Invalid("owner.preferences", "keys must not be empty"))
			continue
		case key != strings.TrimSpace(key):
			problems = append(problems, configkit.Invalid("owner.preferences", fmt.Sprintf("key %q must not carry surrounding whitespace", key)))
			continue
		case strings.Contains(key, ":"):
			problems = append(problems, configkit.Invalid("owner.preferences", fmt.Sprintf("key %q must not contain a colon", key)))
			continue
		case slices.Contains(ownerFixedKeys, key):
			problems = append(problems, configkit.Invalid("owner.preferences", fmt.Sprintf("key %q collides with owner.%s", key, key)))
			continue
		}
		if err := validateOwnerValue("owner.preferences."+key, owner.Preferences[key]); err != nil {
			problems = append(problems, err)
		}
	}
	return errors.Join(problems...)
}

func ValidateOwnerIdentities(owner *Owner) error {
	if owner == nil || len(owner.Identities) == 0 {
		return nil
	}
	if _, err := principal.NewResolver(owner.Identities); err != nil {
		return configkit.Field("owner.identities", fault.New(ErrorCodeOwnerIdentityInvalid, identityProblemDetail(err)))
	}
	return nil
}

// identityProblemDetail renders resolver problems without the machine code the
// enclosing config error already carries, so every failing index appears once.
func identityProblemDetail(err error) string {
	problems := []error{err}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		problems = joined.Unwrap()
	}
	lines := make([]string, 0, len(problems))
	for _, problem := range problems {
		var typed *fault.Error[principal.ErrorCode]
		if errors.As(problem, &typed) {
			lines = append(lines, strings.Replace(problem.Error(), typed.Error(), typed.Detail, 1))
			continue
		}
		lines = append(lines, problem.Error())
	}
	return strings.Join(lines, "\n")
}

func validateOwnerValue(path, value string) error {
	if value == "" {
		return nil
	}
	if strings.ContainsAny(value, "\r\n") {
		return configkit.Invalid(path, "must be a single line")
	}
	if value != strings.TrimSpace(value) {
		return configkit.Invalid(path, "must not carry surrounding whitespace")
	}
	return nil
}

func validBroadcastAlias(alias string) bool {
	return broadcastpkg.ValidAlias(alias)
}

func ValidBroadcastRoute(route string) bool {
	_, _, err := broadcastpkg.ParseRoute(route)
	return err == nil
}

func isSnowflake(id string) bool {
	if id == "" {
		return false
	}
	for i := range len(id) {
		if id[i] < '0' || id[i] > '9' {
			return false
		}
	}
	return true
}

func ValidateVision(vision *Vision, buildProfile capability.Profile) error {
	if vision == nil {
		if buildProfile != capability.ProfileCore {
			return configkit.Invalid("vision", fmt.Sprintf("section is required for build profile %q", buildProfile))
		}
		return nil
	}
	var problems []error
	if vision.MaxEncodedBytes <= 0 {
		problems = append(problems, configkit.Invalid("vision.max_encoded_bytes", "must be positive"))
	}
	if vision.MaxPixels <= 0 {
		problems = append(problems, configkit.Invalid("vision.max_pixels", "must be positive"))
	}
	if vision.MaxDimension <= 0 {
		problems = append(problems, configkit.Invalid("vision.max_dimension", "must be positive"))
	}
	if vision.MaxImages <= 0 {
		problems = append(problems, configkit.Invalid("vision.max_images", "must be positive"))
	}
	if vision.MaxFrames != 1 {
		problems = append(problems, configkit.Invalid("vision.max_frames", "must be 1"))
	}
	if time.Duration(vision.DecodeTimeout) <= 0 || time.Duration(vision.DecodeTimeout) > 30*time.Second {
		problems = append(problems, configkit.Invalid("vision.decode_timeout", "must be positive and at most 30s"))
	}
	if vision.MaxDecodeConcurrency <= 0 {
		problems = append(problems, configkit.Invalid("vision.max_decode_concurrency", "must be positive"))
	}
	if vision.MaxRequestBytes <= 0 {
		problems = append(problems, configkit.Invalid("vision.max_request_bytes", "must be positive"))
	}
	if vision.MaxTransformMemory <= 0 {
		problems = append(problems, configkit.Invalid("vision.max_transform_memory_bytes", "must be positive"))
	}
	if !validVisionDetail(vision.Detail) {
		problems = append(problems, configkit.Invalid("vision.detail", "must be auto, low, or high"))
	}
	if strings.TrimSpace(vision.TransformVersion) == "" {
		problems = append(problems, configkit.Invalid("vision.transform_version", "must not be empty"))
	}
	for _, name := range slices.Sorted(maps.Keys(vision.Providers)) {
		limits := vision.Providers[name]
		if !validProtocolName(name) {
			problems = append(problems, configkit.Invalid("vision.providers."+name, "is not a known provider protocol"))
			continue
		}
		if limits.MaxImages < 0 || limits.MaxImages > vision.MaxImages {
			problems = append(problems, configkit.Invalid("vision.providers."+name+".max_images", "must be between 0 and vision.max_images"))
		}
		if limits.MaxRequestBytes < 0 || limits.MaxRequestBytes > vision.MaxRequestBytes {
			problems = append(problems, configkit.Invalid("vision.providers."+name+".max_request_bytes", "must be between 0 and vision.max_request_bytes"))
		}
		if limits.Detail != "" && !validVisionDetail(limits.Detail) {
			problems = append(problems, configkit.Invalid("vision.providers."+name+".detail", "must be auto, low, or high"))
		}
	}
	return errors.Join(problems...)
}

func validVisionDetail(detail string) bool {
	return detail == "auto" || detail == "low" || detail == "high"
}

func validProtocolName(name string) bool {
	return name == ProtocolOpenAIResponses || name == ProtocolOpenAIChatCompat || name == ProtocolAnthropicMessages || name == ProtocolGeminiNative
}

func ValidateTools(toolsConfig *Tools, profile capability.Profile) error {
	if toolsConfig == nil {
		if profile != capability.ProfileCore {
			return configkit.Invalid("tools", fmt.Sprintf("section is required for build profile %q", profile))
		}
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
	if toolsConfig.Exec.Timeout <= 0 {
		return configkit.Invalid("tools.exec.timeout", "must be positive")
	}
	if toolsConfig.Exec.MaxStdoutBytes <= 0 || toolsConfig.Exec.MaxStderrBytes <= 0 {
		return configkit.Invalid("tools.exec", "output limits must be positive")
	}
	if toolsConfig.Fetch.Timeout <= 0 {
		return configkit.Invalid("tools.fetch.timeout", "must be positive")
	}
	if toolsConfig.Fetch.MaxRedirects < 0 || toolsConfig.Fetch.MaxRedirects > 10 {
		return configkit.Invalid("tools.fetch.max_redirects", "must be between 0 and 10")
	}
	if toolsConfig.Fetch.MaxEncodedBytes <= 0 || toolsConfig.Fetch.MaxDecodedBytes <= 0 {
		return configkit.Invalid("tools.fetch", "body limits must be positive")
	}
	if err := validateToolGitHub(&toolsConfig.GitHub); err != nil {
		return err
	}
	return validateWebSearch(&toolsConfig.WebSearch)
}

func ValidateChildren(childrenConfig *Children, buildProfile capability.Profile) error {
	if childrenConfig == nil {
		if buildProfile != capability.ProfileCore {
			return configkit.Invalid("children", fmt.Sprintf("section is required for build profile %q", buildProfile))
		}
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
