package tools

import (
	"encoding/json"
	"errors"
	"slices"
)

type Risk string

const (
	RiskRead        Risk = "read"
	RiskChange      Risk = "change"
	RiskTrustGrant  Risk = "trust_grant"
	RiskDestructive Risk = "destructive"
)

type RiskFlags struct {
	Effectful        bool
	RequiresApproval bool
	NoStandingGrant  bool
	ExactDisplay     bool
}

func FlagsFor(risk Risk, freshApproval bool) (RiskFlags, bool) {
	switch risk {
	case RiskRead:
		return RiskFlags{}, !freshApproval
	case RiskChange:
		return RiskFlags{Effectful: true, RequiresApproval: freshApproval, NoStandingGrant: freshApproval, ExactDisplay: true}, true
	case RiskTrustGrant, RiskDestructive:
		return RiskFlags{Effectful: true, RequiresApproval: true, NoStandingGrant: true, ExactDisplay: true}, true
	}
	return RiskFlags{}, false
}

func (r Risk) previewHeading() string {
	switch r {
	case RiskTrustGrant:
		return "Grants:"
	case RiskDestructive:
		return "Deletes:"
	default:
		return "Changes:"
	}
}

func (d *Definition) Privileged() bool {
	return d.Risk == RiskTrustGrant || d.Risk == RiskDestructive || d.FreshApproval
}

func (d *Definition) ApplyOperationFlags() error {
	if d == nil {
		return errorf(ErrorCodeDefinitionInvalid, "tool definition must not be nil")
	}
	flags, ok := FlagsFor(d.Risk, d.FreshApproval)
	if !ok {
		return errorf(ErrorCodeDefinitionInvalid, "tool %q has no valid risk for the fresh approval setting", d.Name)
	}
	d.Effectful, d.RequiresApproval, d.NoStandingGrant, d.ExactDisplay = flags.Effectful, flags.RequiresApproval, flags.NoStandingGrant, flags.ExactDisplay
	d.OptIn, d.OwnerOptIn = true, true
	return nil
}

func (d *Definition) ValidateOperation() error {
	if d == nil {
		return errorf(ErrorCodeDefinitionInvalid, "tool definition must not be nil")
	}
	if err := d.validateSinkRelief(); err != nil {
		return err
	}
	if d.Operation == "" {
		return d.validateWithoutOperation()
	}
	flags, ok := FlagsFor(d.Risk, d.FreshApproval)
	if !ok {
		return errorf(ErrorCodeDefinitionInvalid, "tool %q has no valid risk for the fresh approval setting", d.Name)
	}
	var defects []error
	reject := func(format string) {
		defects = append(defects, errorf(ErrorCodeDefinitionInvalid, format, d.Name))
	}
	if d.Effectful != flags.Effectful {
		reject("tool %q does not set Effectful as its risk requires")
	}
	if d.RequiresApproval != flags.RequiresApproval {
		reject("tool %q does not set RequiresApproval as its risk requires")
	}
	if d.NoStandingGrant != flags.NoStandingGrant {
		reject("tool %q does not set NoStandingGrant as its risk requires")
	}
	if d.ExactDisplay != flags.ExactDisplay {
		reject("tool %q does not set ExactDisplay as its risk requires")
	}
	if !d.OptIn || !d.OwnerOptIn {
		reject("tool %q must be opt-in and owner opt-in")
	}
	if d.Orchestration || d.Server != "" || d.Plugin != nil {
		reject("tool %q cannot be an orchestration, server or plugin tool")
	}
	if d.OwnerNotice && (d.Risk != RiskChange || d.FreshApproval || d.ExternalSink) {
		reject("tool %q is an owner notice but is not a plain change that stays off the sinks")
	}
	return errors.Join(defects...)
}

func (d *Definition) validateWithoutOperation() error {
	var defects []error
	if d.Risk != "" {
		defects = append(defects, errorf(ErrorCodeDefinitionInvalid, "tool %q sets a risk without an operation", d.Name))
	}
	if d.FreshApproval {
		defects = append(defects, errorf(ErrorCodeDefinitionInvalid, "tool %q sets fresh approval without an operation", d.Name))
	}
	if d.OwnerNotice {
		defects = append(defects, errorf(ErrorCodeDefinitionInvalid, "tool %q is an owner notice without an operation", d.Name))
	}
	return errors.Join(defects...)
}

func (d *Definition) validateSinkRelief() error {
	relief := d.SinkRelief
	if relief == nil {
		return nil
	}
	reject := func(reason string) error {
		return errorf(ErrorCodeDefinitionInvalid, "tool %q declares a sink relief but %s", d.Name, reason)
	}
	switch {
	case !d.ExternalSink || d.Server != "" || d.Plugin != nil || d.Operation != "":
		return reject("is not a built-in external sink")
	case relief.Argument == "" || !requiredArgument(d.Schema, relief.Argument):
		return reject("names an argument its schema does not require")
	case relief.Kind == SinkReliefBoundedQuery && relief.MaxBytes <= 0:
		return reject("sets no byte bound for a bounded query")
	case relief.Kind == SinkReliefLinkProvenance && relief.MaxBytes != 0:
		return reject("sets a byte bound for link provenance")
	case relief.Kind != SinkReliefBoundedQuery && relief.Kind != SinkReliefLinkProvenance:
		return reject("names an unknown kind")
	}
	return nil
}

func requiredArgument(schema json.RawMessage, name string) bool {
	var shape struct {
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(schema, &shape); err != nil {
		return false
	}
	return slices.Contains(shape.Required, name)
}
