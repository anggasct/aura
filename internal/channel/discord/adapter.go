package discord

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/net/websocket"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/health"
	runtimechannelhost "github.com/anggasct/aura/internal/runtime/channelhost"
	runtimeingress "github.com/anggasct/aura/internal/runtime/ingress"
	"github.com/anggasct/aura/internal/secret"
)

const (
	maxFrameBytes  = 1 << 20
	maxRestBytes   = 1 << 16
	writeTimeout   = 10 * time.Second
	restTimeout    = 10 * time.Second
	helloTimeout   = 15 * time.Second
	backoffBase    = time.Second
	backoffMax     = time.Minute
	invalidWait    = 2 * time.Second
	readMultiplier = 2
	stableSession  = time.Minute
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

type Adapter struct {
	cfg          *config.Discord
	tokenRef     secret.Reference
	digest       string
	resumes      ResumeStore
	logger       *slog.Logger
	sink         runtimeingress.IngressSink
	dial         func(ctx context.Context, url string) (*websocket.Conn, error)
	gatewayURL   string
	restBase     string
	httpClient   *http.Client
	resumeURL    atomic.Value
	retryBase    time.Duration
	retryCap     time.Duration
	invalidDelay time.Duration
	selfID       string
	sessionID    string
	sequence     atomic.Int64
	hasSeq       atomic.Bool
	state        atomic.Int32
	gap          atomic.Bool
}

func New(cfg *config.Discord, resumes ResumeStore, logger *slog.Logger) (*Adapter, error) {
	if cfg == nil {
		return nil, Errorf(ErrorCodeInvalidArgument, "discord config must not be nil")
	}
	if resumes == nil {
		return nil, Errorf(ErrorCodeInvalidArgument, "resume store must not be nil")
	}
	if logger == nil {
		logger = slog.Default()
	}
	tokenRef, err := parseTokenRef(cfg.BotTokenRef)
	if err != nil {
		return nil, err
	}
	adapter := &Adapter{
		cfg:          cfg,
		tokenRef:     tokenRef,
		digest:       digestDiscordConfig(cfg),
		resumes:      resumes,
		logger:       logger,
		gatewayURL:   gatewayURL,
		restBase:     discordRestBase,
		httpClient:   &http.Client{Timeout: restTimeout},
		retryBase:    backoffBase,
		retryCap:     backoffMax,
		invalidDelay: invalidWait,
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

func digestDiscordConfig(cfg *config.Discord) string {
	ids := make([]string, 0, len(cfg.AllowedUserIDs)+len(cfg.AllowedGuildIDs)+len(cfg.AllowedChannelIDs))
	ids = append(ids, cfg.AllowedUserIDs...)
	ids = append(ids, cfg.AllowedGuildIDs...)
	ids = append(ids, cfg.AllowedChannelIDs...)
	slices.Sort(ids)
	digest := sha256.New()
	_, _ = fmt.Fprintf(digest, "%s|%d|%t|%d|%d|", cfg.Instance, requiredIntents, cfg.AcceptDMs, int64(cfg.MinEditInterval), cfg.MaxAttachmentBytes)
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

func (a *Adapter) Deliver(ctx context.Context, req *runtimechannelhost.DeliveryRequest) (runtimechannelhost.ProviderReceipt, error) {
	if req == nil {
		return runtimechannelhost.ProviderReceipt{}, Errorf(ErrorCodeInvalidArgument, "delivery request must not be nil")
	}
	return runtimechannelhost.ProviderReceipt{}, Errorf(ErrorCodeDeliveryUnavailable, "direct delivery is not available")
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
		Detail:      "discord gateway resumed without continuity; some events may be missing",
		Remediation: RemediationReviewGateway,
	}}
}

func (a *Adapter) markGap(ctx context.Context) {
	if a.gap.CompareAndSwap(false, true) {
		a.state.Store(int32(stateDegraded))
		a.logger.WarnContext(ctx, "gateway continuity gap", "component", "discord")
	}
}
