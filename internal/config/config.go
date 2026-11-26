package config

import (
	"fmt"
	"regexp"
	"time"

	"github.com/anggasct/aura/internal/modules/github"
	"github.com/anggasct/aura/internal/platform/configkit"
	"github.com/anggasct/aura/internal/platform/secret"
)

type Sync struct {
	Enabled            bool               `json:"enabled"`
	Remote             string             `json:"remote"`
	Branch             string             `json:"branch"`
	Interval           configkit.Duration `json:"interval"`
	TransportSecretRef secret.Ref         `json:"transport_secret_ref"`
	KnownHostsRef      secret.Ref         `json:"known_hosts_ref"`
	GitBinary          string             `json:"git_binary"`
	SSHBinary          string             `json:"ssh_binary"`
	Include            []string           `json:"include"`
}

type Children struct {
	Enabled            bool               `json:"enabled"`
	MaxDepth           int                `json:"max_depth"`
	MaxActivePerParent int                `json:"max_active_per_parent"`
	MaxActiveGlobal    int                `json:"max_active_global"`
	DefaultTimeout     configkit.Duration `json:"default_timeout"`
	MaxTimeout         configkit.Duration `json:"max_timeout"`
	Recovery           string             `json:"recovery"`
}

type Discord struct {
	Enabled                  bool               `json:"enabled"`
	Accounts                 []DiscordAccount   `json:"accounts"`
	MinEditInterval          configkit.Duration `json:"min_edit_interval"`
	MaxAttachmentBytes       int64              `json:"max_attachment_bytes"`
	ThreadAutoArchiveMinutes int                `json:"thread_auto_archive_minutes"`
}

type DiscordAccount struct {
	Name              string     `json:"name"`
	BotTokenRef       secret.Ref `json:"bot_token_ref"`
	Persona           string     `json:"persona,omitempty"`
	AllowedUserIDs    []string   `json:"allowed_user_ids"`
	AllowedGuildIDs   []string   `json:"allowed_guild_ids"`
	AllowedChannelIDs []string   `json:"allowed_channel_ids"`
	AcceptDMs         bool       `json:"accept_dms"`
	LinkPreviews      bool       `json:"link_previews"`
}

func (*DiscordAccount) Default() any {
	return DiscordAccount{AcceptDMs: true}
}

type Telegram struct {
	Enabled            bool               `json:"enabled"`
	Accounts           []TelegramAccount  `json:"accounts"`
	Polling            TelegramPolling    `json:"polling"`
	MinEditInterval    configkit.Duration `json:"min_edit_interval"`
	MaxAttachmentBytes int64              `json:"max_attachment_bytes"`
	Webhook            TelegramWebhook    `json:"webhook"`
}

type TelegramAccount struct {
	Name               string     `json:"name"`
	BotTokenRef        secret.Ref `json:"bot_token_ref"`
	Mode               string     `json:"mode"`
	WebhookSecretRef   secret.Ref `json:"webhook_secret_ref,omitzero"`
	Persona            string     `json:"persona,omitempty"`
	AllowedUserIDs     []string   `json:"allowed_user_ids"`
	AllowedChatIDs     []string   `json:"allowed_chat_ids"`
	AcceptPrivateChats bool       `json:"accept_private_chats"`
	LinkPreviews       bool       `json:"link_previews"`
}

func (*TelegramAccount) Default() any {
	return TelegramAccount{AcceptPrivateChats: true, Mode: TelegramModePolling}
}

const TelegramMaxAttachmentBytes int64 = 20971520

const (
	TelegramModePolling = "polling"
	TelegramModeWebhook = "webhook"
)

const (
	TelegramWebhookListenAddress   = "127.0.0.1:8283"
	TelegramWebhookMaxBodyBytes    = 1048576
	TelegramWebhookMaxBodyCeiling  = 10485760
	TelegramWebhookMaxConnections  = 4
	TelegramWebhookConnectionLimit = 100
)

type TelegramWebhook struct {
	ListenAddress  string `json:"listen_address"`
	PublicBaseURL  string `json:"public_base_url"`
	MaxBodyBytes   int64  `json:"max_body_bytes"`
	MaxConnections int    `json:"max_connections"`
}

func (a *TelegramAccount) WebhookMode() bool {
	return a != nil && a.Mode == TelegramModeWebhook
}

func (t *Telegram) UsesWebhook() bool {
	for i := range t.Accounts {
		if t.Accounts[i].WebhookMode() {
			return true
		}
	}
	return false
}

