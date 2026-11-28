package telegram

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/anggasct/aura/internal/channels/kit/access"
	"github.com/anggasct/aura/internal/channels/kit/approval"
	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/command"
	"github.com/anggasct/aura/internal/platform/effect"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/health"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/secret"
	runtimechannelhost "github.com/anggasct/aura/internal/platform/turn/channelhost"
	runtimeingress "github.com/anggasct/aura/internal/platform/turn/ingress"
	"github.com/anggasct/aura/internal/platform/vision"
)

const (
	backoffBase        = time.Second
	rateLimitCap       = time.Minute
	maxDeliverInflight = 8
)

const RemediationReviewBot = "review-telegram-bot"

const (
	DeniedReasonIdentity = "identity_unmapped"
)

const (
	faultNone             = ""
	faultBotRefUnresolved = "telegram_token_unavailable"
	faultUnauthorized     = "telegram_unauthorized"
	faultPollConflict     = "telegram_poll_conflict"
	faultUnreachable      = "telegram_unreachable"
	faultIntake           = "telegram_intake_failed"

	faultWebhookSecretInvalid = "telegram_webhook_secret_invalid"
)

var faultDetails = map[string]string{
	faultBotRefUnresolved: "bot token reference does not resolve",
	faultUnauthorized:     "bot token was rejected by telegram",
	faultPollConflict:     "another getUpdates poller or a webhook holds this bot",
	faultUnreachable:      "telegram bot api is unreachable",
	faultIntake:           "accepted updates could not be handed to the runtime",

	faultWebhookSecretInvalid: "webhook secret reference does not resolve to a valid secret token",
}

type adapterState int32

const (
	stateDown adapterState = iota
	stateLive
)

var (
	_ runtimechannelhost.ChannelPort = (*Adapter)(nil)
	_ health.HealthProvider          = Adapters(nil)
)

type EffectRunner interface {
	Execute(ctx context.Context, req *effect.PrepareRequest, provider effect.Provider) (*effect.Intent, error)
}

type CursorStore interface {
	Load(ctx context.Context, source string) (nextUpdateID int64, found bool, err error)
	Advance(ctx context.Context, source string, nextUpdateID int64, at time.Time) error
}

type SessionEnsurer interface {
	EnsureSession(ctx context.Context, sessionID, ownerID, agentID string) error
}

type SessionResolver interface {
	ActiveSession(ctx context.Context, conversationKey string) (string, error)
}

type PersonaRouter interface {
	Resolve(scope *auraagent.RouteScope) (auraagent.RouteMatch, bool)
	Persona(id string) (auraagent.Definition, bool)
	ServesPublic(channel, account string) bool
	ServesPersona(channel, account, persona string) bool
}

type Deps struct {
	Config            *Config
	Account           *Account
	Cursors           CursorStore
	Effects           EffectRunner
	Sessions          SessionEnsurer
	Logger            *slog.Logger
	SessionResolver   SessionResolver
	PersonaRouter     PersonaRouter
	PrincipalResolver *principal.Resolver
	Commands          *command.Dispatcher
	SecretVault       *secret.Vault
	Media             MediaStore
	Vision            *vision.Service
	UpdateLedger      UpdateLedger
	Access            *access.Settings
	Transport         http.RoundTripper
	OnAdmitted        func(ctx context.Context, class principal.Class)
	OnDropped         func(ctx context.Context, reason string)
}

type Adapters []*Adapter

func (adapters Adapters) Check(ctx context.Context) []health.Finding {
	findings := make([]health.Finding, 0, len(adapters))
	for _, adapter := range adapters {
		findings = append(findings, adapter.Check(ctx)...)
	}
	return findings
}

func (adapters Adapters) HealthChecks() []health.RegisteredCheck {
	if len(adapters) == 0 {
		return nil
	}
	return []health.RegisteredCheck{{
		ID:          "telegram",
		Checker:     adapters,
		Remediation: RemediationReviewBot,
	}}
}

