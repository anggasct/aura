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
	"github.com/anggasct/aura/internal/channel/approval"
	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/effect"
	"github.com/anggasct/aura/internal/health"
	"github.com/anggasct/aura/internal/principal"
	runtimechannelhost "github.com/anggasct/aura/internal/runtime/channelhost"
	runtimeingress "github.com/anggasct/aura/internal/runtime/ingress"
	"github.com/anggasct/aura/internal/secret"
	"github.com/anggasct/aura/internal/vision"
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

type Adapter struct {
	cfg       *config.Email
	account   *config.EmailAccount
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
	images    atomic.Pointer[vision.Service]
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

func New(cfg *config.Email, account *config.EmailAccount, connector Connector, mailer Mailer, cursors CursorStore, effects EffectRunner, sessions Sessions, logger *slog.Logger) (*Adapter, error) {
	switch {
	case cfg == nil:
		return nil, Errorf(ErrorCodeInvalidArgument, "email config must not be nil")
	case account == nil || account.Name == "":
		return nil, Errorf(ErrorCodeInvalidArgument, "email account must not be nil or unnamed")
	case connector == nil:
		return nil, Errorf(ErrorCodeInvalidArgument, "mailbox connector must not be nil")
	case mailer == nil:
		return nil, Errorf(ErrorCodeInvalidArgument, "mail submitter must not be nil")
	case cursors == nil:
		return nil, Errorf(ErrorCodeInvalidArgument, "cursor store must not be nil")
	case effects == nil:
		return nil, Errorf(ErrorCodeInvalidArgument, "effect runner must not be nil")
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
		mailer:    mailer,
		effects:   effects,
		cursors:   cursors,
		sessions:  sessions,
		logger:    logger,
		retryBase: backoffBase,
		retryCap:  backoffCap,
		window:    fetchWindow,
		now:       time.Now,

		deliverSem: make(chan struct{}, maxDeliverInflight),
	}
	adapter.state.Store(int32(stateDown))
	adapter.fault.Store(faultNone)
	adapter.smtpFault.Store(faultNone)
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
	fault, _ := a.fault.Load().(string)
	return fault
}

func (a *Adapter) currentSMTPFault() string {
	fault, _ := a.smtpFault.Load().(string)
	return fault
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
	if fault, ok := submitFaults[code]; ok {
		a.smtpFault.Store(fault)
	}
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
