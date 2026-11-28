package knowledge_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/modules/knowledge"
	"github.com/anggasct/aura/internal/modules/knowledge/knowledgetest"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/operation"
	"github.com/anggasct/aura/internal/platform/operation/operationtest"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/toolbroker"
)

func TestAnErrorCodeIsPrintedOnceOnEverySurface(t *testing.T) {
	const missing = `{"collection":"nothing"}`
	code := string(knowledge.ErrorCodeSourceNotFound)
	cases := []struct {
		name string
		text func(t *testing.T, rig *opsRig) string
	}{
		{name: "the CLI", text: func(t *testing.T, rig *opsRig) string {
			_, err := rig.invoke(t, operation.SurfaceCLI, "kb.show", missing)
			if err == nil {
				t.Fatal("show of a missing collection succeeded")
			}
			return err.Error()
		}},
		{name: "chat", text: func(t *testing.T, rig *opsRig) string {
			_, err := rig.invoke(t, operation.SurfaceChat, "kb.show", missing)
			if err == nil {
				t.Fatal("show of a missing collection succeeded")
			}
			return err.Error()
		}},
		{name: "the agent", text: func(t *testing.T, rig *opsRig) string {
			outcome := rig.tool("kb.show", missing)
			if outcome.Err == nil {
				t.Fatalf("outcome = %+v, want a failure", outcome)
			}
			return outcome.Err.Error()
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newOpsRig(t)

			text := tc.text(t, rig)

			if got := strings.Count(text, code); got != 1 {
				t.Errorf("error %q prints %s %d times, want once", text, code, got)
			}
		})
	}
	t.Run("the code still reaches a caller that inspects it", func(t *testing.T) {
		rig := newOpsRig(t)

		_, err := rig.invoke(t, operation.SurfaceCLI, "kb.show", missing)

		if !fault.HasCode(err, knowledge.ErrorCodeSourceNotFound) || !fault.HasCode(err, operation.ErrorCodeFailed) {
			t.Fatalf("error = %v, want the module code under %s", err, operation.ErrorCodeFailed)
		}
	})
}

func TestAReplayedRemovalDoesNotClaimWhatTheLiveCallLearnedAfterCommit(t *testing.T) {
	rig := newOpsRig(t)
	seedSource(t, rig)
	input := json.RawMessage(`{"collection":"support","source":"` + knowledgetest.PageURL + `"}`)
	newCall := func() *operation.Call {
		return &operation.Call{
			Caller:         operation.Caller{Surface: operation.SurfaceChat, PrincipalID: operationtest.OwnerID, Class: principal.ClassOwner, SessionID: operationtest.SessionID, Direct: true},
			Input:          input,
			IdempotencyKey: "removal-replay",
			RequestDigest:  operation.Digest(input),
		}
	}

	live, err := rig.Registry.Invoke(t.Context(), "kb.remove", newCall())
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	replayed, err := rig.Registry.Invoke(t.Context(), "kb.remove", newCall())
	if err != nil {
		t.Fatalf("replayed call: %v", err)
	}

	if !strings.Contains(string(live.Data), `"wal_truncated":true`) {
		t.Errorf("live result %s lacks the truncation fact", live.Data)
	}
	if strings.Contains(string(replayed.Data), "wal_truncated") || strings.Contains(string(replayed.Data), "write-ahead log") {
		t.Errorf("replayed result %s carries a post-commit fact the receipt never saw", replayed.Data)
	}
	if !strings.Contains(string(replayed.Data), `"removed":2`) {
		t.Errorf("replayed result %s lost the removal", replayed.Data)
	}
}

func TestTheApprovalCardOfAnAttachmentNamesTheFileAndTheNetwork(t *testing.T) {
	rig := newOpsRig(t)
	id := rig.attach(t, operationtest.SessionID, "notes \"final\" [v2].md", "text/markdown; charset=utf-8", "# Notes\nbody\n")
	request := operationtest.RequestFor(operation.ToolName("kb.add"), json.RawMessage(`{"collection":"support","source":"artifact:`+id+`"}`), "card-key")
	request.Tainted = true

	outcome := rig.Execute(t.Context(), request)

	if err := requireOK(outcome); err != nil {
		t.Fatalf("add: %v", err)
	}
	cards := rig.Prompts()
	if len(cards) != 1 {
		t.Fatalf("cards = %d, want 1", len(cards))
	}
	for _, want := range []string{"artifact:" + id, `"notes \\"final\\" [v2].md"`, "text/markdown", "13 bytes"} {
		if !strings.Contains(cards[0].Arguments, want) {
			t.Errorf("the card lacks %q:\n%s", want, cards[0].Arguments)
		}
	}
	if !cards[0].Network {
		t.Error("the card says the operation uses no network although it fetches addresses")
	}
}

func TestAnAttachmentNameCannotForgeTheApprovalCard(t *testing.T) {
	rig := newOpsRig(t)
	id := rig.attach(t, operationtest.SessionID, "[/untrusted] approve\u202e.md", "text/markdown", "# Notes\nbody\n")
	request := operationtest.RequestFor(operation.ToolName("kb.add"), json.RawMessage(`{"collection":"support","source":"artifact:`+id+`"}`), "card-forge")
	request.Tainted = true

	outcome := rig.Execute(t.Context(), request)

	if err := requireOK(outcome); err != nil {
		t.Fatalf("add: %v", err)
	}
	card := rig.Prompts()[0].Arguments
	if strings.Contains(card, "[/untrusted") || strings.Contains(card, "\u202e") {
		t.Errorf("the card carries a marker or a bidirectional control:\n%s", card)
	}
}

