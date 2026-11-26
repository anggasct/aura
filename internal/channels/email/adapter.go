package email

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/anggasct/aura/internal/channels/kit/access"
	"github.com/anggasct/aura/internal/channels/kit/approval"
	auraagent "github.com/anggasct/aura/internal/platform/agent"
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

	faultSMTPUnresolved  = "email_smtp_unresolved"
	faultSMTPLogin       = "email_smtp_login_rejected"
	faultSMTPTLS         = "email_smtp_tls_unsafe"
	faultSMTPUnreachable = "email_smtp_unreachable"

	findingUIDValidityChanged = "email_uidvalidity_changed"
)

var faultDetails = map[string]string{
	faultPasswordUnresolved: "mailbox password reference does not resolve",
	faultLoginRejected:      "mailbox server rejected the credentials",
	faultUnreachable:        "mailbox server is unreachable or its certificate is not trusted",
	faultProtocol:           "mailbox server sent a response this reader cannot use",
	faultIntake:             "accepted mail could not be handed to the runtime",
	faultSMTPUnresolved:     "submission password reference does not resolve",
	faultSMTPLogin:          "submission server rejected the credentials",
	faultSMTPTLS:            "submission server does not offer a trusted encrypted connection",
	faultSMTPUnreachable:    "submission server is unreachable",
}

type adapterState int32

const (
	stateDown adapterState = iota
	stateLive
)

var (
	_ runtimechannelhost.ChannelPort     = (*Adapter)(nil)
	_ runtimechannelhost.ChannelIdentity = (*Adapter)(nil)
	_ effect.Provider                    = (*Adapter)(nil)
)

type Cursor struct {
	UIDValidity uint32
	LastUID     uint32
}

type CursorStore interface {
	Load(ctx context.Context, source string) (cursor Cursor, found bool, err error)
	Save(ctx context.Context, source string, cursor Cursor, at time.Time) error
}

type EffectRunner interface {
	Execute(ctx context.Context, req *effect.PrepareRequest, provider effect.Provider) (*effect.Intent, error)
}

type Sessions interface {
	EnsureSession(ctx context.Context, sessionID, ownerID, agentID string) error
	SessionOwner(ctx context.Context, sessionID string) (ownerID string, found bool, err error)
}

type PersonaRouter interface {
	Resolve(scope *auraagent.RouteScope) (auraagent.RouteMatch, bool)
	Persona(id string) (auraagent.Definition, bool)
	ServesPublic(channel, account string) bool
}

type AccessSettings struct {
	Budgets  access.BudgetGate
	Evals    access.EvalGate
	Handoffs access.HandoffGate
	Erasure  access.PrincipalEraser
}

type Deps struct {
	Config            *Config
	Account           *Account
	Connector         Connector
	Mailer            Mailer
	Cursors           CursorStore
	Effects           EffectRunner
	Sessions          Sessions
	Logger            *slog.Logger
	PersonaRouter     PersonaRouter
	PrincipalResolver *principal.Resolver
	Access            *AccessSettings
	OwnerRoute        *approval.OwnerRoute
	HandoffNotifier   HandoffNotifier
	Media             MediaStore
	Vision            *vision.Service
	OnAdmitted        func(ctx context.Context, class principal.Class)
	OnDropped         func(ctx context.Context, reason string)
}

type Adapter struct {
	cfg       *Config
	account   *Account
	connector Connector
	mailer    Mailer
	effects   EffectRunner
	cursors   CursorStore
	sessions  Sessions
	router    PersonaRouter
	owners    *principal.Resolver
	admitted  func(ctx context.Context, class principal.Class)
	dropped   func(ctx context.Context, reason string)
	access    AccessSettings
	approvals *approval.OwnerRoute
	notifier  HandoffNotifier
	artifacts MediaStore
	images    *vision.Service
	targets   sessionTargets
	logger    *slog.Logger
	sink      runtimeingress.IngressSink
	retryBase time.Duration
	retryCap  time.Duration
	window    uint32
	now       func() time.Time

	deliverSem chan struct{}

	state           atomic.Int32
	fault           atomic.Value
	smtpFault       atomic.Value
	validityChanged atomic.Bool
}

