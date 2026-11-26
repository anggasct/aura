package email

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

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
}

func RecoverEffects(ctx context.Context, journal EffectRecoverer, logger *slog.Logger) error {
	if journal == nil {
		return fault.New(ErrorCodeInvalidArgument, "effect journal must not be nil")
	}
	if logger == nil {
		logger = slog.Default()
	}
	report, err := journal.RecoverProvider(ctx, ProviderName)
	if err != nil {
		return fmt.Errorf("recover email effects: %w", err)
	}
	logger.InfoContext(ctx, "email effects recovered", "component", "email", "scanned", report.Scanned, "claimed", report.Claimed)
	return nil
}
