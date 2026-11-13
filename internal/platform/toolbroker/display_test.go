package toolbroker

import (
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/tools"
)

func TestExactDisplayRefusesArgumentsThatHoldASecret(t *testing.T) {
	const secret = "s3cr3t-token"
	run := func(body string) (*probeRig, error) {
		definition := tools.Definition{ExactDisplay: true, Display: []string{"body"}}
		rig := newProbeRig(t, &definition, nil, false, secret)
		_, err := rig.broker.Execute(t.Context(), classifiedRequest(probeToolName, `{"body":"`+body+`"}`, principal.ClassTrusted))
		return rig, err
	}
	rig, err := run("posting " + secret)
	if classOf(err) != ResultPolicyDenied || err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("Execute error = %v, want policy_denied without the secret", err)
	}
	want := `tool "probe_effect" arguments hold a configured secret, so the approval card cannot show them`
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("Execute error = %v, want the detail %q", err, want)
	}
	if len(rig.prompts) != 0 {
		t.Fatalf("prompts = %d, want the decider never asked", len(rig.prompts))
	}
	rig, err = run("looks good")
	if err != nil || len(rig.prompts) != 1 {
		t.Fatalf("Execute = %v with %d prompts, want one prompt", err, len(rig.prompts))
	}
}
