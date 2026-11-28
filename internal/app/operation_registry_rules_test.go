package app

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/modules/knowledge"
	"github.com/anggasct/aura/internal/modules/knowledge/knowledgetest"
	"github.com/anggasct/aura/internal/modules/memory"
	"github.com/anggasct/aura/internal/modules/memory/memorytest"
	"github.com/anggasct/aura/internal/modules/profile"
	"github.com/anggasct/aura/internal/modules/profile/profiletest"
	"github.com/anggasct/aura/internal/modules/schedule"
	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/operation"
	"github.com/anggasct/aura/internal/platform/operation/operationtest"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/tools"
	"github.com/anggasct/aura/internal/platform/turn"
	"github.com/anggasct/aura/internal/platform/workspace"
	workspaceops "github.com/anggasct/aura/internal/platform/workspace/ops"
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

type handlerCounts struct {
	mu   sync.Mutex
	runs map[string]int
}

func (c *handlerCounts) count(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.runs == nil {
		c.runs = map[string]int{}
	}
	c.runs[id]++
}

func (c *handlerCounts) handled(id string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.runs[id]
}

type registryRig struct {
	*operationtest.Rig
	access   *ceilingAccess
	kb       *knowledgetest.Rig
	starts   *recordedStarts
	handlers *handlerCounts
	calls    int
}

