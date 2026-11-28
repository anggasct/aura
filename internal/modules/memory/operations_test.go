package memory_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/modules/memory"
	"github.com/anggasct/aura/internal/modules/memory/memorytest"
	"github.com/anggasct/aura/internal/platform/clikit"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/operation"
	"github.com/anggasct/aura/internal/platform/operation/operationtest"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/secret"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/tools"
	"github.com/anggasct/aura/internal/platform/turn"
	turnstore "github.com/anggasct/aura/internal/platform/turn/store"
)

const defaultPersona = memorytest.MainPersona

type opsRig struct {
	*operationtest.Rig
	mem   *memorytest.Rig
	calls int
}

func newOpsRig(t *testing.T) *opsRig {
	t.Helper()
	return newOpsRigAnswering(t, nil)
}

func newOpsRigAnswering(t *testing.T, answer func(ctx context.Context, prompt *toolbroker.ApprovalPrompt) (bool, error)) *opsRig {
	t.Helper()
	db := operationtest.OpenDatabase(t, memory.Schema())
	mem := memorytest.NewRig(t, db)
	if err := mem.Seed(t.Context()); err != nil {
		t.Fatalf("seed the documents: %v", err)
	}
	operations, err := memory.NewOperations(&memory.OperationDeps{
		Service: mem.Service,
		Scopes:  memorytest.PersonaScopes(),
		Now:     func() time.Time { return time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("NewOperations: %v", err)
	}
	rig := operationtest.NewRig(t, &operationtest.Options{
		DB:          db,
		Catalogs:    []operation.Catalog{memory.Catalog()},
		Bindings:    func(*operationtest.Stubs) []operation.Binding { return operations.Bindings() },
		AnswerCards: answer,
	})
	return &opsRig{Rig: rig, mem: mem}
}

func (r *opsRig) key(label string) string {
	r.calls++
	return "ops-" + label + "-" + strconv.Itoa(r.calls)
}

func (r *opsRig) tool(id, input string) operationtest.Outcome {
	return r.toolAs(defaultPersona, id, input)
}

func (r *opsRig) toolAs(persona, id, input string) operationtest.Outcome {
	request := operationtest.RequestFor(operation.ToolName(id), json.RawMessage(input), r.key(id))
	request.AgentID = persona
	return r.Execute(context.Background(), request)
}

func (r *opsRig) call(surface operation.Surface, id, input string) *operation.Call {
	call := &operation.Call{
		Caller: operation.Caller{Surface: surface, PrincipalID: operationtest.OwnerID, Class: principal.ClassOwner, SessionID: operationtest.SessionID, Direct: true},
		Input:  json.RawMessage(input),
	}
	if surface != operation.SurfaceCLI {
		call.IdempotencyKey = r.key(id)
		call.RequestDigest = operation.Digest(call.Input)
	}
	return call
}

func (r *opsRig) invoke(t *testing.T, surface operation.Surface, id, input string) (operation.Result, error) {
	t.Helper()
	return r.Registry.Invoke(t.Context(), id, r.call(surface, id, input))
}

func (r *opsRig) apply(t *testing.T, surface operation.Surface, id, input string) (operation.Result, error) {
	t.Helper()
	return r.InvokePreviewed(t, id, r.call(surface, id, input))
}

func (r *opsRig) preview(t *testing.T, surface operation.Surface, id, input string) operation.Preview {
	t.Helper()
	preview, err := r.Registry.Preview(t.Context(), id, r.call(surface, id, input))
	if err != nil {
		t.Fatalf("preview of %s: %v", id, err)
	}
	return preview
}

func (r *opsRig) documentIDs(t *testing.T) []string {
	t.Helper()
	rows, err := r.DB.QueryContext(t.Context(), `SELECT id FROM memory_document ORDER BY id`)
	if err != nil {
		t.Fatalf("read the documents: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the documents: %v", err)
	}
	return ids
}

func requireOK(t *testing.T, outcome operationtest.Outcome) {
	t.Helper()
	if outcome.Class != toolbroker.ResultOK {
		t.Fatalf("outcome = %+v, want ok", outcome)
	}
}

func rowIDs(t *testing.T, output string) []string {
	t.Helper()
	var ids []string
	for _, row := range resultRows(t, json.RawMessage(output)) {
		ids = append(ids, fmt.Sprint(row["id"]))
	}
	return ids
}

func resultRows(t *testing.T, data json.RawMessage) []map[string]any {
	t.Helper()
	var decoded struct {
		Rows []map[string]any `json:"rows"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("decode %s: %v", data, err)
	}
	return decoded.Rows
}

func TestCatalogIsValid(t *testing.T) {
	if err := operation.ValidateCatalogs([]operation.Catalog{memory.Catalog()}); err != nil {
		t.Fatalf("ValidateCatalogs: %v", err)
	}
}

func TestMemoryOperationsParity(t *testing.T) {
	steps := []struct {
		name string
		id   string
		in   string
		want []string
	}{
		{name: "inspect a session", id: "memory.inspect", in: `{"session":"` + memorytest.FirstSession + `"}`, want: []string{"mem-a1", "mem-a2", "mem-b1", memorytest.ExpiredSummary}},
		{name: "forget one document", id: "memory.forget", in: `{"id":"mem-a1"}`, want: []string{"mem-a2", "mem-b1", memorytest.ExpiredSummary}},
		{name: "forget by query", id: "memory.forget", in: `{"query":"beta"}`, want: []string{"mem-a2", memorytest.ExpiredSummary}},
		{name: "purge expired summaries", id: "memory.purge", in: `{}`, want: []string{"mem-a2"}},
		{name: "forget a session", id: "memory.forget", in: `{"session":"` + memorytest.FirstSession + `"}`, want: nil},
	}
	agent, cli, chat := newOpsRig(t), newOpsRig(t), newOpsRig(t)
	for _, step := range steps {
		fromTool := agent.tool(step.id, step.in)
		requireOK(t, fromTool)
		fromCLI, err := cli.apply(t, operation.SurfaceCLI, step.id, step.in)
		if err != nil {
			t.Fatalf("%s on the CLI surface: %v", step.name, err)
		}
		fromChat, err := chat.apply(t, operation.SurfaceChat, step.id, step.in)
		if err != nil {
			t.Fatalf("%s in chat: %v", step.name, err)
		}
		if fromTool.Output != string(fromCLI.Data) || string(fromCLI.Data) != string(fromChat.Data) {
			t.Fatalf("%s returned different data\ntool: %s\ncli: %s\nchat: %s", step.name, fromTool.Output, fromCLI.Data, fromChat.Data)
		}
		for name, rig := range map[string]*opsRig{"tool": agent, "cli": cli, "chat": chat} {
			if got := strings.Join(rig.documentIDs(t), ","); got != strings.Join(step.want, ",") {
				t.Fatalf("after %s the %s left %q, want %q", step.name, name, got, strings.Join(step.want, ","))
			}
		}
	}
}

func TestMemoryForgetBindsItsPreview(t *testing.T) {
	const input = `{"session":"` + memorytest.FirstSession + `"}`
	t.Run("on the owner surfaces", func(t *testing.T) {
		for _, surface := range []operation.Surface{operation.SurfaceCLI, operation.SurfaceChat} {
			t.Run(string(surface), func(t *testing.T) {
				rig := newOpsRig(t)
				preview := rig.preview(t, surface, "memory.forget", input)
				if err := rig.mem.Add(t.Context(), &memorytest.Document{ID: "mem-a3", Session: memorytest.FirstSession, Content: "alpha recording three", Sequence: 3}); err != nil {
					t.Fatalf("add a document: %v", err)
				}
				call := rig.call(surface, "memory.forget", input)
				call.PreviewDigest = preview.Digest

				_, err := rig.Registry.Invoke(t.Context(), "memory.forget", call)

				if !fault.HasCode(err, operation.ErrorCodePreviewStale) {
					t.Fatalf("Invoke = %v, want %s", err, operation.ErrorCodePreviewStale)
				}
				if got := strings.Join(rig.documentIDs(t), ","); !strings.Contains(got, "mem-a1") || !strings.Contains(got, "mem-a3") {
					t.Errorf("documents = %q, want everything kept after the stale apply", got)
				}
			})
		}
	})
	t.Run("through the agent", func(t *testing.T) {
		var rig *opsRig
		rig = newOpsRigAnswering(t, func(ctx context.Context, _ *toolbroker.ApprovalPrompt) (bool, error) {
			return true, rig.mem.Add(ctx, &memorytest.Document{ID: "mem-a3", Session: memorytest.FirstSession, Content: "alpha recording three", Sequence: 3})
		})

		outcome := rig.tool("memory.forget", input)

		if outcome.Class != toolbroker.ResultExecutionFailed || outcome.Err == nil || !strings.Contains(outcome.Err.Error(), string(operation.ErrorCodePreviewStale)) {
			t.Fatalf("outcome = %+v, want a failure that names %s", outcome, operation.ErrorCodePreviewStale)
		}
		if got := strings.Join(rig.documentIDs(t), ","); !strings.Contains(got, "mem-a1") || !strings.Contains(got, "mem-a3") {
			t.Errorf("documents = %q, want everything kept after the stale apply", got)
		}
	})
}

func TestMemoryPurgeBindsItsPreview(t *testing.T) {
	rig := newOpsRig(t)
	preview := rig.preview(t, operation.SurfaceChat, "memory.purge", `{}`)
	if err := rig.mem.AddExpiredSummary(t.Context(), &memorytest.Document{ID: "mem-s2", Session: memorytest.SecondSession, Content: "another old summary", Sequence: 3}); err != nil {
		t.Fatalf("add an expired summary: %v", err)
	}
	call := rig.call(operation.SurfaceChat, "memory.purge", `{}`)
	call.PreviewDigest = preview.Digest

	_, err := rig.Registry.Invoke(t.Context(), "memory.purge", call)

	if !fault.HasCode(err, operation.ErrorCodePreviewStale) {
		t.Fatalf("Invoke = %v, want %s", err, operation.ErrorCodePreviewStale)
	}
	if got := strings.Join(rig.documentIDs(t), ","); !strings.Contains(got, memorytest.ExpiredSummary) || !strings.Contains(got, "mem-s2") {
		t.Errorf("documents = %q, want both expired summaries kept", got)
	}
}

func TestADestructiveMemoryCallWithoutAPreviewDeletesNothing(t *testing.T) {
	for _, tc := range []struct{ id, input string }{
		{id: "memory.forget", input: `{"id":"mem-a1"}`},
		{id: "memory.purge", input: `{}`},
	} {
		t.Run(tc.id, func(t *testing.T) {
			rig := newOpsRig(t)
			before := rig.documentIDs(t)

			_, err := rig.invoke(t, operation.SurfaceChat, tc.id, tc.input)

			if !fault.HasCode(err, operation.ErrorCodePreviewStale) {
				t.Fatalf("Invoke = %v, want %s", err, operation.ErrorCodePreviewStale)
			}
			if got := rig.documentIDs(t); strings.Join(got, ",") != strings.Join(before, ",") {
				t.Errorf("documents = %v, want %v", got, before)
			}
		})
	}
}

func TestMemoryForgetNeedsExactlyOneScope(t *testing.T) {
	cases := []struct{ name, input string }{
		{name: "none", input: `{}`},
		{name: "two", input: `{"id":"mem-a1","query":"alpha"}`},
		{name: "three", input: `{"id":"mem-a1","query":"alpha","session":"` + memorytest.FirstSession + `"}`},
	}
	for _, tc := range cases {
		for _, surface := range []operation.Surface{operation.SurfaceCLI, operation.SurfaceChat} {
			t.Run(tc.name+" on "+string(surface), func(t *testing.T) {
				rig := newOpsRig(t)

				_, err := rig.Registry.Preview(t.Context(), "memory.forget", rig.call(surface, "memory.forget", tc.input))

				if !fault.HasCode(err, operation.ErrorCodeInputInvalid) {
					t.Fatalf("Preview = %v, want %s", err, operation.ErrorCodeInputInvalid)
				}
			})
		}
	}
}

func addBulk(t *testing.T, rig *opsRig, count int) {
	t.Helper()
	for sequence := int64(10); sequence < int64(10+count); sequence++ {
		if err := rig.mem.Add(t.Context(), &memorytest.Document{ID: "mem-bulk-" + strconv.FormatInt(sequence, 10), Session: memorytest.FirstSession, Content: "bulk recording", Sequence: sequence}); err != nil {
			t.Fatalf("add a document: %v", err)
		}
	}
}

func TestTheForgetPreviewListsTheWholeSetForTheOwnerAndTheExactLabelsForTheAgent(t *testing.T) {
	rig := newOpsRig(t)
	addBulk(t, rig, 10)
	input := `{"session":"` + memorytest.FirstSession + `"}`

	owner := rig.preview(t, operation.SurfaceCLI, "memory.forget", input)
	outcome := rig.tool("memory.forget", input)

	requireOK(t, outcome)
	if len(owner.Lines) != 1+12 || owner.Lines[0] != "dry-run: 12 would be deleted" {
		t.Errorf("owner preview = %q, want the count and one line per document", owner.Lines)
	}
	cards := rig.Prompts()
	if len(cards) != 1 {
		t.Fatalf("cards = %d, want 1", len(cards))
	}
	card := cards[0].Arguments
	wants := []string{`would delete 12 memory documents of session "session-1"`, "this cannot be undone", "mem-bulk-19 session=session-1 persona=main event_text 19-19", "mem-a1 session=session-1 persona=main"}
	for _, want := range wants {
		if !strings.Contains(card, want) {
			t.Errorf("the card lacks %q:\n%s", want, card)
		}
	}
	if len(card) > tools.ApprovalDisplayLimit {
		t.Errorf("the card is %d bytes, over the %d limit", len(card), tools.ApprovalDisplayLimit)
	}
}

func TestTheForgetCardStatesTheQueryAndTheCount(t *testing.T) {
	rig := newOpsRig(t)

	outcome := rig.tool("memory.forget", `{"query":"alpha"}`)

	requireOK(t, outcome)
	card := rig.Prompts()[0].Arguments
	if !strings.Contains(card, `would delete 2 memory documents matching "alpha"`) {
		t.Errorf("the card does not state the query and the count first:\n%s", card)
	}
}

func TestAForgetCardThatCannotShowEveryLabelRefusesInsteadOfHidingOne(t *testing.T) {
	rig := newOpsRig(t)
	addBulk(t, rig, 30)
	before := rig.documentIDs(t)

	outcome := rig.tool("memory.forget", `{"session":"`+memorytest.FirstSession+`"}`)

	if outcome.Class != toolbroker.ResultApprovalDisplayTooLarge {
		t.Fatalf("outcome = %+v, want %s", outcome, toolbroker.ResultApprovalDisplayTooLarge)
	}
	if got := rig.documentIDs(t); strings.Join(got, ",") != strings.Join(before, ",") {
		t.Errorf("documents = %v, want %v untouched", got, before)
	}
	if len(rig.Prompts()) != 0 {
		t.Error("a card that does not fit was shown")
	}
}

func TestAnEmptyDeletionIsNothingToDoOnEverySurface(t *testing.T) {
	cases := []struct{ id, input string }{
		{id: "memory.forget", input: `{"id":"mem-missing"}`},
		{id: "memory.forget", input: `{"query":"zebra"}`},
		{id: "memory.purge", input: `{"limit":1}`},
	}
	for _, tc := range cases {
		t.Run(tc.id+" "+tc.input, func(t *testing.T) {
			rig := newOpsRig(t)
			if tc.id == "memory.purge" {
				if _, err := rig.invoke(t, operation.SurfaceChat, "memory.purge", `{}`); err == nil {
					t.Fatal("a purge without a preview ran")
				}
				if _, err := rig.apply(t, operation.SurfaceChat, "memory.purge", `{}`); err != nil {
					t.Fatalf("purge: %v", err)
				}
			}
			before := rig.documentIDs(t)

			for _, surface := range []operation.Surface{operation.SurfaceCLI, operation.SurfaceChat} {
				preview := rig.preview(t, surface, tc.id, tc.input)
				if !preview.Empty || len(preview.Lines) != 1 || preview.Lines[0] != "nothing to delete" {
					t.Errorf("%s preview = %+v, want nothing to delete", surface, preview)
				}
			}
			outcome := rig.tool(tc.id, tc.input)
			requireOK(t, outcome)
			if len(rig.Prompts()) != 0 {
				t.Error("an empty deletion raised an approval card")
			}
			if got := rig.documentIDs(t); strings.Join(got, ",") != strings.Join(before, ",") {
				t.Errorf("documents = %v, want %v", got, before)
			}
		})
	}
}

func TestMemoryOperationsWriteOnlyInsideCommit(t *testing.T) {
	cases := []struct {
		name      string
		id        string
		input     string
		previewed bool
	}{
		{name: "forget by session", id: "memory.forget", input: `{"session":"` + memorytest.FirstSession + `"}`, previewed: true},
		{name: "forget by id", id: "memory.forget", input: `{"id":"mem-b1"}`, previewed: true},
		{name: "forget by query", id: "memory.forget", input: `{"query":"alpha"}`, previewed: true},
		{name: "purge", id: "memory.purge", input: `{}`, previewed: true},
		{name: "inspect", id: "memory.inspect", input: `{}`},
		{name: "search", id: "memory.search", input: `{"query":"alpha"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newOpsRig(t)

			operationtest.RequireWritesOnlyInCommit(t, &operationtest.WriteProbe{
				Registry: rig.Registry, DB: rig.DB, ID: tc.id, Input: json.RawMessage(tc.input), Previewed: tc.previewed,
			})
		})
	}
}

func TestAModuleFailurePrintsItsCodeOnce(t *testing.T) {
	const unavailable = string(memory.ErrorCodeInvalidArgument)
	unavailableRig := func(t *testing.T) *operationtest.Rig {
		t.Helper()
		return operationtest.NewRig(t, &operationtest.Options{
			DB:       operationtest.OpenDatabase(t, memory.Schema()),
			Catalogs: []operation.Catalog{memory.Catalog()},
			Bindings: func(*operationtest.Stubs) []operation.Binding { return memory.UnavailableOperations().Bindings() },
		})
	}
	cases := []struct {
		name string
		run  func(t *testing.T, rig *operationtest.Rig) error
	}{
		{name: "the CLI", run: func(t *testing.T, rig *operationtest.Rig) error {
			_, err := rig.Registry.Invoke(t.Context(), "memory.inspect", &operation.Call{
				Caller: operation.Caller{Surface: operation.SurfaceCLI, PrincipalID: operationtest.OwnerID, Class: principal.ClassOwner},
				Input:  json.RawMessage(`{}`),
			})
			return err
		}},
		{name: "chat", run: func(t *testing.T, rig *operationtest.Rig) error {
			_, err := rig.Registry.Invoke(t.Context(), "memory.inspect", &operation.Call{
				Caller:         operation.Caller{Surface: operation.SurfaceChat, PrincipalID: operationtest.OwnerID, Class: principal.ClassOwner, SessionID: operationtest.SessionID, Direct: true},
				Input:          json.RawMessage(`{}`),
				IdempotencyKey: "once-1", RequestDigest: operation.Digest(json.RawMessage(`{}`)),
			})
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.run(t, unavailableRig(t))

			if err == nil {
				t.Fatal("an unavailable service answered")
			}
			if got := strings.Count(err.Error(), unavailable); got != 1 {
				t.Errorf("error %q prints %s %d times, want once", err, unavailable, got)
			}
		})
	}
}

func TestInspectLabelsItsOutputUntrusted(t *testing.T) {
	descriptor, ok := newOpsRig(t).Registry.Descriptor("memory.inspect")
	if !ok {
		t.Fatal("memory.inspect is not registered")
	}

	if descriptor.ResultTaint == nil {
		t.Error("memory.inspect does not evaluate result taint per row")
	}
}

func personaRig(t *testing.T) *opsRig {
	t.Helper()
	rig := newOpsRig(t)
	if err := rig.mem.SeedPersonas(t.Context()); err != nil {
		t.Fatalf("seed the personas: %v", err)
	}
	return rig
}

func TestAgentInspectListsOnlyTheCallingPersonasMemory(t *testing.T) {
	rig := personaRig(t)

	alpha := rig.toolAs(memorytest.AlphaPersona, "memory.inspect", `{}`)
	requireOK(t, alpha)
	beta := rig.toolAs(memorytest.BetaPersona, "memory.inspect", `{"query":"trains"}`)
	requireOK(t, beta)

	alphaIDs, betaIDs := rowIDs(t, alpha.Output), rowIDs(t, beta.Output)
	if strings.Join(alphaIDs, ",") != "mem-alpha-1,mem-alpha-old" {
		t.Errorf("alpha saw %v, want only its own documents", alphaIDs)
	}
	if strings.Join(betaIDs, ",") != "mem-beta-1" {
		t.Errorf("beta saw %v, want only its own document", betaIDs)
	}
}

func TestOwnerSurfacesInspectEveryPersona(t *testing.T) {
	rig := personaRig(t)

	for _, surface := range []operation.Surface{operation.SurfaceCLI, operation.SurfaceChat} {
		result, err := rig.invoke(t, surface, "memory.inspect", `{"query":"trains"}`)
		if err != nil {
			t.Fatalf("%s inspect: %v", surface, err)
		}
		if got := len(resultRows(t, result.Data)); got != 2 {
			t.Errorf("%s inspect returned %d documents, want both personas", surface, got)
		}
	}
}

func TestAgentForgetStaysWithinTheCallingPersona(t *testing.T) {
	cases := []struct{ name, input string }{
		{name: "by session", input: `{"session":"` + memorytest.BetaSession + `"}`},
		{name: "by id", input: `{"id":"mem-beta-1"}`},
		{name: "by query", input: `{"query":"beta"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := personaRig(t)
			before := rig.documentIDs(t)

			outcome := rig.toolAs(memorytest.AlphaPersona, "memory.forget", tc.input)

			if tc.name == "by session" && outcome.Class == toolbroker.ResultOK {
				t.Fatalf("alpha forgot a session of beta: %+v", outcome)
			}
			if len(rig.Prompts()) != 0 {
				t.Errorf("a card was raised for memory alpha cannot reach: %s", rig.Prompts()[0].Arguments)
			}
			if got := rig.documentIDs(t); strings.Join(got, ",") != strings.Join(before, ",") {
				t.Errorf("documents = %v, want %v untouched", got, before)
			}
		})
	}
}

func TestAgentForgetRemovesOwnMemoryAndLeavesTheOtherPersona(t *testing.T) {
	rig := personaRig(t)

	outcome := rig.toolAs(memorytest.AlphaPersona, "memory.forget", `{"query":"trains"}`)

	requireOK(t, outcome)
	ids := strings.Join(rig.documentIDs(t), ",")
	if strings.Contains(ids, "mem-alpha-1") || !strings.Contains(ids, "mem-beta-1") {
		t.Errorf("documents = %s, want alpha's trains note gone and beta's kept", ids)
	}
}

func TestTheOwnerForgetCoversEveryPersona(t *testing.T) {
	rig := personaRig(t)

	if _, err := rig.apply(t, operation.SurfaceCLI, "memory.forget", `{"query":"trains"}`); err != nil {
		t.Fatalf("forget: %v", err)
	}

	ids := strings.Join(rig.documentIDs(t), ",")
	if strings.Contains(ids, "mem-alpha-1") || strings.Contains(ids, "mem-beta-1") {
		t.Errorf("documents = %s, want both trains notes gone", ids)
	}
}

func TestAnUnknownPersonaReachesNoMemory(t *testing.T) {
	rig := personaRig(t)

	outcome := rig.toolAs("ghost", "memory.inspect", `{}`)

	if outcome.Class == toolbroker.ResultOK {
		t.Errorf("a persona without a memory scope listed documents: %s", outcome.Output)
	}
}

func (r *opsRig) cli(t *testing.T, args ...string) string {
	t.Helper()
	open := func(_ *cobra.Command, _ *operation.Descriptor, run func(context.Context, operation.Invoker) error) error {
		return run(t.Context(), r.Registry)
	}
	roots, err := operation.Commands([]operation.Catalog{memory.Catalog()}, open)
	if err != nil {
		t.Fatalf("Commands: %v", err)
	}
	var out bytes.Buffer
	root := &cobra.Command{Use: "aura", SilenceUsage: true, SilenceErrors: true}
	root.AddCommand(roots...)
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	if err := root.ExecuteContext(t.Context()); err != nil {
		t.Fatalf("aura %s: %v\n%s", strings.Join(args, " "), err, out.String())
	}
	return out.String()
}

func TestUntrustedMemoryContentIsEscapedOnTheCLIForInspectAndThePreview(t *testing.T) {
	rig := newOpsRig(t)
	content := "red\u001b[31m \u202egnp.exe\a boom"
	if err := rig.mem.Add(t.Context(), &memorytest.Document{ID: "mem-hostile", Session: memorytest.FirstSession, Content: content, Sequence: 40}); err != nil {
		t.Fatalf("add: %v", err)
	}

	for _, args := range [][]string{
		{"memory", "inspect", "--session", memorytest.FirstSession},
		{"memory", "forget", "--id", "mem-hostile"},
		{"memory", "forget", "--query", "boom"},
	} {
		out := rig.cli(t, args...)
		for _, raw := range []string{"\u001b", "\u202e", "\a"} {
			if strings.Contains(out, raw) {
				t.Errorf("aura %s printed a raw %q:\n%s", strings.Join(args, " "), raw, out)
			}
		}
	}
	if out := rig.cli(t, "memory", "inspect", "--session", memorytest.FirstSession); !strings.Contains(out, `\u{202e}`) {
		t.Errorf("inspect did not show the escaped direction control:\n%s", out)
	}
}

func TestAnOversizedForgetCardIsAvoidedByDeletingInBatchesWithLimit(t *testing.T) {
	rig := newOpsRig(t)
	addBulk(t, rig, 30)
	before := len(rig.documentIDs(t))

	outcome := rig.tool("memory.forget", `{"session":"`+memorytest.FirstSession+`","limit":10}`)

	requireOK(t, outcome)
	if after := len(rig.documentIDs(t)); after != before-10 {
		t.Errorf("documents = %d, want a batch of 10 removed from %d", after, before)
	}
}

func TestTheAgentDescriptionOfForgetTellsHowToAvoidARefusedCard(t *testing.T) {
	rig := newOpsRig(t)
	for _, registration := range rig.Registry.Tools() {
		if registration.Definition.Name != operation.ToolName("memory.forget") {
			continue
		}
		for _, want := range []string{"refused", "limit", "batches"} {
			if !strings.Contains(registration.Definition.Description, want) {
				t.Errorf("the description lacks %q: %s", want, registration.Definition.Description)
			}
		}
		return
	}
	t.Fatal("the memory.forget tool is not registered")
}

func (r *opsRig) cliRun(t *testing.T, args ...string) (string, error) {
	t.Helper()
	open := func(_ *cobra.Command, _ *operation.Descriptor, run func(context.Context, operation.Invoker) error) error {
		return run(t.Context(), r.Registry)
	}
	roots, err := operation.Commands([]operation.Catalog{memory.Catalog()}, open)
	if err != nil {
		t.Fatalf("Commands: %v", err)
	}
	var out bytes.Buffer
	root := &cobra.Command{Use: "aura", SilenceUsage: true, SilenceErrors: true}
	root.AddCommand(roots...)
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err = root.ExecuteContext(t.Context())
	return out.String(), err
}

func TestMemorySearchOperationFiltersByTimeScopeAndTaint(t *testing.T) {
	ctx := t.Context()
	now := time.Date(2026, 9, 12, 12, 5, 0, 0, time.UTC)
	db := operationtest.OpenDatabase(t, memory.Schema())
	store, err := memory.NewSQLiteStore(db, turnstore.NewSessionService(db))
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	vault := secret.New()
	vault.Set("api_key", "sk-canary-secret-token-999")
	service, err := memory.NewService(store, vault, &memory.ServiceOptions{TokenBudget: 600})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	scopes := memorytest.Scopes{
		memorytest.MainPersona:  {memorytest.MainPersona},
		memorytest.AlphaPersona: {memorytest.AlphaPersona},
		memorytest.BetaPersona:  {memorytest.BetaPersona},
		"shared-a":              {"shared-a", "shared-b"},
		"shared-b":              {"shared-a", "shared-b"},
	}
	operations, err := memory.NewOperations(&memory.OperationDeps{
		Service: service,
		Scopes:  scopes,
		Now:     func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewOperations: %v", err)
	}
	rig := &opsRig{
		Rig: operationtest.NewRig(t, &operationtest.Options{
			DB:       db,
			Catalogs: []operation.Catalog{memory.Catalog()},
			Bindings: func(*operationtest.Stubs) []operation.Binding { return operations.Bindings() },
		}),
	}
	sessions := turnstore.NewSessionService(db)
	for _, s := range []struct{ id, agent string }{
		{"sess-alpha", memorytest.AlphaPersona},
		{"sess-beta", memorytest.BetaPersona},
		{"sess-shared-a", "shared-a"},
		{"sess-shared-b", "shared-b"},
	} {
		if err := sessions.Create(ctx, &turn.Session{
			ID: s.id, OwnerID: principal.OwnerID, AgentID: s.agent,
			CreatedAt: now, UpdatedAt: now, Metadata: []byte(`{}`),
		}); err != nil {
			t.Fatalf("create session %s: %v", s.id, err)
		}
	}
	seedDoc := func(id, sessionID, content string, seq int64, tainted bool, createdAt time.Time) {
		t.Helper()
		taintVal := tainted
		if _, err := store.UpsertDocument(ctx, &memory.StoredDocument{
			ID: id, OwnerID: principal.OwnerID, SessionID: sessionID, Kind: memory.KindEventText,
			FromSequence: seq, ToSequence: seq, Content: content,
			TrustLabel: "owner_input", Tainted: &taintVal, CreatedAt: createdAt,
		}); err != nil {
			t.Fatalf("UpsertDocument(%s): %v", id, err)
		}
	}

	t0 := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	t500 := time.Date(2026, 9, 12, 12, 0, 0, 500_000_000, time.UTC)
	t1000 := time.Date(2026, 9, 12, 12, 0, 1, 0, time.UTC)
	seedDoc("mem-t0", "sess-alpha", "alpha deploy timeline zero", 1, false, t0)
	seedDoc("mem-t500", "sess-alpha", "alpha deploy timeline half", 2, false, t500)
	seedDoc("mem-t1000", "sess-alpha", "alpha deploy timeline one", 3, true, t1000)
	seedDoc("mem-beta-priv", "sess-beta", "beta secret deploy timeline", 1, false, t500)
	seedDoc("mem-shared-a", "sess-shared-a", "shared roadmap milestone alpha", 1, false, t0)
	seedDoc("mem-shared-b", "sess-shared-b", "shared roadmap milestone bravo", 1, false, t500)
	seedDoc("mem-secret-row", "sess-alpha", "canarytoken sk-canary-secret-token-999", 4, false, t500)
	seedDoc("mem-hostile", "sess-alpha", "hostile\u001b[31m \u202egnp.exe\a alert", 5, true, t500)
	longContent := "longbound " + strings.Repeat("é", 1500)
	seedDoc("mem-long-1", "sess-alpha", longContent, 6, false, t500)
	seedDoc("mem-long-2", "sess-alpha", longContent, 7, false, t0)

	t.Run("exact sub-second after and before bounds", func(t *testing.T) {
		exactHalf, err := rig.invoke(t, operation.SurfaceCLI, "memory.search", `{"query":"deploy timeline","session":"sess-alpha","after":"2026-09-12T12:00:00.5Z","before":"2026-09-12T12:00:00.5Z"}`)
		if err != nil {
			t.Fatalf("exact half-second search: %v", err)
		}
		if got := rowIDs(t, string(exactHalf.Data)); strings.Join(got, ",") != "mem-t500" {
			t.Errorf("exact .5Z search = %v, want [mem-t500]", got)
		}

		beforeWhole, err := rig.invoke(t, operation.SurfaceCLI, "memory.search", `{"query":"deploy timeline","session":"sess-alpha","before":"2026-09-12T12:00:00Z"}`)
		if err != nil {
			t.Fatalf("before whole-second search: %v", err)
		}
		if got := rowIDs(t, string(beforeWhole.Data)); strings.Join(got, ",") != "mem-t0" {
			t.Errorf("before Z search = %v, want [mem-t0] without .5Z", got)
		}

		limited, err := rig.invoke(t, operation.SurfaceChat, "memory.search", `{"query":"deploy timeline","session":"sess-alpha","limit":1}`)
		if err != nil {
			t.Fatalf("chat limited search: %v", err)
		}
		if got := len(resultRows(t, limited.Data)); got != 1 {
			t.Errorf("chat limit=1 rows = %d, want 1", got)
		}
	})

	t.Run("invalid inputs return operation_input_invalid and exit usage", func(t *testing.T) {
		cases := []struct {
			name string
			in   string
			cli  []string
		}{
			{name: "bad after", in: `{"query":"deploy","after":"not-a-timestamp"}`, cli: []string{"memory", "search", "deploy", "--after", "not-a-timestamp"}},
			{name: "bad before", in: `{"query":"deploy","before":"2026-13-99"}`, cli: []string{"memory", "search", "deploy", "--before", "2026-13-99"}},
			{name: "after greater than before", in: `{"query":"deploy","after":"2026-09-12T12:00:02Z","before":"2026-09-12T12:00:01Z"}`, cli: []string{"memory", "search", "deploy", "--after", "2026-09-12T12:00:02Z", "--before", "2026-09-12T12:00:01Z"}},
			{name: "punctuation only query", in: `{"query":"!!! ((("}`, cli: []string{"memory", "search", "!!! ((("}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				if _, err := rig.invoke(t, operation.SurfaceCLI, "memory.search", tc.in); !fault.HasCode(err, operation.ErrorCodeInputInvalid) {
					t.Errorf("Invoke(%s) = %v, want %s", tc.name, err, operation.ErrorCodeInputInvalid)
				}
				if _, err := rig.cliRun(t, tc.cli...); !clikit.IsUsageError(err) || !fault.HasCode(err, operation.ErrorCodeInputInvalid) {
					t.Errorf("CLI(%s) err = %v, want usage error with %s", tc.name, err, operation.ErrorCodeInputInvalid)
				}
			})
		}
	})

	t.Run("agent scope isolates private personas and honors shared scope while owner sees all", func(t *testing.T) {
		alphaOut := rig.toolAs(memorytest.AlphaPersona, "memory.search", `{"query":"deploy timeline"}`)
		requireOK(t, alphaOut)
		alphaIDs := rowIDs(t, alphaOut.Output)
		for _, id := range alphaIDs {
			if id == "mem-beta-priv" {
				t.Fatalf("alpha agent received beta private memory: %v", alphaIDs)
			}
		}
		if len(alphaIDs) != 3 {
			t.Errorf("alpha agent saw %v, want its 3 deploy timeline docs", alphaIDs)
		}

		sharedOut := rig.toolAs("shared-a", "memory.search", `{"query":"roadmap milestone"}`)
		requireOK(t, sharedOut)
		sharedIDs := rowIDs(t, sharedOut.Output)
		if len(sharedIDs) != 2 {
			t.Errorf("shared-a saw %v, want both shared-a and shared-b docs", sharedIDs)
		}
		sharedPrivate := rig.toolAs("shared-a", "memory.search", `{"query":"beta secret deploy"}`)
		requireOK(t, sharedPrivate)
		if got := rowIDs(t, sharedPrivate.Output); len(got) != 0 {
			t.Errorf("shared-a leaked private beta doc: %v", got)
		}

		ownerCLI, err := rig.invoke(t, operation.SurfaceCLI, "memory.search", `{"query":"deploy timeline"}`)
		if err != nil {
			t.Fatalf("owner CLI search: %v", err)
		}
		ownerIDs := rowIDs(t, string(ownerCLI.Data))
		if !strings.Contains(strings.Join(ownerIDs, ","), "mem-beta-priv") {
			t.Errorf("owner CLI search = %v, want cross-persona results including mem-beta-priv", ownerIDs)
		}
	})

	t.Run("secret rows are dropped and long rows are UTF-8 bounded within token budget", func(t *testing.T) {
		secretRes, err := rig.invoke(t, operation.SurfaceCLI, "memory.search", `{"query":"canarytoken"}`)
		if err != nil {
			t.Fatalf("secret query search: %v", err)
		}
		if got := resultRows(t, secretRes.Data); len(got) != 0 {
			t.Errorf("secret-bearing row leaked: %+v", got)
		}

		longRes, err := rig.invoke(t, operation.SurfaceCLI, "memory.search", `{"query":"longbound","session":"sess-alpha","limit":10}`)
		if err != nil {
			t.Fatalf("long query search: %v", err)
		}
		rows := resultRows(t, longRes.Data)
		if len(rows) != 1 {
			t.Fatalf("budgeted long rows = %d, want 1 (at least 1 fits, second exceeds 600 token budget)", len(rows))
		}
		content := fmt.Sprint(rows[0]["content"])
		if len(content) > 2048 || !utf8.ValidString(content) || !strings.HasSuffix(content, "…") {
			t.Errorf("bounded content invalid: bytes=%d valid=%t suffix=%t", len(content), utf8.ValidString(content), strings.HasSuffix(content, "…"))
		}
	})

	t.Run("cli and chat escape control characters", func(t *testing.T) {
		out := rig.cli(t, "memory", "search", "alert", "--session", "sess-alpha")
		for _, raw := range []string{"\u001b", "\u202e", "\a"} {
			if strings.Contains(out, raw) {
				t.Errorf("CLI search printed raw %q:\n%s", raw, out)
			}
		}
		if !strings.Contains(out, `\u{202e}`) {
			t.Errorf("CLI search did not escape bidi control:\n%s", out)
		}
	})

	t.Run("result taint evaluates per row and fails closed on invalid json", func(t *testing.T) {
		desc, ok := rig.Registry.Descriptor("memory.search")
		if !ok || desc.ResultTaint == nil {
			t.Fatalf("memory.search descriptor missing ResultTaint")
		}
		cleanRes, err := rig.invoke(t, operation.SurfaceCLI, "memory.search", `{"query":"deploy timeline","session":"sess-alpha","before":"2026-09-12T12:00:00.5Z"}`)
		if err != nil {
			t.Fatalf("clean search: %v", err)
		}
		if desc.ResultTaint(cleanRes.Data) {
			t.Errorf("clean rows reported tainted: %s", cleanRes.Data)
		}

		emptyRes, err := rig.invoke(t, operation.SurfaceCLI, "memory.search", `{"query":"nonexistenttermxyz"}`)
		if err != nil {
			t.Fatalf("empty search: %v", err)
		}
		if desc.ResultTaint(emptyRes.Data) {
			t.Errorf("empty rows reported tainted: %s", emptyRes.Data)
		}

		taintedRes, err := rig.invoke(t, operation.SurfaceCLI, "memory.search", `{"query":"deploy timeline","session":"sess-alpha","after":"2026-09-12T12:00:01Z"}`)
		if err != nil {
			t.Fatalf("tainted search: %v", err)
		}
		if !desc.ResultTaint(taintedRes.Data) {
			t.Errorf("tainted row reported clean: %s", taintedRes.Data)
		}

		if !desc.ResultTaint(json.RawMessage(`not json`)) {
			t.Error("unparsable JSON did not fail closed to tainted")
		}
		if !desc.ResultTaint(json.RawMessage(`{"rows":null}`)) {
			t.Error("null rows did not fail closed to tainted")
		}
	})
}
