package telemetry

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

const (
	WorkspaceTaintKindWrite   = "write"
	WorkspaceTaintKindExec    = "exec"
	WorkspaceTaintKindCarried = "carried"
	WorkspaceTaintKindWide    = "wide"

	ConfirmationOutcomeApplied = "applied"
	ConfirmationOutcomeInvalid = "invalid"
)

type TaintRecorder struct {
	marks         metric.Int64Counter
	workspaceRecs metric.Int64Counter
	confirmations metric.Int64Counter
}

func NewTaintRecorder(mp metric.MeterProvider) (*TaintRecorder, error) {
	if mp == nil {
		mp = otel.GetMeterProvider()
	}
	meter := mp.Meter(ScopeName)
	var err error
	r := &TaintRecorder{}
	if r.marks, err = meter.Int64Counter(MetricTurnTaintMarksTotal,
		metric.WithDescription("session taint marks written by source")); err != nil {
		return nil, fmt.Errorf("telemetry: create turn taint marks counter: %w", err)
	}
	if r.workspaceRecs, err = meter.Int64Counter(MetricWorkspaceTaintRecordsTotal,
		metric.WithDescription("workspace taint records written by kind")); err != nil {
		return nil, fmt.Errorf("telemetry: create workspace taint records counter: %w", err)
	}
	if r.confirmations, err = meter.Int64Counter(MetricOperationConfirmationTotal,
		metric.WithDescription("operation confirmation code checks by outcome")); err != nil {
		return nil, fmt.Errorf("telemetry: create operation confirmation counter: %w", err)
	}
	return r, nil
}

func (r *TaintRecorder) RecordMark(ctx context.Context, source string) {
	if r == nil {
		return
	}
	r.marks.Add(ctx, 1, metric.WithAttributeSet(attribute.NewSet(attribute.String(AttrTaintSource, source))))
}

func (r *TaintRecorder) RecordWorkspaceRecord(ctx context.Context, kind string) {
	if r == nil {
		return
	}
	r.workspaceRecs.Add(ctx, 1, metric.WithAttributeSet(attribute.NewSet(attribute.String(AttrWorkspaceTaintKind, kind))))
}

func (r *TaintRecorder) RecordConfirmation(ctx context.Context, outcome string) {
	if r == nil {
		return
	}
	r.confirmations.Add(ctx, 1, metric.WithAttributeSet(attribute.NewSet(attribute.String(AttrOutcome, outcome))))
}