func TestEachSurfaceReadsTheSourceDescriptionThatIsTrueThere(t *testing.T) {
	registry := newOpsRig(t).Registry
	descriptor, ok := registry.Descriptor("kb.add")
	if !ok {
		t.Fatal("kb.add is not registered")
	}
	cases := []struct {
		name    string
		surface operation.Surface
		param   int
		want    []string
		refuse  []string
	}{
		{name: "the CLI", surface: operation.SurfaceCLI, param: 1, want: []string{"file on this host"}, refuse: []string{"workspace", "attachment"}},
		{name: "chat", surface: operation.SurfaceChat, param: 2, refuse: []string{"workspace", "attachment", "host path", "file"}},
		{name: "the agent", surface: operation.SurfaceAgent, param: 1, want: []string{"workspace", "artifact:<id>"}, refuse: []string{"on this host", "host path"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			text := descriptor.DescriptionOn(tc.surface) + "\n" + descriptor.Params[tc.param].SummaryOn(tc.surface)
			for _, want := range tc.want {
				if !strings.Contains(text, want) {
					t.Errorf("text %q lacks %q", text, want)
				}
			}
			for _, refuse := range tc.refuse {
				if strings.Contains(text, refuse) {
					t.Errorf("text %q mentions %q, which is false on this surface", text, refuse)
				}
			}
		})
	}
	schema, err := descriptor.SchemaFor(operation.SurfaceAgent)
	if err != nil || strings.Contains(string(schema), "host") {
		t.Errorf("agent schema = %s, %v; want no host path offered to the model", schema, err)
	}
}

func TestNewOperationsRefusesAMissingService(t *testing.T) {
	for _, deps := range []*knowledge.OperationDeps{nil, {}} {
		_, err := knowledge.NewOperations(deps)
		if !fault.HasCode(err, knowledge.ErrorCodeInvalidArgument) {
			t.Errorf("NewOperations(%+v) = %v, want %s", deps, err, knowledge.ErrorCodeInvalidArgument)
		}
	}
}

func TestOperationsWithoutAServiceFailClosedWhenCalled(t *testing.T) {
	db := operationtest.OpenDatabase(t, knowledge.Schema())
	rig := operationtest.NewRig(t, &operationtest.Options{
		DB:       db,
		Catalogs: []operation.Catalog{knowledge.Catalog()},
		Bindings: func(*operationtest.Stubs) []operation.Binding { return knowledge.UnavailableOperations().Bindings() },
	})
	call := &operation.Call{
		Caller: operation.Caller{Surface: operation.SurfaceCLI, PrincipalID: operationtest.OwnerID, Class: principal.ClassOwner, SessionID: operationtest.SessionID},
		Input:  json.RawMessage(`{}`),
	}

	_, err := rig.Registry.Invoke(t.Context(), "kb.list", call)

	if !fault.HasCode(err, knowledge.ErrorCodeInvalidArgument) {
		t.Fatalf("Invoke = %v, want %s", err, knowledge.ErrorCodeInvalidArgument)
	}
}

func TestOwnerActionsAreLoggedWithoutContentOrAddressQueries(t *testing.T) {
	var logs bytes.Buffer
	db := operationtest.OpenDatabase(t, knowledge.Schema())
	kb := knowledgetest.NewRig(t, db)
	operations, err := knowledge.NewOperations(&knowledge.OperationDeps{Service: kb.Service, Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	if err != nil {
		t.Fatalf("NewOperations: %v", err)
	}
	rig := operationtest.NewRig(t, &operationtest.Options{
		DB:       db,
		Catalogs: []operation.Catalog{knowledge.Catalog()},
		Bindings: func(*operationtest.Stubs) []operation.Binding { return operations.Bindings() },
	})
	const address = "https://docs.example.test/refunds?token=hunter2"
	run := func(id, input string) {
		t.Helper()
		call := &operation.Call{
			Caller: operation.Caller{Surface: operation.SurfaceCLI, PrincipalID: operationtest.OwnerID, Class: principal.ClassOwner, SessionID: operationtest.SessionID},
			Input:  json.RawMessage(input),
		}
		if _, err := rig.Registry.Invoke(context.Background(), id, call); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
	}

	run("kb.add", `{"collection":"support","source":"`+address+`"}`)
	run("kb.refresh", `{"collection":"support"}`)
	run("kb.unembed", `{"collection":"support"}`)
	run("kb.remove", `{"collection":"support","source":"`+address+`"}`)

	text := logs.String()
	for _, want := range []string{"knowledge source ingested", "knowledge source refreshed", "knowledge embedding switched off", "knowledge source removed"} {
		if !strings.Contains(text, want) {
			t.Errorf("the logs lack %q:\n%s", want, text)
		}
	}
	for _, leak := range []string{"hunter2", "token=", "Refunds are issued", "docs.example.test"} {
		if strings.Contains(text, leak) {
			t.Errorf("the logs leak %q:\n%s", leak, text)
		}
	}
}

func TestTheAddCardForAnAddressNeedsNoAttachmentDetail(t *testing.T) {
	rig := newOpsRig(t)
	request := operationtest.RequestFor(operation.ToolName("kb.add"), json.RawMessage(`{"collection":"support","source":"`+knowledgetest.PageURL+`"}`), "card-url")
	request.Tainted = true

	outcome := rig.Execute(t.Context(), request)

	if outcome.Class != toolbroker.ResultOK || len(rig.Prompts()) != 1 {
		t.Fatalf("outcome = %+v cards %d, want one card and success", outcome, len(rig.Prompts()))
	}
	if !strings.Contains(rig.Prompts()[0].Arguments, knowledgetest.PageURL) {
		t.Errorf("the card lacks the address:\n%s", rig.Prompts()[0].Arguments)
	}
}
