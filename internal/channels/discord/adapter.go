package discord

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/websocket"

	"github.com/anggasct/aura/internal/channels/kit/access"
	"github.com/anggasct/aura/internal/channels/kit/approval"
	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/command"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/health"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/secret"
	runtimechannelhost "github.com/anggasct/aura/internal/platform/turn/channelhost"
	runtimeingress "github.com/anggasct/aura/internal/platform/turn/ingress"
	"github.com/anggasct/aura/internal/platform/vision"
)

const (
	maxFrameBytes  = 1 << 20
	maxRestBytes   = 1 << 16
	writeTimeout   = 10 * time.Second
	restTimeout    = 10 * time.Second
	mediaTimeout   = 60 * time.Second
	helloTimeout   = 15 * time.Second
	backoffBase    = time.Second
	backoffMax     = time.Minute
	invalidWait    = 2 * time.Second
	readMultiplier = 2
	stableSession  = time.Minute
)

const (
	intakeQueueCap     = 128
	maxDeliverInflight = 8
)

const discordRestBase = "https://discord.com/api/v10"

const RemediationReviewGateway = "review-gateway-resume"

type adapterState int32

const (
	stateDown adapterState = iota
	stateLive
	stateDegraded
)

var _ runtimechannelhost.ChannelPort = (*Adapter)(nil)

var _ runtimechannelhost.PresencePort = (*Adapter)(nil)

type PersonaRouter interface {
	Resolve(scope *auraagent.RouteScope) (auraagent.RouteMatch, bool)
	Personas() []string
	Persona(id string) (auraagent.Definition, bool)
	ServesPublic(channel, account string) bool
	ServesPersona(channel, account, persona string) bool
}

type threadRoute struct {
	parent  string
	persona string
}

type Deps struct {
	Config            *Config
	Account           *Account
	Resumes           ResumeStore
	Effects           EffectRunner
	Media             MediaStore
	Sessions          SessionEnsurer
	Logger            *slog.Logger
	SessionResolver   SessionResolver
	PersonaRouter     PersonaRouter
	PrincipalResolver *principal.Resolver
	Commands          *command.Dispatcher
	SecretVault       *secret.Vault
	Vision            *vision.Service
	Access            *access.Settings
	OnAdmitted        func(context.Context, principal.Class)
	OnDropped         func(context.Context, string)
}

type Adapters []*Adapter

var _ health.HealthProvider = Adapters(nil)

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
		ID:          "discord",
		Checker:     adapters,
		Remediation: RemediationReviewGateway,
	}}
}

type Adapter struct {
	cfg            *Config
	account        *Account
	tokenRef       secret.Reference
	digest         string
	resumes        ResumeStore
	logger         *slog.Logger
	sink           runtimeingress.IngressSink
	dial           func(ctx context.Context, url string) (*websocket.Conn, error)
	gatewayURL     string
	restBase       string
	httpClient     *http.Client
	resumeURL      atomic.Value
	retryBase      time.Duration
	retryCap       time.Duration
	invalidDelay   time.Duration
	rateCap        time.Duration
	effects        EffectRunner
	media          MediaStore
	sessions       SessionEnsurer
	resolver       SessionResolver
	router         PersonaRouter
	commands       *command.Dispatcher
	owners         *principal.Resolver
	admitted       func(ctx context.Context, class principal.Class)
	dropped        func(ctx context.Context, reason string)
	images         atomic.Pointer[vision.Service]
	gate           *editGate
	postedMu       sync.Mutex
	posted         map[string][]string
	threadsMu      sync.Mutex
	triggerThreads map[string]string
	threadParents  map[string]threadRoute
	presenceMu     sync.Mutex
	presence       map[string]*presenceSession
	secretVault    *secret.Vault
	typingRefresh  time.Duration
	statusAfter    time.Duration
	approvals      *approval.Registry
	targetsMu      sync.Mutex
	targets        map[string]approvalBinding
	access         AccessSettings
	ownerDMsMu     sync.Mutex
	ownerDMs       map[string]string
	deliverSem     chan struct{}
	selfID         string
	selfName       string
	sessionID      string
	sequence       atomic.Int64
	hasSeq         atomic.Bool
	state          atomic.Int32
	gap            atomic.Bool
}

func (a *Adapter) activeSession(ctx context.Context, conversationKey string) (string, error) {
	if a.resolver == nil {
		return conversationKey, nil
	}
	active, err := a.resolver.ActiveSession(ctx, conversationKey)
	if err != nil {
		return "", fmt.Errorf("resolve conversation session: %w", err)
	}
	if active == "" {
		return conversationKey, nil
	}
	return active, nil
}

func (a *Adapter) Account() string {
	if a == nil || a.account == nil {
		return ""
	}
	return a.account.Name
}

func (a *Adapter) source() string {
	return "discord:" + a.account.Name
}

func (a *Adapter) visionService() *vision.Service {
	if a == nil {
		return nil
	}
	return a.images.Load()
}

