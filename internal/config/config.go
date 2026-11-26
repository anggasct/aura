package config

import (
	"time"

	"github.com/anggasct/aura/internal/modules/fetch"
	"github.com/anggasct/aura/internal/modules/github"
	"github.com/anggasct/aura/internal/modules/search"
	"github.com/anggasct/aura/internal/modules/shellexec"
	"github.com/anggasct/aura/internal/platform/configkit"
	"github.com/anggasct/aura/internal/platform/secret"
)

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
	Workspace            string           `json:"workspace"`
	MaxInlineResultBytes int64            `json:"max_inline_result_bytes"`
	Exec                 shellexec.Config `json:"exec"`
	Fetch                fetch.Config     `json:"fetch"`
	WebSearch            search.Config    `json:"web_search"`
	GitHub               github.Config    `json:"github,omitzero"`
}

func DefaultTools() *Tools {
	return &Tools{
		Workspace:            "/srv/aura/workspace",
		MaxInlineResultBytes: 65536,
		Exec:                 shellexec.Default(),
		Fetch:                fetch.Default(),
		WebSearch:            search.Default(),
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
