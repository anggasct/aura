package config

import (
	"errors"
	"fmt"
	"maps"
	"net"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/anggasct/aura/internal/platform/configkit"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/secret"
)

type Webhook struct {
	Enabled            bool               `json:"enabled"`
	Listen             string             `json:"listen_address"`
	MaxBodySize        configkit.ByteSize `json:"max_body_size"`
	TimestampTolerance configkit.Duration `json:"timestamp_tolerance"`
	ReplayRetention    configkit.Duration `json:"replay_retention"`
	RequestsPerMinute  int                `json:"requests_per_minute"`
	Keys               []WebhookKey       `json:"keys"`
}

type WebhookKey struct {
	ID          string `json:"id"`
	SecretEnv   string `json:"secret_env"`
	AcceptUntil string `json:"accept_until"`
}

type Channels struct {
	Discord  Discord  `json:"discord"`
	Telegram Telegram `json:"telegram"`
	Email    Email    `json:"email"`
	HTTP     HTTP     `json:"http"`
}

type Broadcast struct {
	Timezone       string             `json:"timezone"`
	QuietHours     QuietHours         `json:"quiet_hours"`
	MaxDigestItems int                `json:"max_digest_items"`
	MaxDigestBytes int64              `json:"max_digest_bytes"`
	MaxAttempts    int                `json:"max_attempts"`
	MaxDeliveryAge configkit.Duration `json:"max_delivery_age"`
	MinDispatchGap configkit.Duration `json:"min_dispatch_gap"`
	Destinations   map[string]string  `json:"destinations"`
	Fallback       map[string]string  `json:"fallback"`
}

type Scheduler struct {
	Enabled             bool               `json:"enabled"`
	DefaultTimezone     string             `json:"default_timezone"`
	DefaultCatchUpGrace configkit.Duration `json:"default_catch_up_grace"`
	OccurrenceRetention configkit.Duration `json:"occurrence_retention"`
}

type Memory struct {
	Enabled              bool               `json:"enabled"`
	MaxDocuments         int                `json:"max_documents"`
	RecallTokenBudget    int                `json:"recall_token_budget"`
	Locale               string             `json:"locale"`
	EnglishStemming      bool               `json:"english_stemming"`
	SummaryPromptVersion string             `json:"summary_prompt_version"`
	SummaryTTL           configkit.Duration `json:"summary_ttl"`
	RangeThreshold       int                `json:"range_threshold"`
	RangeLiveWindow      int                `json:"range_live_window"`
	PurgeInterval        configkit.Duration `json:"purge_interval"`
	PurgeLimit           int                `json:"purge_limit"`
}

type QuietHours struct {
	Enabled bool   `json:"enabled"`
	Start   string `json:"start"`
	End     string `json:"end"`
}

type Skills struct {
	Enabled                 bool               `json:"enabled"`
	Roots                   []string           `json:"roots"`
	AutoSelect              bool               `json:"auto_select"`
	MaxIndexedSkills        int                `json:"max_indexed_skills"`
	MaxInstructionTokens    int                `json:"max_instruction_tokens"`
	MaxSessionContextTokens int                `json:"max_session_context_tokens"`
	MaxResourceBytes        int64              `json:"max_resource_bytes"`
	QuarantineRetention     configkit.Duration `json:"quarantine_retention"`
}

type Context struct {
	Enabled                    bool           `json:"enabled"`
	RecentCompleteTurns        int            `json:"recent_complete_turns"`
	SafetyMarginTokens         int            `json:"safety_margin_tokens"`
	ConservativeEstimatorRatio float64        `json:"conservative_estimator_ratio"`
	HighWaterRatio             float64        `json:"high_water_ratio"`
	MaxToolExcerptBytes        int            `json:"max_tool_excerpt_bytes"`
	Summary                    ContextSummary `json:"summary"`
}

type ContextSummary struct {
	Task            string `json:"task"`
	MaxSourceTokens int    `json:"max_source_tokens"`
	MaxOutputTokens int    `json:"max_output_tokens"`
	PromptVersion   string `json:"prompt_version"`
}

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