func New(deps *Deps) (*Adapter, error) {
	if deps == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "discord deps must not be nil")
	}
	if deps.Config == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "discord config must not be nil")
	}
	if deps.Account == nil || deps.Account.Name == "" {
		return nil, fault.New(ErrorCodeInvalidArgument, "discord account must not be nil or unnamed")
	}
	if deps.Resumes == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "resume store must not be nil")
	}
	if deps.Effects == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "effect runner must not be nil")
	}
	if deps.Media == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "media store must not be nil")
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
	approvals, err := approval.NewRegistry()
	if err != nil {
		return nil, fault.Wrap(ErrorCodeConnectionFailed, "approval token key is unavailable", err)
	}
	adapter := &Adapter{
		cfg:            deps.Config,
		account:        deps.Account,
		tokenRef:       tokenRef,
		digest:         digestDiscordConfig(deps.Config, deps.Account),
		resumes:        deps.Resumes,
		effects:        deps.Effects,
		media:          deps.Media,
		sessions:       deps.Sessions,
		logger:         logger,
		resolver:       deps.SessionResolver,
		router:         deps.PersonaRouter,
		owners:         deps.PrincipalResolver,
		commands:       deps.Commands,
		secretVault:    deps.SecretVault,
		admitted:       deps.OnAdmitted,
		dropped:        deps.OnDropped,
		gatewayURL:     gatewayURL,
		restBase:       discordRestBase,
		httpClient:     &http.Client{Timeout: mediaTimeout},
		gate:           newEditGate(time.Duration(deps.Config.MinEditInterval)),
		posted:         map[string][]string{},
		approvals:      approvals,
		targets:        map[string]approvalBinding{},
		ownerDMs:       map[string]string{},
		triggerThreads: map[string]string{},
		threadParents:  map[string]threadRoute{},
		deliverSem:     make(chan struct{}, maxDeliverInflight),
		retryBase:      backoffBase,
		retryCap:       backoffMax,
		invalidDelay:   invalidWait,
		rateCap:        rateLimitCap,
	}
	if deps.Vision != nil {
		adapter.images.Store(deps.Vision)
	}
	if deps.Access != nil {
		adapter.access = *deps.Access
	}
	adapter.state.Store(int32(stateDown))
	return adapter, nil
}

func parseTokenRef(ref secret.Ref) (secret.Reference, error) {
	if ref.IsZero() {
		return secret.Reference{}, fault.New(ErrorCodeInvalidArgument, "bot token reference must use env:// or file://")
	}
	return ref.Reference(), nil
}

func digestDiscordConfig(cfg *Config, account *Account) string {
	ids := make([]string, 0, len(account.AllowedUserIDs)+len(account.AllowedGuildIDs)+len(account.AllowedChannelIDs))
	ids = append(ids, account.AllowedUserIDs...)
	ids = append(ids, account.AllowedGuildIDs...)
	ids = append(ids, account.AllowedChannelIDs...)
	slices.Sort(ids)
	digest := sha256.New()
	_, _ = fmt.Fprintf(digest, "%s|%d|%t|%d|%d|", account.Name, requiredIntents, account.AcceptDMs, int64(cfg.MinEditInterval), cfg.MaxAttachmentBytes)
	for _, id := range ids {
		_, _ = fmt.Fprintf(digest, "%s|", id)
	}
	return hex.EncodeToString(digest.Sum(nil))
}

func (a *Adapter) Start(ctx context.Context, sink runtimeingress.IngressSink) error {
	if sink == nil {
		return fault.New(ErrorCodeInvalidArgument, "ingress sink must not be nil")
	}
	a.sink = sink
	if a.dial == nil {
		a.dial = a.dialGateway
	}
	return a.connectLoop(ctx)
}

func (a *Adapter) ChannelName() string {
	return a.source()
}

func (a *Adapter) postedMessages(key string) ([]string, bool) {
	a.postedMu.Lock()
	defer a.postedMu.Unlock()
	ids, ok := a.posted[key]
	if !ok {
		return nil, false
	}
	kept := make([]string, 0, len(ids))
	for _, id := range ids {
		if id != "" {
			kept = append(kept, id)
		}
	}
	return kept, len(kept) > 0
}

func (a *Adapter) rememberPosted(key string, ids []string) {
	a.postedMu.Lock()
	defer a.postedMu.Unlock()
	if len(a.posted) >= maxTrackedDeliveries {
		for drop := range a.posted {
			delete(a.posted, drop)
			break
		}
	}
	a.posted[key] = ids
}

func (a *Adapter) Health(ctx context.Context) runtimechannelhost.ChannelHealth {
	switch adapterState(a.state.Load()) {
	case stateLive:
		return runtimechannelhost.ChannelHealth{Status: runtimechannelhost.ChannelHealthy, Detail: "streaming"}
	case stateDegraded:
		return runtimechannelhost.ChannelHealth{Status: runtimechannelhost.ChannelDegraded, Detail: "resume gap observed"}
	default:
		return runtimechannelhost.ChannelHealth{Status: runtimechannelhost.ChannelDown, Detail: "not connected"}
	}
}

func (a *Adapter) Check(ctx context.Context) []health.Finding {
	if !a.gap.Load() {
		return nil
	}
	return []health.Finding{{
		Component:   "discord",
		Code:        "possible_ingress_gap",
		Status:      health.StatusDegraded,
		Severity:    health.SeverityWarning,
		Detail:      "discord gateway for account " + a.account.Name + " resumed without continuity; some events may be missing",
		Remediation: RemediationReviewGateway,
	}}
}

func (a *Adapter) markGap(ctx context.Context) {
	if a.gap.CompareAndSwap(false, true) {
		a.state.Store(int32(stateDegraded))
		a.logger.WarnContext(ctx, "gateway continuity gap", "component", "discord")
	}
}
