package schedule

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/durable"
	durablesqlite "github.com/anggasct/aura/internal/platform/durable/sqlite"
	"github.com/anggasct/aura/internal/platform/effect"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/operation"
	"github.com/anggasct/aura/internal/platform/operation/operationtest"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/toolbroker"
)

type ownerRoute struct {
	alias string
	route string
}

func (o ownerRoute) OwnerRoute() (alias, route string) { return o.alias, o.route }

type startedRuns struct {
	keys []string
	next Starter
}

func (s *startedRuns) fingerprint() string { return strings.Join(s.keys, ",") }

func (s *startedRuns) Start(ctx context.Context, req durable.StartRequest) (durable.RunRef, error) {
	s.keys = append(s.keys, req.Key)
	if s.next != nil {
		return s.next.Start(ctx, req)
	}
	return durable.RunRef{Key: req.Key}, nil
}

type switchableChecker struct {
	mu   sync.Mutex
	fail func(context.Context, string) error
}

func (c *switchableChecker) set(fail func(context.Context, string) error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fail = fail
}

func (c *switchableChecker) check(ctx context.Context, id string) error {
	c.mu.Lock()
	fail := c.fail
	c.mu.Unlock()
	if fail == nil {
		return nil
	}
	return fail(ctx, id)
}

type scheduleRig struct {
	*operationtest.Rig
	checker *switchableChecker
	db      *sql.DB
	starter *startedRuns
	store   *SQLiteStore
	service *Service
}

func newScheduleRig(t *testing.T, owner OwnerDestination) *scheduleRig {
	t.Helper()
	return newConfiguredScheduleRig(t, &rigOptions{owner: owner})
}

func newWrappedScheduleRig(t *testing.T, owner OwnerDestination, wrap func(*scheduleRig, []operation.Binding)) *scheduleRig {
	t.Helper()
	return newConfiguredScheduleRig(t, &rigOptions{owner: owner, wrap: wrap})
}

type rigOptions struct {
	owner    OwnerDestination
	wrap     func(*scheduleRig, []operation.Binding)
	durable  bool
	webTools WebToolsOffered
}

func newConfiguredScheduleRig(t *testing.T, options *rigOptions) *scheduleRig {
	t.Helper()
	owner, wrap := options.owner, options.wrap
	db := operationtest.OpenDatabase(t, Schema())
	store, err := NewSQLiteStore(db)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	service, err := NewService(store)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	checker := &switchableChecker{}
	starter := &startedRuns{}
	if options.durable {
		runtime, err := durablesqlite.New(durablesqlite.HostOptions(db, slog.New(slog.DiscardHandler), time.Second))
		if err != nil {
			t.Fatalf("durablesqlite.New: %v", err)
		}
		runtime.DeclareHandlers(HandlerName)
		starter.next = runtime
	}
	operations, err := NewOperations(&OperationDeps{
		Config: Default(), Store: store, Service: service, Starter: starter, Owner: owner,
		WorkflowChecker: checker.check, WebTools: options.webTools,
	})
	if err != nil {
		t.Fatalf("NewOperations: %v", err)
	}
	scheduled := &scheduleRig{checker: checker, db: db, starter: starter, store: store, service: service}
	scheduled.Rig = operationtest.NewRig(t, &operationtest.Options{
		DB:       db,
		Catalogs: []operation.Catalog{Catalog()},
		Bindings: func(*operationtest.Stubs) []operation.Binding {
			bindings := operations.Bindings()
			if wrap != nil {
				wrap(scheduled, bindings)
			}
			return bindings
		},
	})
	return scheduled
}

func (r *scheduleRig) addArguments(t *testing.T) json.RawMessage {
	t.Helper()
	return json.RawMessage(`{"name":"nightly","schedule":"0 21 * * *","timezone":"UTC","prompt":"summarize","context":"full"}`)
}

func TestCatalogIsValid(t *testing.T) {
	if err := operation.ValidateCatalogs([]operation.Catalog{Catalog()}); err != nil {
		t.Fatalf("ValidateCatalogs: %v", err)
	}
}

