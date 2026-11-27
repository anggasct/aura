package tools

import (
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/platform/fault"
)

func TestRiskMapsOntoDefinitionFlags(t *testing.T) {
	cases := []struct {
		name  string
		risk  Risk
		fresh bool
		want  RiskFlags
		alone bool
	}{
		{name: "read", risk: RiskRead, want: RiskFlags{}},
		{name: "change", risk: RiskChange, want: RiskFlags{Effectful: true, ExactDisplay: true}, alone: true},
		{name: "change with fresh approval", risk: RiskChange, fresh: true, want: RiskFlags{Effectful: true, RequiresApproval: true, NoStandingGrant: true, ExactDisplay: true}, alone: true},
		{name: "trust grant", risk: RiskTrustGrant, want: RiskFlags{Effectful: true, RequiresApproval: true, NoStandingGrant: true, ExactDisplay: true}, alone: true},
		{name: "destructive", risk: RiskDestructive, want: RiskFlags{Effectful: true, RequiresApproval: true, NoStandingGrant: true, ExactDisplay: true}, alone: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			definition := Definition{Name: "aura_probe", Version: Version, Risk: tc.risk, FreshApproval: tc.fresh, Operation: "probe.run"}
			if err := definition.ApplyOperationFlags(); err != nil {
				t.Fatalf("ApplyOperationFlags: %v", err)
			}
			got := RiskFlags{Effectful: definition.Effectful, RequiresApproval: definition.RequiresApproval, NoStandingGrant: definition.NoStandingGrant, ExactDisplay: definition.ExactDisplay}
			if got != tc.want {
				t.Errorf("flags = %+v, want %+v", got, tc.want)
			}
			if !definition.OptIn || !definition.OwnerOptIn {
				t.Error("a definition with an operation must be opt-in and owner opt-in")
			}
			if definition.RunsAlone() != tc.alone {
				t.Errorf("RunsAlone = %t, want %t", definition.RunsAlone(), tc.alone)
			}
			if definition.StandingGrantAllowed() {
				t.Error("a standing grant is allowed for an operation")
			}
			if err := definition.ValidateOperation(); err != nil {
				t.Errorf("ValidateOperation rejected the generated definition: %v", err)
			}
			if definition.Privileged() != (tc.risk == RiskTrustGrant || tc.risk == RiskDestructive || tc.fresh) {
				t.Errorf("Privileged = %t for %s", definition.Privileged(), tc.name)
			}
		})
	}
}

func TestFlagsForRefusesWhatHasNoMapping(t *testing.T) {
	cases := []struct {
		name  string
		risk  Risk
		fresh bool
	}{
		{name: "an empty risk", risk: ""},
		{name: "an unknown risk", risk: "sometimes"},
		{name: "a read with fresh approval", risk: RiskRead, fresh: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := FlagsFor(tc.risk, tc.fresh); ok {
				t.Error("FlagsFor mapped a risk it should refuse")
			}
			definition := Definition{Name: "aura_probe", Operation: "probe.run", Risk: tc.risk, FreshApproval: tc.fresh}
			if err := definition.ApplyOperationFlags(); !fault.HasCode(err, ErrorCodeDefinitionInvalid) {
				t.Errorf("ApplyOperationFlags = %v, want %s", err, ErrorCodeDefinitionInvalid)
			}
		})
	}
}

func TestDefinitionOperationValidationJoinsEveryDefect(t *testing.T) {
	definition := Definition{Name: "aura_probe", Version: Version, Operation: "probe.run", Risk: RiskDestructive}
	err := definition.ValidateOperation()
	if !fault.HasCode(err, ErrorCodeDefinitionInvalid) {
		t.Fatalf("ValidateOperation = %v, want %s", err, ErrorCodeDefinitionInvalid)
	}
	for _, want := range []string{"Effectful", "RequiresApproval", "NoStandingGrant", "ExactDisplay", "opt-in"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("ValidateOperation = %v, want it to name %s", err, want)
		}
	}
}

func TestLegacyDefinitionsKeepTheirShape(t *testing.T) {
	definition := Definition{Name: "exec", Version: Version, Effectful: true, RequiresApproval: true}
	if err := definition.ValidateOperation(); err != nil {
		t.Fatalf("ValidateOperation rejected a definition without an operation: %v", err)
	}
	if !definition.StandingGrantAllowed() {
		t.Error("a definition without an operation lost its standing grant eligibility")
	}
	definition.UntrustedOutput = true
	if err := definition.ValidateOperation(); err != nil {
		t.Errorf("ValidateOperation rejected an untrusted output flag on a module tool: %v", err)
	}
	var missing *Definition
	if err := missing.ValidateOperation(); !fault.HasCode(err, ErrorCodeDefinitionInvalid) {
		t.Errorf("ValidateOperation on nil = %v, want %s", err, ErrorCodeDefinitionInvalid)
	}
	if err := missing.ApplyOperationFlags(); !fault.HasCode(err, ErrorCodeDefinitionInvalid) {
		t.Errorf("ApplyOperationFlags on nil = %v, want %s", err, ErrorCodeDefinitionInvalid)
	}
}

func TestPreviewApprovalTextAppendsEscapedLinesUnderTheRiskHeading(t *testing.T) {
	definition := Definition{Name: "aura_probe", Version: Version, Operation: "probe.run", Risk: RiskDestructive}
	if err := definition.ApplyOperationFlags(); err != nil {
		t.Fatalf("ApplyOperationFlags: %v", err)
	}
	arguments := []byte(`{"target":"x"}`)
	got, err := definition.PreviewApprovalText(arguments, []string{"a\nb", "tail"}, nil)
	if err != nil {
		t.Fatalf("PreviewApprovalText: %v", err)
	}
	if want := "target: x\nDeletes:\na\\nb\ntail"; got != want {
		t.Errorf("card text = %q, want %q", got, want)
	}
	if _, err := definition.PreviewApprovalText(arguments, []string{strings.Repeat("x", ApprovalDisplayLimit)}, nil); !fault.HasCode(err, ErrorCodeDisplayTooLarge) {
		t.Errorf("PreviewApprovalText over the limit = %v, want %s", err, ErrorCodeDisplayTooLarge)
	}
	redact := func(text string) string { return strings.ReplaceAll(text, "secret", "[redacted]") }
	if _, err := definition.PreviewApprovalText(arguments, []string{"has a secret"}, redact); !fault.HasCode(err, ErrorCodeDisplayRedacted) {
		t.Errorf("PreviewApprovalText with a secret = %v, want %s", err, ErrorCodeDisplayRedacted)
	}
}
