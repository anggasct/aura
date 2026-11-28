package memory_test

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/modules/memory"
	"github.com/anggasct/aura/internal/modules/memory/memorytest"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/operation"
	"github.com/anggasct/aura/internal/platform/operation/operationtest"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/tools"
)

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
	operations, err := memory.NewOperations(&memory.OperationDeps{Service: mem.Service})
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
	return r.Execute(context.Background(), operationtest.RequestFor(operation.ToolName(id), json.RawMessage(input), r.key(id)))
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
	return operationtest.InvokePreviewed(t, r.Registry, id, r.call(surface, id, input))
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

func resultText(t *testing.T, data json.RawMessage) string {
	t.Helper()
	var decoded struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("decode %s: %v", data, err)
	}
	return decoded.Text
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
		if resultText(t, json.RawMessage(fromTool.Output)) != resultText(t, fromCLI.Data) || resultText(t, fromCLI.Data) != resultText(t, fromChat.Data) {
			t.Fatalf("%s printed different text\ntool: %s\ncli: %s\nchat: %s", step.name, fromTool.Output, fromCLI.Data, fromChat.Data)
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
				if err := rig.mem.Add(t.Context(), "mem-a3", memorytest.FirstSession, "alpha recording three", 3); err != nil {
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
			return true, rig.mem.Add(ctx, "mem-a3", memorytest.FirstSession, "alpha recording three", 3)
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
	if err := rig.mem.AddExpiredSummary(t.Context(), "mem-s2", memorytest.SecondSession, 3); err != nil {
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

func TestTheForgetPreviewListsTheWholeSetForTheOwnerAndABoundedCardForTheAgent(t *testing.T) {
	rig := newOpsRig(t)
	for sequence := int64(10); sequence < 22; sequence++ {
		if err := rig.mem.Add(t.Context(), "mem-bulk-"+strconv.FormatInt(sequence, 10), memorytest.FirstSession, "bulk recording", sequence); err != nil {
			t.Fatalf("add a document: %v", err)
		}
	}
	input := `{"session":"` + memorytest.FirstSession + `"}`

	owner := rig.preview(t, operation.SurfaceCLI, "memory.forget", input)
	outcome := rig.tool("memory.forget", input)

	requireOK(t, outcome)
	if len(owner.Lines) != 1+14 || owner.Lines[0] != "dry-run: 14 would be deleted" {
		t.Errorf("owner preview = %q, want the count and one line per document", owner.Lines)
	}
	cards := rig.Prompts()
	if len(cards) != 1 {
		t.Fatalf("cards = %d, want 1", len(cards))
	}
	card := cards[0].Arguments
	for _, want := range []string{"would delete 14 memory documents", "and 4 more", "this cannot be undone"} {
		if !strings.Contains(card, want) {
			t.Errorf("the card lacks %q:\n%s", want, card)
		}
	}
	if len(card) > tools.ApprovalDisplayLimit {
		t.Errorf("the card is %d bytes, over the %d limit", len(card), tools.ApprovalDisplayLimit)
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

	if !descriptor.UntrustedOutput {
		t.Error("memory.inspect does not mark its output untrusted")
	}
}