func TestCronAddAgentSchemaNamesNoDestination(t *testing.T) {
	catalog := Catalog()
	var add *operation.Descriptor
	for i := range catalog.Descriptors {
		if catalog.Descriptors[i].ID == "cron.add" {
			add = &catalog.Descriptors[i]
		}
	}
	if add == nil {
		t.Fatal("the catalog has no cron.add")
	}
	if add.Risk != operation.RiskTrustGrant || !add.Sink || !add.Preview || add.Commit != operation.CommitTransactional {
		t.Fatalf("cron.add = risk %s sink %v preview %v commit %s", add.Risk, add.Sink, add.Preview, add.Commit)
	}
	schema, err := add.SchemaFor(operation.SurfaceAgent)
	if err != nil {
		t.Fatalf("SchemaFor: %v", err)
	}
	var shape struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(schema, &shape); err != nil {
		t.Fatalf("decode the schema: %v", err)
	}
	for _, name := range []string{"channel", "destination"} {
		if _, found := shape.Properties[name]; found {
			t.Errorf("the agent schema of cron.add names %q", name)
		}
	}
	for _, name := range []string{"name", "schedule", "prompt", "context", "workflow", "input", "overlap", "grace", "timezone"} {
		if _, found := shape.Properties[name]; !found {
			t.Errorf("the agent schema of cron.add lacks %q", name)
		}
	}
}

func TestAgentCronAddDeliversToOwner(t *testing.T) {
	rig := newScheduleRig(t, ownerRoute{alias: "me", route: "telegram:owner"})
	request := operationtest.RequestFor("aura_cron_add", rig.addArguments(t), "add-1")

	outcome := rig.Execute(t.Context(), request)

	if outcome.Class != toolbroker.ResultOK {
		t.Fatalf("outcome = %+v, want ok", outcome)
	}
	jobs, err := rig.store.ListJobs(t.Context(), 10)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs = %+v, %v; want one", jobs, err)
	}
	if jobs[0].OriginChannel != "telegram" || jobs[0].OriginDestination != "me" {
		t.Errorf("origin = %s/%s, want telegram/me", jobs[0].OriginChannel, jobs[0].OriginDestination)
	}
	if len(rig.starter.keys) != 1 {
		t.Errorf("started runs = %v, want one", rig.starter.keys)
	}
	cards := rig.Prompts()
	if len(cards) != 1 || !strings.Contains(cards[0].Arguments, "telegram:owner") {
		t.Fatalf("approval cards = %+v, want one naming the owner route", cards)
	}
}

func TestCronAddWithoutAnOwnerDestinationIsRefusedDefinitively(t *testing.T) {
	cases := []struct {
		name  string
		owner OwnerDestination
	}{
		{name: "no port", owner: nil},
		{name: "an empty key", owner: ownerRoute{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newScheduleRig(t, tc.owner)

			outcome := rig.Execute(t.Context(), operationtest.RequestFor("aura_cron_add", rig.addArguments(t), "add-1"))

			if outcome.Err == nil || !strings.Contains(outcome.Err.Error(), string(ErrorCodeCapabilityUnavailable)) {
				t.Fatalf("outcome = %+v, want a capability_unavailable refusal", outcome)
			}
			jobs, err := rig.store.ListJobs(t.Context(), 10)
			if err != nil || len(jobs) != 0 {
				t.Fatalf("jobs = %+v, %v; want none", jobs, err)
			}
			if unknown, err := rig.Executor.Journal().ListByState(t.Context(), effect.StateUnknown, 0); err != nil || len(unknown) != 0 {
				t.Fatalf("unknown intents = %d, %v; want none", len(unknown), err)
			}
		})
	}
}

func TestCronAddRefusesAChangedOwnerDestinationAfterThePreview(t *testing.T) {
	current := ownerRoute{alias: "me", route: "telegram:owner"}
	rig := newScheduleRig(t, ownerFunc(func() (string, string) { return current.alias, current.route }))
	preview, err := rig.Registry.Preview(t.Context(), "cron.add", &operation.Call{
		Caller: operation.Caller{Surface: operation.SurfaceChat, PrincipalID: principal.OwnerID, Class: principal.ClassOwner, Direct: true},
		Input:  json.RawMessage(`{"name":"nightly","schedule":"0 21 * * *","timezone":"UTC","prompt":"summarize","context":"full","channel":"telegram","destination":"me"}`),
	})
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	current = ownerRoute{alias: "me", route: "telegram:elsewhere"}

	_, err = rig.Registry.Invoke(t.Context(), "cron.add", &operation.Call{
		Caller:         operation.Caller{Surface: operation.SurfaceChat, PrincipalID: principal.OwnerID, Class: principal.ClassOwner, Direct: true},
		Input:          json.RawMessage(`{"name":"nightly","schedule":"0 21 * * *","timezone":"UTC","prompt":"summarize","context":"full","channel":"telegram","destination":"me"}`),
		IdempotencyKey: "add-1",
		PreviewDigest:  preview.Digest,
	})

	if !fault.HasCode(err, operation.ErrorCodePreviewStale) {
		t.Fatalf("Invoke = %v, want operation_preview_stale", err)
	}
}