type TelegramPolling struct {
	Timeout    configkit.Duration `json:"timeout"`
	MaxBackoff configkit.Duration `json:"max_backoff"`
}

const (
	EmailMaxAttachmentBytes  int64 = 20971520
	EmailDefaultIMAPPort           = 993
	EmailDefaultSMTPPort           = 587
	EmailDefaultFolder             = "INBOX"
	EmailDefaultPollInterval       = time.Minute
	EmailMinPollInterval           = 10 * time.Second
	EmailMaxPollInterval           = time.Hour
)

type Email struct {
	Enabled            bool           `json:"enabled"`
	Accounts           []EmailAccount `json:"accounts"`
	MaxAttachmentBytes int64          `json:"max_attachment_bytes"`
}

type EmailAccount struct {
	Name           string    `json:"name"`
	Persona        string    `json:"persona"`
	Address        string    `json:"address"`
	IMAP           EmailIMAP `json:"imap"`
	SMTP           EmailSMTP `json:"smtp"`
	AuthservID     string    `json:"authserv_id"`
	AllowedSenders []string  `json:"allowed_senders"`
}

type EmailSMTP struct {
	Host        string     `json:"host"`
	Port        int        `json:"port"`
	Username    string     `json:"username"`
	PasswordRef secret.Ref `json:"password_ref"`
}

type EmailIMAP struct {
	Host         string             `json:"host"`
	Port         int                `json:"port"`
	Username     string             `json:"username"`
	PasswordRef  secret.Ref         `json:"password_ref"`
	Folder       string             `json:"folder"`
	PollInterval configkit.Duration `json:"poll_interval"`
}

const (
	HTTPListenAddress            = "127.0.0.1:8284"
	HTTPMaxBodyBytes             = 1048576
	HTTPMaxBodyCeiling           = 10485760
	HTTPRequestsPerMinute        = 60
	HTTPRequestsPerMinuteCeiling = 6000
	HTTPMaxConcurrentPerKey      = 4
	HTTPMaxConcurrentPerKeyLimit = 64
	HTTPMaxHistoryMessages       = 20
	HTTPMaxHistoryMessagesLimit  = 100
	HTTPMaxHistoryBytes          = 32768
	HTTPMaxHistoryBytesLimit     = 262144
	HTTPKeepaliveInterval        = 15 * time.Second
	HTTPKeepaliveIntervalFloor   = time.Second
	HTTPKeepaliveIntervalCeiling = time.Minute
)

type HTTP struct {
	Enabled             bool               `json:"enabled"`
	ListenAddress       string             `json:"listen_address"`
	MaxBodyBytes        int64              `json:"max_body_bytes"`
	RequestsPerMinute   int                `json:"requests_per_minute"`
	MaxConcurrentPerKey int                `json:"max_concurrent_per_key"`
	MaxHistoryMessages  int                `json:"max_history_messages"`
	MaxHistoryBytes     int                `json:"max_history_bytes"`
	KeepaliveInterval   configkit.Duration `json:"keepalive_interval"`
}

type Terminal struct {
	RenderHz            int                `json:"render_hz"`
	MaxInputBytes       int                `json:"max_input_bytes"`
	InMemoryHistory     int                `json:"in_memory_history"`
	SecondInterruptTime configkit.Duration `json:"second_interrupt_window"`
	PlainApproval       string             `json:"plain_approval"`
}

type Tools struct {
	Workspace            string        `json:"workspace"`
	MaxInlineResultBytes int64         `json:"max_inline_result_bytes"`
	Exec                 ToolExec      `json:"exec"`
	Fetch                ToolFetch     `json:"fetch"`
	WebSearch            ToolWebSearch `json:"web_search"`
	GitHub               github.Config `json:"github,omitzero"`
}

type ToolExec struct {
	Timeout        configkit.Duration `json:"timeout"`
	MaxStdoutBytes int64              `json:"max_stdout_bytes"`
	MaxStderrBytes int64              `json:"max_stderr_bytes"`
}

type ToolFetch struct {
	Timeout         configkit.Duration `json:"timeout"`
	MaxRedirects    int                `json:"max_redirects"`
	MaxEncodedBytes int64              `json:"max_encoded_bytes"`
	MaxDecodedBytes int64              `json:"max_decoded_bytes"`
}

type ToolWebSearch struct {
	Provider      string     `json:"provider"`
	CredentialRef secret.Ref `json:"credential_ref,omitzero"`
	Endpoint      string     `json:"endpoint,omitempty"`
	MaxResults    int        `json:"max_results"`
}