type Adapter struct {
	cfg        *Config
	account    *Account
	tokenRef   secret.Reference
	cursors    CursorStore
	effects    EffectRunner
	sessions   SessionEnsurer
	resolver   SessionResolver
	router     PersonaRouter
	owners     *principal.Resolver
	admitted   func(ctx context.Context, class principal.Class)
	dropped    func(ctx context.Context, reason string)
	access     access.Settings
	logger     *slog.Logger
	bot        *botClient
	sink       runtimeingress.IngressSink
	retryBase  time.Duration
	rateCap    time.Duration
	deliverSem chan struct{}
	state      atomic.Int32
	fault      atomic.Value

	webhookRef   secret.Reference
	webhookSlots chan struct{}
	webhook      atomic.Pointer[webhookSession]

	artifacts    MediaStore
	images       *vision.Service
	ledger       UpdateLedger
	mediaTimeout time.Duration

	identity        atomic.Pointer[botIdentity]
	identityRefresh chan struct{}

	approvals   *approval.Registry
	stops       *approval.Registry
	proposalTTL time.Duration
	targetsMu   sync.Mutex
	targets     map[string]approvalTarget
	commands    *command.Dispatcher

	secretVault   *secret.Vault
	presenceMu    sync.Mutex
	presence      map[string]*presenceSession
	typingRefresh time.Duration
	statusAfter   time.Duration
	elapsedTick   time.Duration
	deleteTimeout time.Duration
	holdMu        sync.Mutex
	holds         map[int64]time.Time
	latestPrivate latestInbound
}

func New(deps *Deps) (*Adapter, error) {
	if deps == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "telegram deps must not be nil")
	}
	if deps.Config == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "telegram config must not be nil")
	}
	if deps.Account == nil || deps.Account.Name == "" {
		return nil, fault.New(ErrorCodeInvalidArgument, "telegram account must not be nil or unnamed")
	}
	if deps.Cursors == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "cursor store must not be nil")
	}
	if deps.Effects == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "effect runner must not be nil")
	}
	if deps.Sessions == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "session ensurer must not be nil")
	}
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	tokenRef, err := parseTokenRef(deps.Account.BotTokenRef)
	if err != nil {
		return nil, err
	}
	webhookRef, webhookSlots, err := webhookSettings(deps.Config, deps.Account)
	if err != nil {
		return nil, err
	}
	approvals, err := approval.NewRegistry()
	if err != nil {
		return nil, fault.Wrap(ErrorCodeConnectionFailed, "approval token key is unavailable", err)
	}
	stops, err := approval.NewRegistry()
	if err != nil {
		return nil, fault.Wrap(ErrorCodeConnectionFailed, "stop token key is unavailable", err)
	}
	var settings access.Settings
	if deps.Access != nil {
		settings = *deps.Access
	}
	bot := newBotClient()
	if deps.Transport != nil {
		bot.http.Transport = deps.Transport
	}
	adapter := &Adapter{
		cfg:        deps.Config,
		account:    deps.Account,
		tokenRef:   tokenRef,
		cursors:    deps.Cursors,
		effects:    deps.Effects,
		sessions:   deps.Sessions,
		resolver:   deps.SessionResolver,
		router:     deps.PersonaRouter,
		owners:     deps.PrincipalResolver,
		admitted:   deps.OnAdmitted,
		dropped:    deps.OnDropped,
		access:     settings,
		logger:     logger,
		bot:        bot,
		retryBase:  backoffBase,
		rateCap:    rateLimitCap,
		deliverSem: make(chan struct{}, maxDeliverInflight),

		webhookRef:   webhookRef,
		webhookSlots: webhookSlots,

		artifacts:    deps.Media,
		images:       deps.Vision,
		ledger:       deps.UpdateLedger,
		mediaTimeout: defaultMediaTimeout,

		presence:    map[string]*presenceSession{},
		holds:       map[int64]time.Time{},
		approvals:   approvals,
		stops:       stops,
		proposalTTL: approval.ProposalCardTTL,
		targets:     map[string]approvalTarget{},
		commands:    deps.Commands,
		secretVault: deps.SecretVault,

		identityRefresh: make(chan struct{}, 1),
	}
	adapter.state.Store(int32(stateDown))
	adapter.fault.Store(faultNone)
	return adapter, nil
}

