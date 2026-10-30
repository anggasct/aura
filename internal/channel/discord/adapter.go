package discord

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/websocket"

	auraagent "github.com/anggasct/aura/internal/agent"
	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/health"
	"github.com/anggasct/aura/internal/principal"
	runtimechannelhost "github.com/anggasct/aura/internal/runtime/channelhost"
	runtimeingress "github.com/anggasct/aura/internal/runtime/ingress"
	"github.com/anggasct/aura/internal/secret"
	"github.com/anggasct/aura/internal/vision"
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
}

type threadRoute struct {
	parent  string
	persona string
}

type Adapter struct {
	cfg            *config.Discord
	account        *config.DiscordAccount
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
	router         PersonaRouter
	owners         *principal.Resolver
	admitted       func(ctx context.Context, class principal.Class)
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
	approvalsMu    sync.Mutex
	approvals      map[string]*pendingApproval
	seen           map[string]struct{}
	approvalKey    [32]byte
	targetsMu      sync.Mutex
	targets        map[string]approvalBinding
	deliverSem     chan struct{}
	selfID         string
	sessionID      string
	sequence       atomic.Int64
	hasSeq         atomic.Bool
	state          atomic.Int32
	gap            atomic.Bool
}

func (a *Adapter) SetVisionService(svc *vision.Service) {
	if a == nil {
		return
	}
	a.images.Store(svc)
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

func (a *Adapter) Account() string {
	if a == nil || a.account == nil {
		return ""
	}
	return a.account.Name
}

func (a *Adapter) source() string {
	return "discord:" + a.account.Name
}

func (a *Adapter) SetAdmissionObserver(observe func(ctx context.Context, class principal.Class)) {
	if a == nil {
		return
	}
	a.admitted = observe
}

func (a *Adapter) visionService() *vision.Service {
	if a == nil {
		return nil
	}
	return a.images.Load()
}

func New(cfg *config.Discord, account *config.DiscordAccount, resumes ResumeStore, effects EffectRunner, media MediaStore, sessions SessionEnsurer, logger *slog.Logger) (*Adapter, error) {
	if cfg == nil {
		return nil, Errorf(ErrorCodeInvalidArgument, "discord config must not be nil")
	}
	if account == nil || account.Name == "" {
		return nil, Errorf(ErrorCodeInvalidArgument, "discord account must not be nil or unnamed")
	}
	if resumes == nil {
		return nil, Errorf(ErrorCodeInvalidArgument, "resume store must not be nil")
	}
	if effects == nil {
		return nil, Errorf(ErrorCodeInvalidArgument, "effect runner must not be nil")
	}
	if media == nil {
		return nil, Errorf(ErrorCodeInvalidArgument, "media store must not be nil")
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
	var approvalKey [32]byte
	if _, err := rand.Read(approvalKey[:]); err != nil {
		return nil, Errorf(ErrorCodeConnectionFailed, "approval token key is unavailable")
	}
	adapter := &Adapter{
		cfg:            cfg,
		account:        account,
		tokenRef:       tokenRef,
		digest:         digestDiscordConfig(cfg, account),
		resumes:        resumes,
		effects:        effects,
		media:          media,
		sessions:       sessions,
		logger:         logger,
		gatewayURL:     gatewayURL,
		restBase:       discordRestBase,
		httpClient:     &http.Client{Timeout: mediaTimeout},
		gate:           newEditGate(time.Duration(cfg.MinEditInterval)),
		posted:         map[string][]string{},
		approvals:      map[string]*pendingApproval{},
		seen:           map[string]struct{}{},
		approvalKey:    approvalKey,
		targets:        map[string]approvalBinding{},
		triggerThreads: map[string]string{},
		threadParents:  map[string]threadRoute{},
		deliverSem:     make(chan struct{}, maxDeliverInflight),
		retryBase:      backoffBase,
		retryCap:       backoffMax,
		invalidDelay:   invalidWait,
		rateCap:        rateLimitCap,
	}
	adapter.state.Store(int32(stateDown))
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

func digestDiscordConfig(cfg *config.Discord, account *config.DiscordAccount) string {
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
		return Errorf(ErrorCodeInvalidArgument, "ingress sink must not be nil")
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