const (
	MCPTransportStdio          = "stdio"
	MCPTransportStreamableHTTP = "streamable_http"
	MCPTransportLegacySSE      = "legacy_sse"
)

type MCP struct {
	Servers []MCPServer `json:"servers"`
}

type MCPServer struct {
	Name             string             `json:"name"`
	Transport        string             `json:"transport"`
	Command          string             `json:"command,omitempty"`
	Args             []string           `json:"args,omitempty"`
	Environment      map[string]string  `json:"environment,omitempty"`
	Capabilities     []string           `json:"capabilities,omitempty"`
	StartupTimeout   configkit.Duration `json:"startup_timeout,omitempty"`
	RequestTimeout   configkit.Duration `json:"request_timeout,omitempty"`
	MaxMessageSize   configkit.ByteSize `json:"max_message_size,omitempty"`
	Restart          *MCPRestart        `json:"restart,omitempty"`
	URL              string             `json:"url,omitempty"`
	Auth             *MCPAuth           `json:"auth,omitempty"`
	ConnectTimeout   configkit.Duration `json:"connect_timeout,omitempty"`
	AllowRedirects   *bool              `json:"allow_redirects,omitempty"`
	CompatibilityAck bool               `json:"compatibility_ack,omitempty"`
}

type MCPRestart struct {
	MaxAttempts int                `json:"max_attempts"`
	Window      configkit.Duration `json:"window"`
}

type MCPAuth struct {
	OAuth  *MCPOAuth      `json:"oauth,omitempty"`
	Static *MCPStaticAuth `json:"static,omitempty"`
}

type MCPOAuth struct {
	ClientIDEnv     string `json:"client_id_env"`
	ClientSecretEnv string `json:"client_secret_env"`
	TokenStore      string `json:"token_store"`
}

type MCPStaticAuth struct {
	Header        string `json:"header,omitempty"`
	CredentialRef string `json:"credential_ref"`
}

var (
	mcpServerNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,62}$`)
	syncBranchPattern    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)
)

func DefaultTools() *Tools {
	return &Tools{
		Workspace:            "/srv/aura/workspace",
		MaxInlineResultBytes: 65536,
		Exec: ToolExec{
			Timeout:        configkit.Duration(30 * time.Second),
			MaxStdoutBytes: 65536,
			MaxStderrBytes: 65536,
		},
		Fetch: ToolFetch{
			Timeout:         configkit.Duration(30 * time.Second),
			MaxRedirects:    5,
			MaxEncodedBytes: 2097152,
			MaxDecodedBytes: 8388608,
		},
		WebSearch: ToolWebSearch{
			Provider:   WebSearchProviderDuckDuckGo,
			MaxResults: 5,
		},
	}
}

func DefaultTerminal() Terminal {
	return Terminal{
		RenderHz:            20,
		MaxInputBytes:       262144,
		InMemoryHistory:     100,
		SecondInterruptTime: configkit.Duration(2 * time.Second),
		PlainApproval:       "deny",
	}
}

func DefaultSync() *Sync {
	return &Sync{
		Enabled:            false,
		Branch:             "main",
		Interval:           configkit.Duration(15 * time.Minute),
		TransportSecretRef: defaultSyncTransportSecretRef(),
		KnownHostsRef:      defaultSyncKnownHostsRef(),
		GitBinary:          "/usr/bin/git",
		SSHBinary:          "/usr/bin/ssh",
		Include:            []string{"skills/**", "config-templates/**"},
	}
}

func DefaultChildren() *Children {
	return &Children{
		Enabled:            true,
		MaxDepth:           1,
		MaxActivePerParent: 2,
		MaxActiveGlobal:    4,
		DefaultTimeout:     configkit.Duration(5 * time.Minute),
		MaxTimeout:         configkit.Duration(30 * time.Minute),
		Recovery:           "interrupt",
	}
}

func defaultSyncTransportSecretRef() secret.Ref {
	return mustRef(fmt.Sprintf("env://%s_%s_%s", "AURA", "SYNC", "TRANSPORT_SECRET"))
}

func defaultSyncKnownHostsRef() secret.Ref {
	return mustRef(fmt.Sprintf("env://%s_%s_%s", "AURA", "SYNC", "KNOWN_HOSTS"))
}

func mustRef(text string) secret.Ref {
	ref, err := secret.ParseRef(text)
	if err != nil {
		panic(err)
	}
	return ref
}
