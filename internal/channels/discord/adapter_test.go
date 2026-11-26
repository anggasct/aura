package discord

import (
	"testing"

	"github.com/anggasct/aura/internal/platform/fault"
)

func TestNewRefusesMissingRequiredDeps(t *testing.T) {
	validDeps := func() *Deps {
		cfg := testAdapterConfig()
		return &Deps{
			Config:   &cfg,
			Account:  &cfg.Accounts[0],
			Resumes:  &memoryResumeStore{},
			Effects:  newFakeEffectRunner(),
			Media:    &fakeMediaStore{},
			Sessions: &fakeSessionEnsurer{},
		}
	}

	if _, err := New(nil); !fault.HasCode(err, ErrorCodeInvalidArgument) {
		t.Fatalf("New(nil) = %v, want invalid_argument", err)
	}

	cases := map[string]func(*Deps){
		"nil config":      func(d *Deps) { d.Config = nil },
		"nil account":     func(d *Deps) { d.Account = nil },
		"unnamed account": func(d *Deps) { d.Account.Name = "" },
		"nil resumes":     func(d *Deps) { d.Resumes = nil },
		"nil effects":     func(d *Deps) { d.Effects = nil },
		"nil media":       func(d *Deps) { d.Media = nil },
		"nil sessions":    func(d *Deps) { d.Sessions = nil },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			deps := validDeps()
			mutate(deps)
			if _, err := New(deps); !fault.HasCode(err, ErrorCodeInvalidArgument) {
				t.Errorf("New() error = %v, want invalid_argument", err)
			}
		})
	}
}

func TestAdaptersHealthChecks(t *testing.T) {
	var empty Adapters
	if checks := empty.HealthChecks(); len(checks) != 0 {
		t.Fatalf("empty.HealthChecks() = %+v, want nil", checks)
	}

	cfg := testAdapterConfig()
	adapter, err := New(&Deps{
		Config:   &cfg,
		Account:  &cfg.Accounts[0],
		Resumes:  &memoryResumeStore{},
		Effects:  newFakeEffectRunner(),
		Media:    &fakeMediaStore{},
		Sessions: &fakeSessionEnsurer{},
	})
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	adapters := Adapters{adapter}
	checks := adapters.HealthChecks()
	if len(checks) != 1 || checks[0].ID != "discord" || checks[0].Remediation != RemediationReviewGateway || checks[0].Checker == nil {
		t.Fatalf("HealthChecks() = %+v", checks)
	}
	if findings := checks[0].Checker.Check(t.Context()); len(findings) != 0 {
		t.Fatalf("fresh Check() = %+v, want none", findings)
	}
	adapter.markGap(t.Context())
	if findings := checks[0].Checker.Check(t.Context()); len(findings) != 1 || findings[0].Code != "possible_ingress_gap" {
		t.Fatalf("degraded Check() = %+v, want possible_ingress_gap", findings)
	}
}