func New(deps *Deps) (*Adapter, error) {
	if deps == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "email dependencies must not be nil")
	}
	switch {
	case deps.Config == nil:
		return nil, fault.New(ErrorCodeInvalidArgument, "email config must not be nil")
	case deps.Account == nil || deps.Account.Name == "":
		return nil, fault.New(ErrorCodeInvalidArgument, "email account must not be nil or unnamed")
	case deps.Connector == nil:
		return nil, fault.New(ErrorCodeInvalidArgument, "mailbox connector must not be nil")
	case deps.Mailer == nil:
		return nil, fault.New(ErrorCodeInvalidArgument, "mail submitter must not be nil")
	case deps.Cursors == nil:
		return nil, fault.New(ErrorCodeInvalidArgument, "cursor store must not be nil")
	case deps.Effects == nil:
		return nil, fault.New(ErrorCodeInvalidArgument, "effect runner must not be nil")
	case deps.Sessions == nil:
		return nil, fault.New(ErrorCodeInvalidArgument, "session store must not be nil")
	}
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	var accessSettings AccessSettings
	if deps.Access != nil {
		accessSettings = *deps.Access
	}
	adapter := &Adapter{
		cfg:        deps.Config,
		account:    deps.Account,
		connector:  deps.Connector,
		mailer:     deps.Mailer,
		effects:    deps.Effects,
		cursors:    deps.Cursors,
		sessions:   deps.Sessions,
		router:     deps.PersonaRouter,
		owners:     deps.PrincipalResolver,
		admitted:   deps.OnAdmitted,
		dropped:    deps.OnDropped,
		access:     accessSettings,
		approvals:  deps.OwnerRoute,
		notifier:   deps.HandoffNotifier,
		artifacts:  deps.Media,
		images:     deps.Vision,
		logger:     logger,
		retryBase:  backoffBase,
		retryCap:   backoffCap,
		window:     fetchWindow,
		now:        time.Now,
		deliverSem: make(chan struct{}, maxDeliverInflight),
	}
	adapter.state.Store(int32(stateDown))
	adapter.fault.Store(faultNone)
	adapter.smtpFault.Store(faultNone)
	return adapter, nil
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
		return fault.New(ErrorCodeInvalidArgument, "ingress sink must not be nil")
	}
	a.sink = sink
	a.run(ctx)
	return nil
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
	if detail, ok := faultDetails[a.currentSMTPFault()]; ok {
		findings = append(findings, a.finding(a.currentSMTPFault(), detail))
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
	code, _ := a.fault.Load().(string)
	return code
}

func (a *Adapter) currentSMTPFault() string {
	code, _ := a.smtpFault.Load().(string)
	return code
}

var submitFaults = map[ErrorCode]string{
	ErrorCodeCredentialsUnavailable: faultSMTPUnresolved,
	ErrorCodeLoginRejected:          faultSMTPLogin,
	ErrorCodeTLSRequired:            faultSMTPTLS,
	ErrorCodeTLSUntrusted:           faultSMTPTLS,
	ErrorCodeConnectionFailed:       faultSMTPUnreachable,
	ErrorCodeSubmissionDeferred:     faultSMTPUnreachable,
}

func (a *Adapter) noteSubmitFailure(code ErrorCode) {
	if name, ok := submitFaults[code]; ok {
		a.smtpFault.Store(name)
	}
}

func (a *Adapter) markLive() {
	a.state.Store(int32(stateLive))
	a.fault.Store(faultNone)
}

func (a *Adapter) markFault(name string) {
	a.state.Store(int32(stateDown))
	a.fault.Store(name)
}

func parsePasswordRef(ref secret.Ref) (secret.Reference, error) {
	if ref.IsZero() {
		return secret.Reference{}, fault.New(ErrorCodeInvalidArgument, "mailbox password reference must use env:// or file://")
	}
	return ref.Reference(), nil
}
