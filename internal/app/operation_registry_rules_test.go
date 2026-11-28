package app

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/modules/schedule"
	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/operation"
	"github.com/anggasct/aura/internal/platform/operation/operationtest"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/tools"
	"github.com/anggasct/aura/internal/platform/turn"
)

const rulesAgentID = "team"

type ceilingAccess struct {
	tools   []string
	lookups int
}

func (a *ceilingAccess) ToolCeiling(agentID string, _ principal.Class) ([]string, bool) {
	if agentID != rulesAgentID {
		return nil, false
	}
	return a.tools, true
}

func (a *ceilingAccess) StandingGrant(context.Context, string, string, string, string) (bool, error) {
	a.lookups++
	return true, nil
}

type recordedStarts struct {
	keys []string
}

func (s *recordedStarts) Start(_ context.Context, req durable.StartRequest) (durable.RunRef, error) {
	s.keys = append(s.keys, req.Key)
	return durable.RunRef{Key: req.Key}, nil
}

type fixedOwner struct{}

func (fixedOwner) OwnerRoute() (alias, route string) { return "me", "telegram:owner" }

type registryRig struct {
	*operationtest.Rig
	access *ceilingAccess
	starts *recordedStarts
	calls  int
}

func newRegistryRig(t *testing.T, answer func(context.Context, *toolbroker.ApprovalPrompt) (bool, error)) *registryRig {
	t.Helper()
	db := operationtest.OpenDatabase(t, schedule.Schema())
	store, err := schedule.NewSQLiteStore(db)
	if err != nil {
		t.Fatalf("schedule.NewSQLiteStore: %v", err)
	}
	service, err := schedule.NewService(store)
	if err != nil {
		t.Fatalf("schedule.NewService: %v", err)
	}
	starts := &recordedStarts{}
	operations, err := schedule.NewOperations(&schedule.OperationDeps{
		Config: schedule.Default(), Store: store, Service: service, Starter: starts, Owner: fixedOwner{},
		WorkflowChecker: func(context.Context, string) error { return nil },
	})
	if err != nil {
		t.Fatalf("schedule.NewOperations: %v", err)
	}
	access := &ceilingAccess{}
	rig := operationtest.NewRig(t, &operationtest.Options{
		DB:          db,
		Catalogs:    operationCatalogs(),
		Bindings:    func(*operationtest.Stubs) []operation.Binding { return operations.Bindings() },
		Access:      access,
		AnswerCards: answer,
	})
	registrations := rig.Registry.Tools()
	for i := range registrations {
		access.tools = append(access.tools, registrations[i].Definition.Name)
	}
	return &registryRig{Rig: rig, access: access, starts: starts}
}

func (r *registryRig) request(name string, input json.RawMessage) *toolbroker.ToolRequest {
	r.calls++
	request := operationtest.RequestFor(name, input, "rules-"+strconv.Itoa(r.calls))
	request.RequestID = "call-" + strconv.Itoa(r.calls)
	return request
}

func (r *registryRig) requireNothingHappened(t *testing.T, before string) {
	t.Helper()
	if after := operationtest.Snapshot(t, r.DB); after != before {
		t.Errorf("the refused call changed the database")
	}
	if prompts := r.Prompts(); len(prompts) != 0 {
		t.Errorf("the refused call raised %d approval cards", len(prompts))
	}
	if len(r.starts.keys) != 0 {
		t.Errorf("the refused call started runs %v", r.starts.keys)
	}
	if r.access.lookups != 0 {
		t.Errorf("the refused call looked up %d standing grants", r.access.lookups)
	}
}

var registrySampleInput = map[string]string{
	"aura_cron_add":     `{"name":"nightly","schedule":"0 21 * * *","timezone":"UTC","prompt":"summarize"}`,
	"aura_cron_delete":  `{"id":"job-1"}`,
	"aura_cron_list":    `{}`,
	"aura_cron_pause":   `{"id":"job-1"}`,
	"aura_cron_resume":  `{"id":"job-1"}`,
	"aura_cron_run_now": `{"id":"job-1"}`,
	"aura_cron_runs":    `{"id":"job-1"}`,
}

func (r *registryRig) sampleInput(t *testing.T, name string) json.RawMessage {
	t.Helper()
	input, ok := registrySampleInput[name]
	if !ok {
		t.Fatalf("%s needs an entry in registrySampleInput", name)
	}
	return json.RawMessage(input)
}

func registeredTools(t *testing.T, rig *registryRig) []toolbroker.ToolRegistration {
	t.Helper()
	registrations := rig.Registry.Tools()
	if len(registrations) == 0 {
		t.Fatal("the operation registry holds no tools")
	}
	return registrations
}