type Profile struct {
	Enabled                 bool    `json:"enabled"`
	MaxContextFacts         int     `json:"max_context_facts"`
	MaxContextTokens        int     `json:"max_context_tokens"`
	ExtractionQueueCapacity int     `json:"extraction_queue_capacity"`
	CandidateMinConfidence  float64 `json:"candidate_min_confidence"`
	PromptVersion           string  `json:"prompt_version"`
}

type Owner struct {
	Name        string            `json:"name,omitempty"`
	Language    string            `json:"language,omitempty"`
	Timezone    string            `json:"timezone,omitempty"`
	Preferences map[string]string `json:"preferences,omitempty"`
	Identities  []string          `json:"identities,omitempty"`
}

type Access struct {
	ApprovalTimeout  configkit.Duration `json:"approval_timeout"`
	StandingGrantTTL configkit.Duration `json:"standing_grant_ttl"`
	Budgets          AccessBudgets      `json:"budgets"`
	Retention        AccessRetention    `json:"retention"`
	HandoffTimeout   configkit.Duration `json:"handoff_timeout"`
}

type AccessRetention struct {
	Trusted configkit.Duration `json:"trusted"`
	Public  configkit.Duration `json:"public"`
}

type AccessBudgets struct {
	Trusted PrincipalBudget `json:"trusted"`
	Public  PrincipalBudget `json:"public"`
}

