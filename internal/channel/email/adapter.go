package email

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	auraagent "github.com/anggasct/aura/internal/agent"
	"github.com/anggasct/aura/internal/channel/access"
	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/health"
	"github.com/anggasct/aura/internal/principal"
	runtimechannelhost "github.com/anggasct/aura/internal/runtime/channelhost"
	runtimeingress "github.com/anggasct/aura/internal/runtime/ingress"
	"github.com/anggasct/aura/internal/secret"
)

const (
	backoffBase     = time.Second
	backoffCap      = time.Minute
	fetchWindow     = 100
	maxHeaderBytes  = 256 << 10
	messageOverhead = 1 << 20
)

const RemediationReviewMailbox = "review-email-account"

const (
	faultNone               = ""
	faultPasswordUnresolved = "email_password_unavailable"
	faultLoginRejected      = "email_login_rejected"
	faultUnreachable        = "email_unreachable"
	faultProtocol           = "email_protocol_invalid"
	faultIntake             = "email_intake_failed"

	findingUIDValidityChanged = "email_uidvalidity_changed"
)

var faultDetails = map[string]string{
	faultPasswordUnresolved: "mailbox password reference does not resolve",
	faultLoginRejected:      "mailbox server rejected the credentials",
	faultUnreachable:        "mailbox server is unreachable or its certificate is not trusted",
	faultProtocol:           "mailbox server sent a response this reader cannot use",
	faultIntake:             "accepted mail could not be handed to the runtime",
}

type adapterState int32

const (
	stateDown adapterState = iota
	stateLive
)

var (
	_ runtimechannelhost.ChannelPort     = (*Adapter)(nil)
	_ runtimechannelhost.ChannelIdentity = (*Adapter)(nil)
)

type Cursor struct {
	UIDValidity uint32
	LastUID     uint32
}

type CursorStore interface {
	Load(ctx context.Context, source string) (cursor Cursor, found bool, err error)
	Save(ctx context.Context, source string, cursor Cursor, at time.Time) error
}

type Sessions interface {
	EnsureSession(ctx context.Context, sessionID, ownerID, agentID string) error
	SessionOwner(ctx context.Context, sessionID string) (ownerID string, found bool, err error)
}

type PersonaRouter interface {
	Resolve(scope *auraagent.RouteScope) (auraagent.RouteMatch, bool)
	Persona(id string) (auraagent.Definition, bool)
}

type AccessSettings struct {
	Budgets access.BudgetGate
	Evals   access.EvalGate
}

type Adapter struct {
	cfg       *config.Email
	account   *config.EmailAccount
	connector Connector
	cursors   CursorStore
	sessions  Sessions
	router    PersonaRouter
	owners    *principal.Resolver
	admitted  func(ctx context.Context, class principal.Class)
	dropped   func(ctx context.Context, reason string)
	access    AccessSettings
	logger    *slog.Logger
	sink      runtimeingress.IngressSink
	retryBase time.Duration
	retryCap  time.Duration
	window    uint32

	state           atomic.Int32
	fault           atomic.Value
	validityChanged atomic.Bool
}

func New(cfg *config.Email, account *config.EmailAccount, connector Connector, cursors CursorStore, sessions Sessions, logger *slog.Logger) (*Adapter, error) {
	switch {
	case cfg == nil:
		return nil, Errorf(ErrorCodeInvalidArgument, "email config must not be nil")
	case account == nil || account.Name == "":
		return nil, Errorf(ErrorCodeInvalidArgument, "email account must not be nil or unnamed")
	case connector == nil:
		return nil, Errorf(ErrorCodeInvalidArgument, "mailbox connector must not be nil")
	case cursors == nil:
		return nil, Errorf(ErrorCodeInvalidArgument, "cursor store must not be nil")
	case sessions == nil:
		return nil, Errorf(ErrorCodeInvalidArgument, "session store must not be nil")
	}
	if logger == nil {
		logger = slog.Default()
	}
	adapter := &Adapter{
		cfg:       cfg,
		account:   account,
		connector: connector,
		cursors:   cursors,
		sessions:  sessions,
		logger:    logger,
		retryBase: backoffBase,
		retryCap:  backoffCap,
		window:    fetchWindow,
	}
	adapter.state.Store(int32(stateDown))
	adapter.fault.Store(faultNone)
	return adapter, nil
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

func (a *Adapter) SetAccess(settings *AccessSettings) {
	if a == nil || settings == nil {
		return
	}
	a.access = *settings
}

func (a *Adapter) SetAdmissionObserver(observe func(ctx context.Context, class principal.Class)) {
	if a == nil {
		return
	}
	a.admitted = observe
}

func (a *Adapter) Account() string {
	if a == nil || a.account == nil {
		return ""
	}
	return a.account.Name
}

func (a *Adapter) source() string {
	return principal.ChannelEmail + ":" + a.account.Name
}

func (a *Adapter) ChannelName() string {
	return a.source()
}

func (a *Adapter) Start(ctx context.Context, sink runtimeingress.IngressSink) error {
	if sink == nil {
		return Errorf(ErrorCodeInvalidArgument, "ingress sink must not be nil")
	}
	a.sink = sink
	a.run(ctx)
	return nil
}

func (a *Adapter) Deliver(context.Context, *runtimechannelhost.DeliveryRequest) (runtimechannelhost.ProviderReceipt, error) {
	return runtimechannelhost.ProviderReceipt{}, Errorf(ErrorCodeDeliveryUnavailable, "email replies are not available yet")
}

func (a *Adapter) Health(context.Context) runtimechannelhost.ChannelHealth {
	if adapterState(a.state.Load()) == stateLive {
		return runtimechannelhost.ChannelHealth{Status: runtimechannelhost.ChannelHealthy, Detail: "mailbox reachable"}
	}
	if detail, ok := faultDetails[a.currentFault()]; ok {
		return runtimechannelhost.ChannelHealth{Status: runtimechannelhost.ChannelDown, Detail: detail}
	}
	return runtimechannelhost.ChannelHealth{Status: runtimechannelhost.ChannelDown, Detail: "mailbox not connected"}
}

func (a *Adapter) Check(context.Context) []health.Finding {
	var findings []health.Finding
	if detail, ok := faultDetails[a.currentFault()]; ok {
		findings = append(findings, a.finding(a.currentFault(), detail))
	}
	if a.validityChanged.Load() {
		findings = append(findings, a.finding(findingUIDValidityChanged, "mailbox identity changed; mail that arrived before the change was not ingested"))
	}
	return findings
}

func (a *Adapter) finding(code, detail string) health.Finding {
	return health.Finding{
		Component:   "email",
		Code:        code,
		Status:      health.StatusDegraded,
		Severity:    health.SeverityWarning,
		Detail:      fmt.Sprintf("email account %s: %s", a.account.Name, detail),
		Remediation: RemediationReviewMailbox,
	}
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

func parsePasswordRef(raw string) (secret.Reference, error) {
	switch {
	case strings.HasPrefix(raw, "env://"):
		return secret.Reference{Env: strings.TrimPrefix(raw, "env://")}, nil
	case strings.HasPrefix(raw, "file://"):
		return secret.Reference{File: strings.TrimPrefix(raw, "file://")}, nil
	default:
		return secret.Reference{}, Errorf(ErrorCodeInvalidArgument, "mailbox password reference must use env:// or file://")
	}
}