func TestEveryOperationToolIsOwnerOnly(t *testing.T) {
	denied := []struct {
		name   string
		mutate func(request *toolbroker.ToolRequest)
	}{
		{name: "trusted class", mutate: func(r *toolbroker.ToolRequest) { r.PrincipalClass, r.AgentID = principal.ClassTrusted, rulesAgentID }},
		{name: "public class", mutate: func(r *toolbroker.ToolRequest) { r.PrincipalClass, r.AgentID = principal.ClassPublic, rulesAgentID }},
		{name: "webhook principal", mutate: func(r *toolbroker.ToolRequest) {
			r.PrincipalClass, r.PrincipalID, r.AgentID, r.Origin = principal.ClassTrusted, "webhook:billing", rulesAgentID, turn.OriginWebhook
		}},
		{name: "child origin", mutate: func(r *toolbroker.ToolRequest) { r.Origin = turn.OriginChild }},
		{name: "workflow origin", mutate: func(r *toolbroker.ToolRequest) { r.Origin = turn.OriginWorkflow }},
		{name: "unset origin", mutate: func(r *toolbroker.ToolRequest) { r.Origin = "" }},
	}
	rig := newRegistryRig(t, nil)
	before := operationtest.Snapshot(t, rig.DB)
	registrations := registeredTools(t, rig)
	for i := range registrations {
		name := registrations[i].Definition.Name
		for _, tc := range denied {
			t.Run(name+" for a "+tc.name, func(t *testing.T) {
				request := rig.request(name, rig.sampleInput(t, name))
				tc.mutate(request)

				got := rig.Execute(t.Context(), request)

				if got.Class != toolbroker.ResultToolTierDenied {
					t.Errorf("outcome = %+v, want %s", got, toolbroker.ResultToolTierDenied)
				}
			})
		}
	}
	rig.requireNothingHappened(t, before)
}

func TestEveryTrustGrantOperationAlwaysAsksTheOwner(t *testing.T) {
	var trustGrants int
	probe := newRegistryRig(t, nil)
	registrations := registeredTools(t, probe)
	for i := range registrations {
		definition := &registrations[i].Definition
		if definition.Risk != tools.RiskTrustGrant {
			continue
		}
		trustGrants++
		name := definition.Name
		input := probe.sampleInput(t, name)

		t.Run(name+" is refused on a tainted turn without a card", func(t *testing.T) {
			rig := newRegistryRig(t, nil)
			before := operationtest.Snapshot(t, rig.DB)
			request := rig.request(name, input)
			request.Tainted = true

			got := rig.Execute(t.Context(), request)

			if got.Class != toolbroker.ResultUntrustedInputTrustGrant {
				t.Fatalf("outcome = %+v, want %s", got, toolbroker.ResultUntrustedInputTrustGrant)
			}
			rig.requireNothingHappened(t, before)
		})
		for _, tracked := range []bool{false, true} {
			t.Run(name+" asks on every call tracked="+strconv.FormatBool(tracked), func(t *testing.T) {
				rig := newRegistryRig(t, nil)
				for range 2 {
					request := rig.request(name, input)
					request.ContentTaintTracked = tracked
					if got := rig.Execute(t.Context(), request); got.Class != toolbroker.ResultOK {
						t.Fatalf("outcome = %+v, want ok", got)
					}
				}
				prompts := rig.Prompts()
				if len(prompts) != 2 {
					t.Fatalf("approval cards = %d, want one per call", len(prompts))
				}
				for _, prompt := range prompts {
					if prompt.StandingGrantAllowed {
						t.Error("the card offers a standing grant")
					}
				}
				if rig.access.lookups != 0 {
					t.Errorf("standing grant lookups = %d, want none", rig.access.lookups)
				}
			})
		}
		t.Run(name+" refuses an attached grant", func(t *testing.T) {
			rig := newRegistryRig(t, nil)
			before := operationtest.Snapshot(t, rig.DB)
			request := rig.request(name, input)
			grant, err := rig.Broker.Grant(t.Context(), request, time.Minute)
			if err != nil {
				t.Fatalf("Grant: %v", err)
			}
			request.Approval = &grant

			got := rig.Execute(t.Context(), request)

			if got.Class != toolbroker.ResultToolTierDenied {
				t.Fatalf("outcome = %+v, want %s", got, toolbroker.ResultToolTierDenied)
			}
			rig.requireNothingHappened(t, before)
		})
		t.Run(name+" does nothing when the owner rejects the card", func(t *testing.T) {
			rig := newRegistryRig(t, func(context.Context, *toolbroker.ApprovalPrompt) (bool, error) { return false, nil })

			got := rig.Execute(t.Context(), rig.request(name, input))

			if got.Class != toolbroker.ResultApprovalRequired {
				t.Fatalf("outcome = %+v, want %s", got, toolbroker.ResultApprovalRequired)
			}
			if len(rig.starts.keys) != 0 {
				t.Errorf("a rejected card started runs %v", rig.starts.keys)
			}
		})
	}
	if trustGrants == 0 {
		t.Fatal("the registry holds no trust-grant operation to check")
	}
}