func parseTokenRef(ref secret.Ref) (secret.Reference, error) {
	return parseSecretReference(ref, "bot token")
}

func parseSecretReference(ref secret.Ref, what string) (secret.Reference, error) {
	if ref.IsZero() {
		return secret.Reference{}, fault.New(ErrorCodeInvalidArgument, what+" reference must use env:// or file://")
	}
	return ref.Reference(), nil
}

func (a *Adapter) Account() string {
	if a == nil || a.account == nil {
		return ""
	}
	return a.account.Name
}

func (a *Adapter) source() string {
	return principal.ChannelTelegram + ":" + a.account.Name
}

func (a *Adapter) ChannelName() string {
	return a.source()
}

func (a *Adapter) Start(ctx context.Context, sink runtimeingress.IngressSink) error {
	if sink == nil {
		return fault.New(ErrorCodeInvalidArgument, "ingress sink must not be nil")
	}
	a.sink = sink
	a.publishCommandMenu(ctx)
	var lookups sync.WaitGroup
	if len(a.account.AllowedChatIDs) > 0 {
		lookups.Go(func() { a.identityLoop(ctx) })
	}
	defer lookups.Wait()
	if a.account.WebhookMode() {
		a.webhookLoop(ctx)
		return nil
	}
	a.pollLoop(ctx)
	return nil
}

func (a *Adapter) Health(context.Context) runtimechannelhost.ChannelHealth {
	mode := ModePolling
	idle := "not polling"
	if a.account.WebhookMode() {
		mode = ModeWebhook
		idle = "webhook not registered"
	}
	if adapterState(a.state.Load()) == stateLive {
		return runtimechannelhost.ChannelHealth{Status: runtimechannelhost.ChannelHealthy, Detail: mode}
	}
	if detail, ok := faultDetails[a.currentFault()]; ok {
		return runtimechannelhost.ChannelHealth{Status: runtimechannelhost.ChannelDown, Detail: detail}
	}
	return runtimechannelhost.ChannelHealth{Status: runtimechannelhost.ChannelDown, Detail: idle}
}

func (a *Adapter) Check(ctx context.Context) []health.Finding {
	code := a.currentFault()
	if detail, ok := faultDetails[code]; ok {
		return []health.Finding{a.finding(code, detail)}
	}
	if !a.account.WebhookMode() || adapterState(a.state.Load()) != stateLive {
		return nil
	}
	return a.webhookFindings(ctx)
}

func (a *Adapter) finding(code, detail string) health.Finding {
	return health.Finding{
		Component:   "telegram",
		Code:        code,
		Status:      health.StatusDegraded,
		Severity:    health.SeverityWarning,
		Detail:      fmt.Sprintf("telegram account %s: %s", a.account.Name, detail),
		Remediation: RemediationReviewBot,
	}
}

func (a *Adapter) botToken() (string, bool) {
	token, err := a.tokenRef.Resolve()
	if err != nil {
		return "", false
	}
	token = strings.TrimSpace(token)
	return token, token != ""
}

func (a *Adapter) currentFault() string {
	code, _ := a.fault.Load().(string)
	return code
}

func (a *Adapter) markLive() {
	a.state.Store(int32(stateLive))
	a.fault.Store(faultNone)
}

func (a *Adapter) markFault(code string) {
	a.state.Store(int32(stateDown))
	a.fault.Store(code)
}