type ownerFunc func() (string, string)

func (f ownerFunc) OwnerRoute() (alias, route string) { return f() }

func TestCronOperationsWriteOnlyInsideCommit(t *testing.T) {
	rig := newScheduleRig(t, ownerRoute{alias: "me", route: "telegram:owner"})
	created, err := rig.service.Create(t.Context(), &JobSpec{
		Name: "nightly", CronExpression: "0 21 * * *", Timezone: "UTC", Prompt: "p", Context: ContextFull,
		OriginChannel: "discord", OriginDestination: "default", OverlapPolicy: OverlapSkip,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	byID := `{"id":"` + created.ID + `"}`
	cases := []struct {
		name  string
		id    string
		input string
	}{
		{name: "add", id: "cron.add", input: `{"name":"other","schedule":"0 3 * * *","timezone":"UTC","prompt":"p","context":"full","channel":"discord","destination":"default"}`},
		{name: "pause", id: "cron.pause", input: byID},
		{name: "resume", id: "cron.resume", input: byID},
		{name: "run-now", id: "cron.run-now", input: byID},
		{name: "delete", id: "cron.delete", input: byID},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			operationtest.RequireWritesOnlyInCommit(t, &operationtest.WriteProbe{Registry: rig.Registry, DB: rig.db, ID: tc.id, Input: json.RawMessage(tc.input), Outside: rig.starter.fingerprint})
		})
	}
}

func TestCronReadsReturnRows(t *testing.T) {
	rig := newScheduleRig(t, nil)
	created, err := rig.service.Create(t.Context(), &JobSpec{
		Name: "nightly", CronExpression: "0 21 * * *", Timezone: "UTC", Prompt: "p", Context: ContextFull,
		OriginChannel: "discord", OriginDestination: "default", OverlapPolicy: OverlapSkip,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	owner := operation.Caller{Surface: operation.SurfaceChat, PrincipalID: principal.OwnerID, Class: principal.ClassOwner, Direct: true}

	list, err := rig.Registry.Invoke(t.Context(), "cron.list", &operation.Call{Caller: owner, Input: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatalf("cron.list: %v", err)
	}
	var listed struct {
		Rows []struct {
			ID       string  `json:"id"`
			NextFire *string `json:"next_fire"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(list.Data, &listed); err != nil || len(listed.Rows) != 1 || listed.Rows[0].ID != created.ID || listed.Rows[0].NextFire == nil {
		t.Fatalf("cron.list = %s, %v", list.Data, err)
	}
	if _, err := time.Parse(time.RFC3339, *listed.Rows[0].NextFire); err != nil {
		t.Errorf("next_fire %q is not RFC 3339", *listed.Rows[0].NextFire)
	}

	runs, err := rig.Registry.Invoke(t.Context(), "cron.runs", &operation.Call{Caller: owner, Input: json.RawMessage(`{"id":"` + created.ID + `"}`)})
	if err != nil {
		t.Fatalf("cron.runs: %v", err)
	}
	if strings.TrimSpace(string(runs.Data)) != `{"rows":[]}` {
		t.Errorf("cron.runs = %s, want no rows", runs.Data)
	}
}

func TestWriteGuardSeesARunStartedOutsideCommit(t *testing.T) {
	rig := newWrappedScheduleRig(t, nil, func(scheduled *scheduleRig, bindings []operation.Binding) {
		for i := range bindings {
			if bindings[i].ID != "cron.resume" {
				continue
			}
			handle := bindings[i].Handle
			bindings[i].Handle = func(ctx context.Context, call *operation.Call) (operation.Result, error) {
				if _, err := scheduled.starter.Start(ctx, durable.StartRequest{Key: "early", Handler: HandlerName}); err != nil {
					return operation.Result{}, err
				}
				return handle(ctx, call)
			}
		}
	})
	created, err := rig.service.Create(t.Context(), &JobSpec{
		Name: "nightly", CronExpression: "0 21 * * *", Timezone: "UTC", Prompt: "p", Context: ContextFull,
		OriginChannel: "discord", OriginDestination: "default", OverlapPolicy: OverlapSkip,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	problem := operationtest.WritesOutsideCommit(t, &operationtest.WriteProbe{
		Registry: rig.Registry, DB: rig.db, ID: "cron.resume", Input: json.RawMessage(`{"id":"` + created.ID + `"}`), Outside: rig.starter.fingerprint,
	})

	if problem == "" {
		t.Fatal("a run started before Commit went unnoticed")
	}
}