type PrincipalBudget struct {
	MaxTokensPerDay   int64 `json:"max_tokens_per_day"`
	MaxTurnsPerMinute int   `json:"max_turns_per_minute"`
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

const (
	BindingChannelDiscord  = "discord"
	BindingChannelTelegram = "telegram"
	BindingChannelEmail    = "email"
)

type Binding struct {
	Persona   string `json:"persona"`
	Channel   string `json:"channel"`
	Account   string `json:"account"`
	GuildID   string `json:"guild_id,omitempty"`
	ChannelID string `json:"channel_id,omitempty"`
	ChatID    string `json:"chat_id,omitempty"`
	ThreadID  string `json:"thread_id,omitempty"`
	UserID    string `json:"user_id,omitempty"`
}

type Terminal struct {
	RenderHz            int                `json:"render_hz"`
	MaxInputBytes       int                `json:"max_input_bytes"`
	InMemoryHistory     int                `json:"in_memory_history"`
	SecondInterruptTime configkit.Duration `json:"second_interrupt_window"`
	PlainApproval       string             `json:"plain_approval"`
}

type Runtime struct {
	MaxActiveTurns  int                `json:"max_active_turns"`
	MaxPendingTurns int                `json:"max_pending_turns"`
	TurnTimeout     configkit.Duration `json:"turn_timeout"`
	ShutdownTimeout configkit.Duration `json:"shutdown_timeout"`
	RunRetention    configkit.Duration `json:"run_retention"`
}

type Tools struct {
	Workspace            string        `json:"workspace"`
	MaxInlineResultBytes int64         `json:"max_inline_result_bytes"`
	Exec                 ToolExec      `json:"exec"`
	Fetch                ToolFetch     `json:"fetch"`
	WebSearch            ToolWebSearch `json:"web_search"`
	GitHub               ToolGitHub    `json:"github,omitzero"`
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

type Storage struct {
	Path               string             `json:"path"`
	BusyTimeout        configkit.Duration `json:"busy_timeout"`
	MaxOpenConnections int                `json:"max_open_connections"`
	ArtifactQuota      configkit.ByteSize `json:"artifact_quota"`
	BackupDirectory    string             `json:"backup_directory"`
	BackupInterval     configkit.Duration `json:"backup_interval"`
	BackupRetention    int                `json:"backup_retention"`
}

type Models struct {
	Definitions          map[string]ModelDefinition `json:"definitions"`
	Providers            map[string]ProviderPreset  `json:"providers,omitempty"`
	RequestTimeout       configkit.Duration         `json:"request_timeout"`
	StreamingIdleTimeout configkit.Duration         `json:"streaming_idle_timeout"`
	Routing              map[string]string          `json:"routing"`
}

type ModelDefinition struct {
	Protocol     string            `json:"protocol,omitempty"`
	Model        string            `json:"model"`
	BaseURL      string            `json:"base_url,omitempty"`
	Provider     string            `json:"provider,omitempty"`
	Custom       *CustomProvider   `json:"custom,omitempty"`
	APIKeyRef    secret.Ref        `json:"api_key_ref"`
	Capabilities ModelCapabilities `json:"capabilities"`
}

const (
	ProviderAuthBearer      = "bearer"
	ProviderSessionNone     = "none"
	ProviderSessionOpencode = "opencode-session"
	SessionHeaderName       = "x-opencode-session"
)

type ProviderPreset struct {
	Protocol        string            `json:"protocol"`
	BaseURL         string            `json:"base_url"`
	AuthScheme      string            `json:"auth_scheme"`
	Headers         map[string]string `json:"headers,omitempty"`
	SessionBehavior string            `json:"session_behavior"`
}

type CustomProvider = ProviderPreset

type ResolvedDefinition struct {
	Protocol        string
	BaseURL         string
	Headers         map[string]string
	SessionBehavior string
	ProviderName    string
}

func BuiltinProviders() map[string]ProviderPreset {
	return map[string]ProviderPreset{
		"openai": {
			Protocol:        ProtocolOpenAIResponses,
			BaseURL:         "https://api.openai.com/v1",
			AuthScheme:      ProviderAuthBearer,
			SessionBehavior: ProviderSessionNone,
		},
		"anthropic": {
			Protocol:        ProtocolAnthropicMessages,
			BaseURL:         "https://api.anthropic.com",
			AuthScheme:      ProviderAuthBearer,
			SessionBehavior: ProviderSessionNone,
		},
		"gemini": {
			Protocol:        ProtocolGeminiNative,
			BaseURL:         "https://generativelanguage.googleapis.com",
			AuthScheme:      ProviderAuthBearer,
			SessionBehavior: ProviderSessionNone,
		},
		"openrouter": {
			Protocol:        ProtocolOpenAIChatCompat,
			BaseURL:         "https://openrouter.ai/api/v1",
			AuthScheme:      ProviderAuthBearer,
			SessionBehavior: ProviderSessionNone,
		},
		"opencode-go": {
			Protocol:        ProtocolOpenAIResponses,
			BaseURL:         "https://opencode.ai/zen/go",
			AuthScheme:      ProviderAuthBearer,
			SessionBehavior: ProviderSessionOpencode,
		},
	}
}

func (m Models) ResolveDefinition(def *ModelDefinition) (ResolvedDefinition, error) {
	registry := BuiltinProviders()
	for name, preset := range m.Providers {
		registry[name] = preset
	}
	if def.Provider != "" && def.Custom != nil {
		return ResolvedDefinition{}, configkit.Field("models.definitions", fault.New(ErrorCodeModelProviderInvalid, "entry sets both provider and custom"))
	}
	if def.Custom != nil {
		if strings.TrimSpace(def.Protocol) != "" || strings.TrimSpace(def.BaseURL) != "" {
			return ResolvedDefinition{}, configkit.Field("models.definitions", fault.New(ErrorCodeModelProviderInvalid, "entry with custom must not set protocol or base_url"))
		}
		if err := ValidateProviderShape("custom", *def.Custom); err != nil {
			return ResolvedDefinition{}, err
		}
		return ResolvedDefinition{
			Protocol:        def.Custom.Protocol,
			BaseURL:         strings.TrimRight(def.Custom.BaseURL, "/"),
			Headers:         copyProviderHeaders(def.Custom.Headers),
			SessionBehavior: def.Custom.SessionBehavior,
			ProviderName:    "custom",
		}, nil
	}
	if def.Provider != "" {
		if strings.TrimSpace(def.Protocol) != "" {
			return ResolvedDefinition{}, configkit.Field("models.definitions", fault.New(ErrorCodeModelProviderInvalid, "entry with provider must not set protocol"))
		}
		preset, ok := registry[def.Provider]
		if !ok {
			return ResolvedDefinition{}, configkit.Field("models.definitions", fault.New(ErrorCodeModelProviderInvalid, "entry references unknown provider"))
		}
		if err := ValidateProviderShape("provider", preset); err != nil {
			return ResolvedDefinition{}, err
		}
		baseURL := preset.BaseURL
		if strings.TrimSpace(def.BaseURL) != "" {
			baseURL = def.BaseURL
		}
		if err := ValidateBaseURL(baseURL); err != nil {
			return ResolvedDefinition{}, configkit.Field("models.definitions", fault.New(ErrorCodeModelProviderInvalid, "entry with provider has invalid base_url: "+err.Error()))
		}
		return ResolvedDefinition{
			Protocol:        preset.Protocol,
			BaseURL:         strings.TrimRight(baseURL, "/"),
			Headers:         copyProviderHeaders(preset.Headers),
			SessionBehavior: preset.SessionBehavior,
			ProviderName:    def.Provider,
		}, nil
	}
	return ResolvedDefinition{
		Protocol:        def.Protocol,
		BaseURL:         strings.TrimRight(def.BaseURL, "/"),
		SessionBehavior: ProviderSessionNone,
	}, nil
}

func ValidateProviderShape(where string, preset ProviderPreset) error {
	if !ValidProtocol(preset.Protocol) {
		return configkit.Field(where, fault.New(ErrorCodeModelProviderInvalid, "uses unsupported protocol (must be one of "+strings.Join(ProtocolNames(), ", ")+")"))
	}
	if strings.TrimSpace(preset.BaseURL) == "" {
		return configkit.Field(where, fault.New(ErrorCodeModelProviderInvalid, "requires a non-empty base_url"))
	}
	if err := ValidateBaseURL(preset.BaseURL); err != nil {
		return configkit.Field(where, fault.New(ErrorCodeModelProviderInvalid, "has invalid base_url: "+err.Error()))
	}
	if preset.AuthScheme != ProviderAuthBearer {
		return configkit.Field(where, fault.New(ErrorCodeModelProviderInvalid, "uses unsupported auth scheme (must be "+ProviderAuthBearer+")"))
	}
	if preset.SessionBehavior != ProviderSessionNone && preset.SessionBehavior != ProviderSessionOpencode {
		return configkit.Field(where, fault.New(ErrorCodeModelProviderInvalid, "uses unsupported session behavior (must be "+ProviderSessionNone+" or "+ProviderSessionOpencode+")"))
	}
	return validateProviderHeaders(where, preset.Headers)
}

func validateProviderHeaders(where string, headers map[string]string) error {
	for _, name := range slices.Sorted(maps.Keys(headers)) {
		value := headers[name]
		if strings.TrimSpace(name) == "" || strings.ContainsAny(name, " \t:") {
			return configkit.Field(where, fault.New(ErrorCodeModelProviderInvalid, "sets invalid header name"))
		}
		if strings.EqualFold(name, "authorization") || strings.EqualFold(name, "content-type") || strings.EqualFold(name, SessionHeaderName) {
			return configkit.Field(where, fault.New(ErrorCodeModelProviderInvalid, fmt.Sprintf("header %q collides with a managed header", name)))
		}
		trimmed := strings.TrimSpace(value)
		if strings.HasPrefix(trimmed, "env://") || strings.HasPrefix(trimmed, "file://") || strings.Contains(trimmed, "${") {
			return configkit.Field(where, fault.New(ErrorCodeModelProviderInvalid, fmt.Sprintf("header %q must not carry a secret reference", name)))
		}
	}
	return nil
}

func copyProviderHeaders(headers map[string]string) map[string]string {
	if len(headers) == 0 {
		return nil
	}
	out := make(map[string]string, len(headers))
	for name, value := range headers {
		out[name] = value
	}
	return out
}

const (
	DefaultModelRouteMaxProviderAttempts = 4
	DefaultModelRouteRetryDelayBudget    = 20 * time.Second
)

type ModelRoute struct {
	Candidates          []string           `json:"candidates"`
	MaxProviderAttempts int                `json:"max_provider_attempts"`
	RetryDelayBudget    configkit.Duration `json:"retry_delay_budget"`
	CostBudgetUSD       float64            `json:"cost_budget_usd"`
	Circuit             ModelRouteCircuit  `json:"circuit"`
}

func DefaultModelRoute() ModelRoute {
	return ModelRoute{
		MaxProviderAttempts: DefaultModelRouteMaxProviderAttempts,
		RetryDelayBudget:    configkit.Duration(DefaultModelRouteRetryDelayBudget),
	}
}

func (*ModelRoute) Default() any {
	return DefaultModelRoute()
}

type ModelRouteCircuit struct {
	FailureThreshold int                `json:"failure_threshold"`
	OpenDuration     configkit.Duration `json:"open_duration"`
	MaxOpenDuration  configkit.Duration `json:"max_open_duration"`
}

type AgentLimits struct {
	TurnTimeout configkit.Duration `json:"turn_timeout"`
}

type AgentDefinition struct {
	ID              string             `json:"id"`
	Name            string             `json:"name,omitempty"`
	Description     string             `json:"description"`
	Instructions    string             `json:"instructions"`
	SoulFile        string             `json:"soul_file"`
	Soul            string             `json:"-"`
	Tools           []string           `json:"tools"`
	Capabilities    []string           `json:"capabilities"`
	ModelRoute      string             `json:"model_route"`
	MemoryScope     string             `json:"memory_scope,omitempty"`
	Limits          AgentLimits        `json:"limits"`
	Audience        string             `json:"audience,omitempty"`
	Pairing         bool               `json:"pairing,omitempty"`
	PrincipalTools  []string           `json:"principal_tools,omitempty"`
	PublicTools     []string           `json:"public_tools,omitempty"`
	ApprovalTimeout configkit.Duration `json:"approval_timeout,omitempty"`
	Messages        map[string]string  `json:"messages,omitempty"`
	GroupContext    string             `json:"group_context,omitempty"`
	Evals           AgentEvals         `json:"evals,omitzero"`
	Workflows       []string           `json:"workflows,omitempty"`
	Plugins         []string           `json:"plugins,omitempty"`

	Knowledge          []string `json:"knowledge,omitempty"`
	PrincipalKnowledge []string `json:"principal_knowledge,omitempty"`
	PublicKnowledge    []string `json:"public_knowledge,omitempty"`
}

type AgentEvals struct {
	Dir      string             `json:"dir,omitempty"`
	Behavior *AgentEvalBehavior `json:"behavior,omitempty"`
}

type AgentEvalBehavior struct {
	Required            bool    `json:"required,omitempty"`
	MinScore            float64 `json:"min_score,omitempty"`
	JudgeRoute          string  `json:"judge_route,omitempty"`
	Schedule            string  `json:"schedule,omitempty"`
	EnforceOnRegression bool    `json:"enforce_on_regression,omitempty"`
}

type Agents struct {
	Definitions []AgentDefinition `json:"definitions"`
}

type Workflows struct {
	DefinitionsDir     string             `json:"definitions_dir"`
	MaxConcurrentSteps int                `json:"max_concurrent_steps"`
	DefaultStepTimeout configkit.Duration `json:"default_step_timeout"`
	Generated          GeneratedWorkflows `json:"generated"`
}

type GeneratedWorkflows struct {
	MaxSteps  int   `json:"max_steps"`
	MaxTokens int64 `json:"max_tokens"`
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

type ModelCapabilities struct {
	Streaming            bool   `json:"streaming"`
	Tools                bool   `json:"tools"`
	StructuredOutput     bool   `json:"structured_output"`
	Vision               bool   `json:"vision"`
	Audio                bool   `json:"audio"`
	Reasoning            bool   `json:"reasoning"`
	ContextTokens        int    `json:"context_tokens"`
	Tokenizer            string `json:"tokenizer"`
	UsageReporting       bool   `json:"usage_reporting"`
	MicrosPerInputToken  int64  `json:"micros_per_input_token"`
	MicrosPerOutputToken int64  `json:"micros_per_output_token"`
}

const (
	ProtocolOpenAIResponses   = "openai_responses"
	ProtocolOpenAIChatCompat  = "openai_chat_compat"
	ProtocolAnthropicMessages = "anthropic_messages"
	ProtocolGeminiNative      = "gemini_native"
)

var (
	envNamePattern       = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	mcpServerNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,62}$`)
	syncBranchPattern    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)
)

func ProtocolNames() []string {
	return []string{ProtocolOpenAIResponses, ProtocolOpenAIChatCompat, ProtocolAnthropicMessages, ProtocolGeminiNative}
}

func ValidProtocol(p string) bool {
	return slices.Contains(ProtocolNames(), p)
}

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

func DefaultRuntime() Runtime {
	return Runtime{
		MaxActiveTurns:  4,
		MaxPendingTurns: 64,
		TurnTimeout:     configkit.Duration(5 * time.Minute),
		ShutdownTimeout: configkit.Duration(30 * time.Second),
		RunRetention:    configkit.Duration(720 * time.Hour),
	}
}

func DefaultAgents() *Agents {
	return &Agents{}
}

func DefaultWorkflows() *Workflows {
	return &Workflows{
		MaxConcurrentSteps: 4,
		DefaultStepTimeout: configkit.Duration(15 * time.Minute),
		Generated:          GeneratedWorkflows{MaxSteps: 20, MaxTokens: 200000},
	}
}

func DefaultModels() Models {
	return Models{
		Definitions:          map[string]ModelDefinition{},
		RequestTimeout:       configkit.Duration(120 * time.Second),
		StreamingIdleTimeout: configkit.Duration(60 * time.Second),
		Routing:              defaultModelsRouting(),
	}
}

func DefaultStorage() Storage {
	return Storage{
		BusyTimeout:        configkit.Duration(5 * time.Second),
		MaxOpenConnections: 4,
		ArtifactQuota:      configkit.ByteSize(5 << 30),
		BackupInterval:     configkit.Duration(24 * time.Hour),
		BackupRetention:    7,
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

func DefaultWebhook() Webhook {
	return Webhook{
		Listen:             "127.0.0.1:8282",
		MaxBodySize:        configkit.ByteSize(1 << 20),
		TimestampTolerance: configkit.Duration(5 * time.Minute),
		ReplayRetention:    configkit.Duration(24 * time.Hour),
		RequestsPerMinute:  60,
	}
}

func DefaultChannels() Channels {
	return Channels{
		Discord: Discord{
			MinEditInterval:          configkit.Duration(2 * time.Second),
			MaxAttachmentBytes:       20971520,
			ThreadAutoArchiveMinutes: 1440,
		},
		Telegram: Telegram{
			Polling: TelegramPolling{
				Timeout:    configkit.Duration(30 * time.Second),
				MaxBackoff: configkit.Duration(time.Minute),
			},
			MinEditInterval:    configkit.Duration(2 * time.Second),
			MaxAttachmentBytes: TelegramMaxAttachmentBytes,
			Webhook: TelegramWebhook{
				ListenAddress:  TelegramWebhookListenAddress,
				MaxBodyBytes:   TelegramWebhookMaxBodyBytes,
				MaxConnections: TelegramWebhookMaxConnections,
			},
		},
		Email: Email{
			MaxAttachmentBytes: EmailMaxAttachmentBytes,
		},
		HTTP: HTTP{
			ListenAddress:       HTTPListenAddress,
			MaxBodyBytes:        HTTPMaxBodyBytes,
			RequestsPerMinute:   HTTPRequestsPerMinute,
			MaxConcurrentPerKey: HTTPMaxConcurrentPerKey,
			MaxHistoryMessages:  HTTPMaxHistoryMessages,
			MaxHistoryBytes:     HTTPMaxHistoryBytes,
			KeepaliveInterval:   configkit.Duration(HTTPKeepaliveInterval),
		},
	}
}

func DefaultBroadcast() Broadcast {
	return Broadcast{
		Timezone:       "UTC",
		QuietHours:     QuietHours{Enabled: true, Start: "23:00", End: "07:00"},
		MaxDigestItems: 20,
		MaxDigestBytes: 12000,
		MaxAttempts:    3,
		MaxDeliveryAge: configkit.Duration(24 * time.Hour),
		MinDispatchGap: configkit.Duration(5 * time.Second),
		Destinations:   map[string]string{},
		Fallback:       map[string]string{},
	}
}

func DefaultScheduler() Scheduler {
	return Scheduler{
		Enabled:             true,
		DefaultTimezone:     "UTC",
		DefaultCatchUpGrace: configkit.Duration(15 * time.Minute),
		OccurrenceRetention: configkit.Duration(720 * time.Hour),
	}
}

func DefaultMemory() Memory {
	return Memory{
		Enabled:              true,
		MaxDocuments:         10,
		RecallTokenBudget:    2000,
		SummaryPromptVersion: "memory-summary-v1",
		SummaryTTL:           configkit.Duration(720 * time.Hour),
		RangeThreshold:       20,
		RangeLiveWindow:      5,
		PurgeInterval:        configkit.Duration(1 * time.Hour),
		PurgeLimit:           100,
	}
}

func DefaultSkills() *Skills {
	return &Skills{
		Enabled:                 true,
		Roots:                   []string{"/srv/aura/skills"},
		AutoSelect:              true,
		MaxIndexedSkills:        256,
		MaxInstructionTokens:    5000,
		MaxSessionContextTokens: 15000,
		MaxResourceBytes:        8388608,
		QuarantineRetention:     configkit.Duration(720 * time.Hour),
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

func DefaultContext() *Context {
	return &Context{
		Enabled:                    true,
		RecentCompleteTurns:        10,
		SafetyMarginTokens:         1024,
		ConservativeEstimatorRatio: 0.80,
		HighWaterRatio:             0.90,
		MaxToolExcerptBytes:        2048,
		Summary: ContextSummary{
			Task:            "compression",
			MaxSourceTokens: 32768,
			MaxOutputTokens: 2048,
			PromptVersion:   "v1",
		},
	}
}

func DefaultProfile() *Profile {
	return &Profile{
		Enabled:                 true,
		MaxContextFacts:         20,
		MaxContextTokens:        1024,
		ExtractionQueueCapacity: 64,
		CandidateMinConfidence:  0.70,
		PromptVersion:           "v1",
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

func DefaultAccess() *Access {
	return &Access{
		ApprovalTimeout:  configkit.Duration(30 * time.Minute),
		StandingGrantTTL: configkit.Duration(168 * time.Hour),
		Budgets: AccessBudgets{
			Trusted: PrincipalBudget{MaxTokensPerDay: 200000, MaxTurnsPerMinute: 6},
			Public:  PrincipalBudget{MaxTokensPerDay: 20000, MaxTurnsPerMinute: 3},
		},
		Retention:      AccessRetention{Trusted: configkit.Duration(4320 * time.Hour), Public: configkit.Duration(720 * time.Hour)},
		HandoffTimeout: configkit.Duration(24 * time.Hour),
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

func defaultModelsRouting() map[string]string {
	return map[string]string{
		"agent":       "primary",
		"summarize":   "auxiliary",
		"title":       "auxiliary",
		"curation":    "auxiliary",
		"compression": "auxiliary",
		"profiling":   "auxiliary",
	}
}

func ValidateBaseURL(raw string) error {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("is not a valid URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("scheme must be http or https")
	}
	if u.Host == "" {
		return errors.New("missing host")
	}
	if u.User != nil {
		return errors.New("user info is not allowed")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return errors.New("query and fragment are not allowed")
	}
	if u.Scheme == "http" && !isLoopbackURL(u) {
		return errors.New("https is required for non-loopback endpoints")
	}
	return nil
}

func IsLoopbackBaseURL(raw string) bool {
	if raw == "" {
		return false
	}
	u, err := url.Parse(raw)
	return err == nil && isLoopbackURL(u)
}

func isLoopbackURL(u *url.URL) bool {
	host := strings.TrimSuffix(u.Hostname(), ".")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
