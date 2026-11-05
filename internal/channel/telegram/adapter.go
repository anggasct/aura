package telegram

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	auraagent "github.com/anggasct/aura/internal/agent"
	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/effect"
	"github.com/anggasct/aura/internal/health"
	"github.com/anggasct/aura/internal/principal"
	runtimechannelhost "github.com/anggasct/aura/internal/runtime/channelhost"
	runtimeingress "github.com/anggasct/aura/internal/runtime/ingress"
	"github.com/anggasct/aura/internal/secret"
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
)

var faultDetails = map[string]string{
	faultBotRefUnresolved: "bot token reference does not resolve",
	faultUnauthorized:     "bot token was rejected by telegram",
	faultPollConflict:     "another getUpdates poller or a webhook holds this bot",
	faultUnreachable:      "telegram bot api is unreachable",
	faultIntake:           "accepted updates could not be handed to the runtime",
}

type adapterState int32

const (
	stateDown adapterState = iota
	stateLive
)

var _ runtimechannelhost.ChannelPort = (*Adapter)(nil)

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
}

type Adapter struct {
	cfg        *config.Telegram
	account    *config.TelegramAccount
	tokenRef   secret.Reference
	cursors    CursorStore
	effects    EffectRunner
	sessions   SessionEnsurer
	resolver   SessionResolver
	router     PersonaRouter
	owners     *principal.Resolver
	admitted   func(ctx context.Context, class principal.Class)
	denied     func(ctx context.Context, reason string)
	dropped    func(ctx context.Context, reason string)
	access     AccessSettings
	logger     *slog.Logger
	bot        *botClient
	sink       runtimeingress.IngressSink
	retryBase  time.Duration
	rateCap    time.Duration
	deliverSem chan struct{}
	state      atomic.Int32
	fault      atomic.Value

	secretVault   *secret.Vault
	presenceMu    sync.Mutex
	presence      map[string]*presenceSession
	typingRefresh time.Duration
	statusAfter   time.Duration
	holdMu        sync.Mutex
	holds         map[int64]time.Time
}

func New(cfg *config.Telegram, account *config.TelegramAccount, cursors CursorStore, effects EffectRunner, sessions SessionEnsurer, logger *slog.Logger) (*Adapter, error) {
	if cfg == nil {
		return nil, Errorf(ErrorCodeInvalidArgument, "telegram config must not be nil")
	}
	if account == nil || account.Name == "" {
		return nil, Errorf(ErrorCodeInvalidArgument, "telegram account must not be nil or unnamed")
	}
	if cursors == nil {
		return nil, Errorf(ErrorCodeInvalidArgument, "cursor store must not be nil")
	}
	if effects == nil {
		return nil, Errorf(ErrorCodeInvalidArgument, "effect runner must not be nil")
	}
	if sessions == nil {
		return nil, Errorf(ErrorCodeInvalidArgument, "session ensurer must not be nil")
	}
	if logger == nil {
		logger = slog.Default()
	}
	tokenRef, err := parseTokenRef(account.BotTokenRef)
	if err != nil {
		return nil, err
	}
	adapter := &Adapter{
		cfg:        cfg,
		account:    account,
		tokenRef:   tokenRef,
		cursors:    cursors,
		effects:    effects,
		sessions:   sessions,
		logger:     logger,
		bot:        newBotClient(),
		retryBase:  backoffBase,
		rateCap:    rateLimitCap,
		deliverSem: make(chan struct{}, maxDeliverInflight),
		presence:   map[string]*presenceSession{},
		holds:      map[int64]time.Time{},
	}
	adapter.state.Store(int32(stateDown))
	adapter.fault.Store(faultNone)
	return adapter, nil
}

func parseTokenRef(raw string) (secret.Reference, error) {
	switch {
	case strings.HasPrefix(raw, "env://"):
		return secret.Reference{Env: strings.TrimPrefix(raw, "env://")}, nil
	case strings.HasPrefix(raw, "file://"):
		return secret.Reference{File: strings.TrimPrefix(raw, "file://")}, nil
	default:
		return secret.Reference{}, Errorf(ErrorCodeInvalidArgument, "bot token reference must use env:// or file://")
	}
}

func (a *Adapter) SetPrincipalResolver(owners *principal.Resolver) {
	if a == nil {
		return
	}
	a.owners = owners
}

func (a *Adapter) SetPersonaRouter(router PersonaRouter) {
	if a == nil {
		return
	}
	a.router = router
}

func (a *Adapter) SetSessionResolver(resolver SessionResolver) {
	if a == nil {
		return
	}
	a.resolver = resolver
}

func (a *Adapter) SetAdmissionObserver(observe func(ctx context.Context, class principal.Class)) {
	if a == nil {
		return
	}
	a.admitted = observe
}

func (a *Adapter) SetDenialObserver(observe func(ctx context.Context, reason string)) {
	if a == nil {
		return
	}
	a.denied = observe
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
		return Errorf(ErrorCodeInvalidArgument, "ingress sink must not be nil")
	}
	a.sink = sink
	return a.pollLoop(ctx)
}

func (a *Adapter) Health(context.Context) runtimechannelhost.ChannelHealth {
	if adapterState(a.state.Load()) == stateLive {
		return runtimechannelhost.ChannelHealth{Status: runtimechannelhost.ChannelHealthy, Detail: "polling"}
	}
	if detail, ok := faultDetails[a.currentFault()]; ok {
		return runtimechannelhost.ChannelHealth{Status: runtimechannelhost.ChannelDown, Detail: detail}
	}
	return runtimechannelhost.ChannelHealth{Status: runtimechannelhost.ChannelDown, Detail: "not polling"}
}

func (a *Adapter) Check(context.Context) []health.Finding {
	fault := a.currentFault()
	detail, ok := faultDetails[fault]
	if !ok {
		return nil
	}
	return []health.Finding{{
		Component:   "telegram",
		Code:        fault,
		Status:      health.StatusDegraded,
		Severity:    health.SeverityWarning,
		Detail:      fmt.Sprintf("telegram account %s: %s", a.account.Name, detail),
		Remediation: RemediationReviewBot,
	}}
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
	fault, _ := a.fault.Load().(string)
	return fault
}

func (a *Adapter) markLive() {
	a.state.Store(int32(stateLive))
	a.fault.Store(faultNone)
}

func (a *Adapter) markFault(fault string) {
	a.state.Store(int32(stateDown))
	a.fault.Store(fault)
}
