package httpchat

import (
	"testing"
	"time"

	"github.com/anggasct/aura/internal/channels/kit/access"
	"github.com/anggasct/aura/internal/platform/fault"
)

func TestNewAndHealthChecks(t *testing.T) {
	base := func() *Deps {
		cfg := Default()
		gates := &gateLog{}
		return &Deps{
			Config:   &cfg,
			Keys:     &memoryKeys{},
			Sessions: &memorySessions{},
			Replies:  &fakeReplySource{},
			Personas: staticPersonas{},
			Runtime:  &scriptedRuntime{},
			Access: access.Settings{
				Budgets:  &fakeBudgets{log: gates},
				Evals:    &fakeEvals{log: gates},
				Handoffs: &fakeHandoffs{log: gates},
			},
			AnswerTexts:   testAnswerTexts,
			ShutdownGrace: time.Second,
			Logger:        slogDiscard(),
		}
	}
	cases := map[string]func(*Deps) *Deps{
		"nil deps":       func(*Deps) *Deps { return nil },
		"nil config":     func(d *Deps) *Deps { d.Config = nil; return d },
		"nil keys":       func(d *Deps) *Deps { d.Keys = nil; return d },
		"nil sessions":   func(d *Deps) *Deps { d.Sessions = nil; return d },
		"nil replies":    func(d *Deps) *Deps { d.Replies = nil; return d },
		"nil runtime":    func(d *Deps) *Deps { d.Runtime = nil; return d },
		"zero grace":     func(d *Deps) *Deps { d.ShutdownGrace = 0; return d },
		"nil logger":     func(d *Deps) *Deps { d.Logger = nil; return d },
		"nil handoff":    func(d *Deps) *Deps { d.Access.Handoffs = nil; return d },
		"empty listener": func(d *Deps) *Deps { d.Config.ListenAddress = ""; return d },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			srv, err := New(mutate(base()))
			if !fault.HasCode(err, ErrorCodeInvalidArgument) || srv != nil {
				t.Fatalf("New = %v, %v; want %s", srv, err, ErrorCodeInvalidArgument)
			}
		})
	}
	var nilServer *Server
	if checks := nilServer.HealthChecks(); len(checks) != 0 {
		t.Fatalf("nilServer.HealthChecks() = %+v, want nil", checks)
	}
	srv, err := New(base())
	if err != nil {
		t.Fatalf("New(valid) = %v", err)
	}
	checks := srv.HealthChecks()
	if len(checks) != 1 || checks[0].ID != "http" || checks[0].Checker != srv || checks[0].Remediation != RemediationReviewListener {
		t.Fatalf("HealthChecks() = %+v", checks)
	}
}
