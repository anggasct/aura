package telegram

import (
	"testing"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/secret/secrettest"
)

func TestTelegramNewRefusesMissingRequiredDeps(t *testing.T) {
	h := newHarness(t)
	validDeps := func() *Deps {
		cfg := Default()
		return &Deps{
			Config:   &cfg,
			Account:  &Account{Name: "pocket", BotTokenRef: secrettest.Ref("env://" + testTokenEnv)},
			Cursors:  h.cursors,
			Effects:  h.effects,
			Sessions: h.sessions,
		}
	}
	cases := map[string]func() *Deps{
		"nil deps":         func() *Deps { return nil },
		"nil config":       func() *Deps { d := validDeps(); d.Config = nil; return d },
		"nil account":      func() *Deps { d := validDeps(); d.Account = nil; return d },
		"unnamed account":  func() *Deps { d := validDeps(); d.Account.Name = ""; return d },
		"nil cursors":      func() *Deps { d := validDeps(); d.Cursors = nil; return d },
		"nil effects":      func() *Deps { d := validDeps(); d.Effects = nil; return d },
		"nil sessions":     func() *Deps { d := validDeps(); d.Sessions = nil; return d },
		"missing tokenref": func() *Deps { d := validDeps(); d.Account.BotTokenRef = secrettest.Ref(""); return d },
	}
	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			adapter, err := New(build())
			if adapter != nil || !fault.HasCode(err, ErrorCodeInvalidArgument) {
				t.Fatalf("New() = %v, %v; want nil adapter and invalid_argument", adapter, err)
			}
		})
	}
}

func TestAdaptersHealthChecks(t *testing.T) {
	if checks := Adapters(nil).HealthChecks(); len(checks) != 0 {
		t.Fatalf("empty Adapters.HealthChecks() = %+v, want empty", checks)
	}
	h := newHarness(t)
	adapters := Adapters{h.adapter(t, nil)}
	checks := adapters.HealthChecks()
	if len(checks) != 1 {
		t.Fatalf("HealthChecks() len = %d, want 1", len(checks))
	}
	if checks[0].ID != "telegram" || checks[0].Remediation != RemediationReviewBot || checks[0].Checker == nil {
		t.Fatalf("HealthChecks()[0] = %+v", checks[0])
	}
	if findings := checks[0].Checker.Check(t.Context()); len(findings) != 0 {
		t.Fatalf("Check() = %+v, want no findings before fault", findings)
	}
}