func newRegistryRig(t *testing.T, answer func(context.Context, *toolbroker.ApprovalPrompt) (bool, error)) *registryRig {
	t.Helper()
	db := operationtest.OpenDatabase(t, schedule.Schema(), knowledge.Schema(), memory.Schema(), profile.Schema(), workspace.Schema())
	kb := knowledgetest.NewRig(t, db)
	seedKnowledgeSource := func(ctx context.Context) error {
		prepared, err := kb.Service.PrepareURL(ctx, knowledgetest.Collection, knowledgetest.PageURL)
		if err != nil {
			return err
		}
		_, err = kb.Service.Write(ctx, knowledge.ActionAdd, prepared)
		return err
	}
	if err := seedKnowledgeSource(t.Context()); err != nil {
		t.Fatalf("seed the knowledge source: %v", err)
	}
	knowledgeOperations, err := knowledge.NewOperations(&knowledge.OperationDeps{Service: kb.Service})
	if err != nil {
		t.Fatalf("knowledge.NewOperations: %v", err)
	}
	mem := memorytest.NewRig(t, db)
	seedMemory := mem.Seed
	if err := seedMemory(t.Context()); err != nil {
		t.Fatalf("seed the memory documents: %v", err)
	}
	memoryOperations, err := memory.NewOperations(&memory.OperationDeps{Service: mem.Service, Scopes: memorytest.PersonaScopes()})
	if err != nil {
		t.Fatalf("memory.NewOperations: %v", err)
	}
	prof := profiletest.NewRig(t, db)
	seedProfile := func(ctx context.Context) error {
		if err := prof.SeedVerified(ctx); err != nil {
			return err
		}
		return prof.SeedCandidate(ctx)
	}
	if err := seedProfile(t.Context()); err != nil {
		t.Fatalf("seed the profile facts: %v", err)
	}
	profileOperations, err := profile.NewOperations(&profile.OperationDeps{Service: prof.Service})
	if err != nil {
		t.Fatalf("profile.NewOperations: %v", err)
	}
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
	handlers := &handlerCounts{}
	rig := operationtest.NewRig(t, &operationtest.Options{
		DB:       db,
		Catalogs: operationCatalogs(),
		Bindings: func(*operationtest.Stubs) []operation.Binding {
			workspaceOperations := workspaceops.NewOperations(workspace.NewTaintStore(db))
			bindings := slices.Concat(operations.Bindings(), knowledgeOperations.Bindings(), memoryOperations.Bindings(), profileOperations.Bindings(), workspaceOperations.Bindings())
			seedEmbedded := func(ctx context.Context) error {
				_, err := kb.Service.Embed(ctx, knowledgetest.Collection)
				return err
			}
			reseeds := map[string]func(context.Context) error{
				"kb.remove":      seedKnowledgeSource,
				"kb.unembed":     seedEmbedded,
				"memory.forget":  seedMemory,
				"memory.purge":   seedMemory,
				"profile.delete": seedProfile,
			}
			for i := range bindings {
				id, handle, preview := bindings[i].ID, bindings[i].Handle, bindings[i].Preview
				reseed := reseeds[id]
				bindings[i].Handle = func(ctx context.Context, call *operation.Call) (operation.Result, error) {
					handlers.count(id)
					if reseed != nil {
						if err := reseed(ctx); err != nil {
							return operation.Result{}, err
						}
					}
					return handle(ctx, call)
				}
				if preview != nil && reseed != nil {
					bindings[i].Preview = func(ctx context.Context, call *operation.Call) (operation.Preview, error) {
						if err := reseed(ctx); err != nil {
							return operation.Preview{}, err
						}
						return preview(ctx, call)
					}
				}
			}
			return bindings
		},
		Access:      access,
		AnswerCards: answer,

		WithDiscovery: true,
		Behaviors:     new(memory.Module).Behaviors(),
	})
	registrations := rig.Registry.Tools()
	for i := range registrations {
		access.tools = append(access.tools, registrations[i].Definition.Name)
	}
	return &registryRig{Rig: rig, access: access, kb: kb, starts: starts, handlers: handlers}
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
	"aura_cron_add":     `{"name":"nightly","schedule":"0 21 * * *","timezone":"UTC","prompt":"summarize","context":"full"}`,
	"aura_cron_delete":  `{"id":"job-1"}`,
	"aura_cron_list":    `{}`,
	"aura_cron_pause":   `{"id":"job-1"}`,
	"aura_cron_resume":  `{"id":"job-1"}`,
	"aura_cron_run_now": `{"id":"job-1"}`,
	"aura_cron_runs":    `{"id":"job-1"}`,
	"aura_kb_add":       `{"collection":"support","source":"https://docs.example.test/refunds"}`,
	"aura_kb_check":     `{}`,
	"aura_kb_embed":     `{"collection":"support"}`,
	"aura_kb_list":      `{}`,
	"aura_kb_refresh":   `{"collection":"support"}`,
	"aura_kb_remove":    `{"collection":"support","source":"https://docs.example.test/refunds"}`,
	"aura_kb_show":      `{"collection":"support"}`,
	"aura_kb_unembed":   `{"collection":"support"}`,

	"aura_memory_forget":  `{"id":"mem-a1"}`,
	"aura_memory_inspect": `{}`,
	"aura_memory_purge":   `{}`,

	"aura_profile_delete": `{"id":"` + profileSampleFactID + `"}`,
	"aura_profile_list":   `{}`,
	"aura_profile_reject": `{"id":"` + profileSampleFactID + `"}`,
	"aura_profile_review": `{}`,
	"aura_profile_set":    `{"category":"tool","key":"shell","value":"zsh"}`,
	"aura_profile_show":   `{"id":"` + profileSampleFactID + `"}`,
}

var profileSampleFactID = profiletest.VerifiedFactID()

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

func TestEveryPrivilegedOperationAlwaysAsksTheOwner(t *testing.T) {
	var privileged int
	probe := newRegistryRig(t, nil)
	registrations := registeredTools(t, probe)
	for i := range registrations {
		definition := &registrations[i].Definition
		if !definition.Privileged() {
			continue
		}
		privileged++
		name := definition.Name
		input := probe.sampleInput(t, name)

		if definition.Risk == tools.RiskTrustGrant {
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
		} else {
			t.Run(name+" still asks on a tainted turn", func(t *testing.T) {
				rig := newRegistryRig(t, nil)
				request := rig.request(name, input)
				request.Tainted = true

				got := rig.Execute(t.Context(), request)

				if got.Class == toolbroker.ResultUntrustedInputTrustGrant {
					t.Fatalf("outcome = %+v, a call that is not a trust grant is refused as one", got)
				}
				if prompts := rig.Prompts(); len(prompts) != 1 {
					t.Fatalf("approval cards = %d, want exactly one before anything runs", len(prompts))
				}
			})
		}
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
	if privileged == 0 {
		t.Fatal("the registry holds no privileged operation to check")
	}
}
