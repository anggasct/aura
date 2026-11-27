package email

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/effect"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/health"
)

var ErrNoRoute = errors.New("email session has no account to send from")

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
		ID:          "email",
		Checker:     adapters,
		Remediation: RemediationReviewMailbox,
	}}
}

type Outbox struct {
	adapters []*Adapter
}

func NewOutbox(adapters []*Adapter) *Outbox {
	return &Outbox{adapters: adapters}
}

func (o *Outbox) adapterFor(sessionID string) (*Adapter, Route, bool) {
	if o == nil {
		return nil, Route{}, false
	}
	for _, adapter := range o.adapters {
		if route, ok := adapter.SessionRoute(sessionID); ok {
			return adapter, route, true
		}
	}
	return nil, Route{}, false
}

func (o *Outbox) RouteOf(sessionID string) (Route, bool) {
	_, route, ok := o.adapterFor(sessionID)
	return route, ok
}

func (o *Outbox) Send(ctx context.Context, outgoing *Outgoing) (string, error) {
	if outgoing == nil {
		return "", fault.New(ErrorCodeInvalidArgument, "outgoing message must not be nil")
	}
	adapter, _, ok := o.adapterFor(outgoing.SessionID)
	if !ok {
		return "", ErrNoRoute
	}
	return adapter.Send(ctx, outgoing)
}

type EffectRecoverer interface {
	RecoverProvider(ctx context.Context, provider string) (effect.RecoveryReport, error)
	RecoverIntents(ctx context.Context, ids []string) (effect.RecoveryReport, error)
}

type RecoveryDeps struct {
	Journal EffectRecoverer
	Logger  *slog.Logger
	Clock   durable.Clock
}

type RecoveryRetry struct {
	At        time.Time
	IntentIDs []string
}

func (r RecoveryRetry) Pending() bool { return !r.At.IsZero() && len(r.IntentIDs) > 0 }

func (d *RecoveryDeps) validate() error {
	if d == nil || d.Journal == nil {
		return fault.New(ErrorCodeInvalidArgument, "effect journal must not be nil")
	}
	if d.Logger == nil {
		return fault.New(ErrorCodeInvalidArgument, "recovery logger must not be nil")
	}
	if d.Clock == nil {
		return fault.New(ErrorCodeInvalidArgument, "recovery clock must not be nil")
	}
	return nil
}

func (d *RecoveryDeps) logPass(ctx context.Context, report *effect.RecoveryReport) {
	d.Logger.InfoContext(ctx, "email effects recovered", "component", "email", "scanned", report.Scanned, "claimed", report.Claimed)
}

func RecoverEffects(ctx context.Context, deps *RecoveryDeps) (RecoveryRetry, error) {
	if err := deps.validate(); err != nil {
		return RecoveryRetry{}, err
	}
	report, err := deps.Journal.RecoverProvider(ctx, ProviderName)
	if err != nil {
		return RecoveryRetry{}, fmt.Errorf("recover email effects: %w", err)
	}
	deps.logPass(ctx, &report)
	return RecoveryRetry{At: report.RetryAt, IntentIDs: report.Pending}, nil
}

func RecoverEffectsAt(ctx context.Context, deps *RecoveryDeps, retry RecoveryRetry) error {
	if err := deps.validate(); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-deps.Clock.Timer(retry.At.Sub(deps.Clock.Now()) + time.Nanosecond):
	}
	report, err := deps.Journal.RecoverIntents(ctx, retry.IntentIDs)
	if err != nil {
		return fmt.Errorf("recover email effects: %w", err)
	}
	deps.logPass(ctx, &report)
	return nil
}
