package telemetry

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

const (
	MetricAccessDeniedTotal      = "access.denied.total"
	MetricApprovalRoutedTotal    = "approval.routed.total"
	MetricStandingGrantUsedTotal = "standing_grant.used.total"
	AttrAccessDeniedReason       = "access.denied.reason"
	AttrApprovalRoutedResult     = "approval.routed.result"
	MetricPairingRequestsTotal   = "pairing.requests.total"
	AttrPairingResult            = "pairing.result"
	MetricBudgetExhaustedTotal   = "budget.exhausted.total"
	AttrBudgetLimit              = "budget.limit"
)

type AccessRecorder struct {
	denied   metric.Int64Counter
	routed   metric.Int64Counter
	standing metric.Int64Counter
	pairing  metric.Int64Counter
	budget   metric.Int64Counter
}

func NewAccessRecorder(mp metric.MeterProvider) (*AccessRecorder, error) {
	if mp == nil {
		mp = otel.GetMeterProvider()
	}
	meter := mp.Meter(ScopeName)
	denied, err := meter.Int64Counter(MetricAccessDeniedTotal, metric.WithDescription("channel messages dropped by persona audience"))
	if err != nil {
		return nil, fmt.Errorf("telemetry: create access denied counter: %w", err)
	}
	routed, err := meter.Int64Counter(MetricApprovalRoutedTotal, metric.WithDescription("owner-routed approvals by result"))
	if err != nil {
		return nil, fmt.Errorf("telemetry: create approval routed counter: %w", err)
	}
	standing, err := meter.Int64Counter(MetricStandingGrantUsedTotal, metric.WithDescription("tool calls allowed by an owner standing grant"))
	if err != nil {
		return nil, fmt.Errorf("telemetry: create standing grant counter: %w", err)
	}
	pairing, err := meter.Int64Counter(MetricPairingRequestsTotal, metric.WithDescription("dm pairing requests by result"))
	if err != nil {
		return nil, fmt.Errorf("telemetry: create pairing requests counter: %w", err)
	}
	budget, err := meter.Int64Counter(MetricBudgetExhaustedTotal, metric.WithDescription("non-owner turns refused by a principal budget"))
	if err != nil {
		return nil, fmt.Errorf("telemetry: create budget exhausted counter: %w", err)
	}
	return &AccessRecorder{denied: denied, routed: routed, standing: standing, pairing: pairing, budget: budget}, nil
}

func (r *AccessRecorder) BudgetExhausted(ctx context.Context, class, limit string) {
	if r == nil {
		return
	}
	r.budget.Add(ctx, 1, metric.WithAttributes(attribute.String(AttrPrincipalClass, class), attribute.String(AttrBudgetLimit, limit)))
}

func (r *AccessRecorder) Denied(ctx context.Context, reason string) {
	if r == nil {
		return
	}
	r.denied.Add(ctx, 1, metric.WithAttributes(attribute.String(AttrAccessDeniedReason, reason)))
}

func (r *AccessRecorder) ApprovalRouted(ctx context.Context, result string) {
	if r == nil {
		return
	}
	r.routed.Add(ctx, 1, metric.WithAttributes(attribute.String(AttrApprovalRoutedResult, result)))
}

func (r *AccessRecorder) StandingGrantUsed(ctx context.Context) {
	if r == nil {
		return
	}
	r.standing.Add(ctx, 1)
}

func (r *AccessRecorder) PairingRequest(ctx context.Context, result string) {
	if r == nil {
		return
	}
	r.pairing.Add(ctx, 1, metric.WithAttributes(attribute.String(AttrPairingResult, result)))
}
