package knowledge_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/modules/knowledge"
	"github.com/anggasct/aura/internal/modules/knowledge/knowledgetest"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/operation"
	"github.com/anggasct/aura/internal/platform/operation/operationtest"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/storage"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/turn"
	turnstore "github.com/anggasct/aura/internal/platform/turn/store"
)

const otherSessionID = "session-2"

type opsRig struct {
	*operationtest.Rig
	kb        *knowledgetest.Rig
	workspace string
	artifacts storage.ArtifactStore
	calls     int
}

func newOpsRig(t *testing.T) *opsRig {
	t.Helper()
	db := operationtest.OpenDatabase(t, knowledge.Schema())
	now := time.Now().UTC()
	if err := turnstore.NewSessionService(db).Create(t.Context(), &turn.Session{ID: otherSessionID, OwnerID: operationtest.OwnerID, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("create the second session: %v", err)
	}
	kb := knowledgetest.NewRig(t, db)
	operations, err := knowledge.NewOperations(&knowledge.OperationDeps{Service: kb.Service})
	if err != nil {
		t.Fatalf("NewOperations: %v", err)
	}
	workspace := t.TempDir()
	artifacts := storage.NewArtifactStore(db, t.TempDir(), 1<<30)
	rig := operationtest.NewRig(t, &operationtest.Options{
		DB:       db,
		Catalogs: []operation.Catalog{knowledge.Catalog()},
		Bindings: func(*operationtest.Stubs) []operation.Binding { return operations.Bindings() },
		Files:    &operation.FileDeps{Workspace: workspace, Artifacts: artifacts},
	})
	return &opsRig{Rig: rig, kb: kb, workspace: workspace, artifacts: artifacts}
}

func (r *opsRig) key(label string) string {
	r.calls++
	return "ops-" + label + "-" + strconv.Itoa(r.calls)
}

func (r *opsRig) tool(id string, input string) operationtest.Outcome {
	return r.Execute(context.Background(), operationtest.RequestFor(operation.ToolName(id), json.RawMessage(input), r.key(id)))
}

func (r *opsRig) invoke(t *testing.T, surface operation.Surface, id, input string) (operation.Result, error) {
	t.Helper()
	call := &operation.Call{
		Caller: operation.Caller{Surface: surface, PrincipalID: operationtest.OwnerID, Class: principal.ClassOwner, SessionID: operationtest.SessionID, Direct: true},
		Input:  json.RawMessage(input),
	}
	if surface != operation.SurfaceCLI {
		call.IdempotencyKey = r.key(id)
		call.RequestDigest = operation.Digest(call.Input)
	}
	return r.Registry.Invoke(t.Context(), id, call)
}

func (r *opsRig) attach(t *testing.T, session, name, mediaType, body string) string {
	t.Helper()
	ref, err := r.artifacts.Put(t.Context(), strings.NewReader(body), &storage.ArtifactMetadata{
		ID: "art-" + strconv.Itoa(r.calls+100), SessionID: session, Filename: name, MediaType: mediaType,
	})
	r.calls++
	if err != nil {
		t.Fatalf("store the attachment: %v", err)
	}
	return ref.ID
}

func (r *opsRig) writeWorkspaceFile(t *testing.T, name, body string) {
	t.Helper()
	target := filepath.Join(r.workspace, name)
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatalf("create the directory: %v", err)
	}
	if err := os.WriteFile(target, []byte(body), 0o600); err != nil {
		t.Fatalf("write the file: %v", err)
	}
}

func (r *opsRig) sources(t *testing.T) []knowledge.SourceRecord {
	t.Helper()
	detail, err := r.kb.Service.Show(t.Context(), knowledgetest.Collection)
	if fault.HasCode(err, knowledge.ErrorCodeSourceNotFound) {
		return nil
	}
	if err != nil {
		t.Fatalf("Show: %v", err)
	}
	return detail.Sources
}

func seedSource(t *testing.T, rig *opsRig) {
	t.Helper()
	prepared, err := rig.kb.Service.PrepareURL(t.Context(), knowledgetest.Collection, knowledgetest.PageURL)
	if err != nil {
		t.Fatalf("PrepareURL: %v", err)
	}
	if _, err := rig.kb.Service.Write(t.Context(), knowledge.ActionAdd, prepared); err != nil {
		t.Fatalf("Write: %v", err)
	}
}

func TestCatalogIsValid(t *testing.T) {
	if err := operation.ValidateCatalogs([]operation.Catalog{knowledge.Catalog()}); err != nil {
		t.Fatalf("ValidateCatalogs: %v", err)
	}
}

func TestKnowledgeAddSourcesBySurface(t *testing.T) {
	const documentBody = "# Policy\nRefunds are issued within thirty days.\n"
	cases := []struct {
		name     string
		run      func(t *testing.T, rig *opsRig) error
		wantURI  func(rig *opsRig) string
		wantKind string
	}{
		{
			name: "the CLI reads a host path",
			run: func(t *testing.T, rig *opsRig) error {
				host := filepath.Join(t.TempDir(), "policy.md")
				if err := os.WriteFile(host, []byte(documentBody), 0o600); err != nil {
					t.Fatal(err)
				}
				_, err := rig.invoke(t, operation.SurfaceCLI, "kb.add", `{"collection":"support","source":`+quote(host)+`}`)
				return err
			},
			wantKind: knowledge.SourceKindFile,
		},
		{
			name: "the CLI fetches an https URL",
			run: func(t *testing.T, rig *opsRig) error {
				_, err := rig.invoke(t, operation.SurfaceCLI, "kb.add", `{"collection":"support","source":"`+knowledgetest.PageURL+`"}`)
				return err
			},
			wantURI:  func(*opsRig) string { return knowledgetest.PageURL },
			wantKind: knowledge.SourceKindURL,
		},
		{
			name: "chat fetches an https URL",
			run: func(t *testing.T, rig *opsRig) error {
				_, err := rig.invoke(t, operation.SurfaceChat, "kb.add", `{"collection":"support","url":"`+knowledgetest.PageURL+`"}`)
				return err
			},
			wantURI:  func(*opsRig) string { return knowledgetest.PageURL },
			wantKind: knowledge.SourceKindURL,
		},
		{
			name: "the agent fetches an https URL",
			run: func(t *testing.T, rig *opsRig) error {
				return requireOK(rig.tool("kb.add", `{"collection":"support","source":"`+knowledgetest.PageURL+`"}`))
			},
			wantURI:  func(*opsRig) string { return knowledgetest.PageURL },
			wantKind: knowledge.SourceKindURL,
		},
		{
			name: "the agent reads a workspace path",
			run: func(t *testing.T, rig *opsRig) error {
				rig.writeWorkspaceFile(t, "docs/policy.md", documentBody)
				return requireOK(rig.tool("kb.add", `{"collection":"support","source":"docs/policy.md"}`))
			},
			wantURI:  func(*opsRig) string { return "workspace:docs/policy.md" },
			wantKind: knowledge.SourceKindFile,
		},
		{
			name: "the agent reads an attachment of its session",
			run: func(t *testing.T, rig *opsRig) error {
				id := rig.attach(t, operationtest.SessionID, "policy.md", "text/markdown; charset=utf-8", documentBody)
				return requireOK(rig.tool("kb.add", `{"collection":"support","source":"artifact:`+id+`"}`))
			},
			wantURI:  func(*opsRig) string { return "" },
			wantKind: knowledge.SourceKindFile,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newOpsRig(t)

			if err := tc.run(t, rig); err != nil {
				t.Fatalf("add: %v", err)
			}

			sources := rig.sources(t)
			if len(sources) != 1 || sources[0].Kind != tc.wantKind || sources[0].Chunks == 0 {
				t.Fatalf("sources = %+v, want one %s source with chunks", sources, tc.wantKind)
			}
			if want := tc.wantURI; want != nil && want(rig) != "" && sources[0].URI != want(rig) {
				t.Errorf("stored address = %q, want %q", sources[0].URI, want(rig))
			}
			if actions := rig.kb.Ledger.Actions(); len(actions) != 1 || actions[0].Action != knowledge.ActionAdd {
				t.Errorf("owner actions = %+v, want one add", actions)
			}
		})
	}
}

func quote(value string) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

func requireOK(outcome operationtest.Outcome) error {
	if outcome.Class != toolbroker.ResultOK {
		return outcomeError(outcome)
	}
	return nil
}

type outcomeError operationtest.Outcome

func (o outcomeError) Error() string {
	if o.Err != nil {
		return string(o.Class) + ": " + o.Err.Error()
	}
	return string(o.Class) + ": " + o.Output
}

func TestAgentSourcesStayInWorkspaceAndSession(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "outside-secret.md")
	if err := os.WriteFile(outside, []byte("# Secret\nnot for the index\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name      string
		source    func(t *testing.T, rig *opsRig) string
		wantClass toolbroker.ResultClass
		wantCode  string
	}{
		{
			name:      "an absolute host path",
			source:    func(*testing.T, *opsRig) string { return outside },
			wantClass: toolbroker.ResultPolicyDenied,
		},
		{
			name:      "a path climbing out of the workspace",
			source:    func(*testing.T, *opsRig) string { return "../outside-secret.md" },
			wantClass: toolbroker.ResultPolicyDenied,
		},
		{
			name: "a symlink leaving the workspace",
			source: func(t *testing.T, rig *opsRig) string {
				if err := os.Symlink(outside, filepath.Join(rig.workspace, "link.md")); err != nil {
					t.Fatal(err)
				}
				return "link.md"
			},
			wantClass: toolbroker.ResultPolicyDenied,
		},
		{
			name:      "a missing workspace file",
			source:    func(*testing.T, *opsRig) string { return "nothing.md" },
			wantClass: toolbroker.ResultExecutionFailed,
			wantCode:  "source_unavailable",
		},
		{
			name: "an attachment of another session",
			source: func(t *testing.T, rig *opsRig) string {
				return "artifact:" + rig.attach(t, otherSessionID, "policy.md", "text/markdown", "# Policy\nother session\n")
			},
			wantClass: toolbroker.ResultExecutionFailed,
			wantCode:  "source_unavailable",
		},
		{
			name:      "an unknown attachment",
			source:    func(*testing.T, *opsRig) string { return "artifact:art-0000000000000000" },
			wantClass: toolbroker.ResultExecutionFailed,
			wantCode:  "source_unavailable",
		},
		{
			name:      "an attachment id that is too long",
			source:    func(*testing.T, *opsRig) string { return "artifact:" + strings.Repeat("a", 200) },
			wantClass: toolbroker.ResultExecutionFailed,
			wantCode:  "source_unavailable",
		},
		{
			name:      "a plain http URL",
			source:    func(*testing.T, *opsRig) string { return "http://docs.example.test/refunds" },
			wantClass: toolbroker.ResultExecutionFailed,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newOpsRig(t)
			source := tc.source(t, rig)

			outcome := rig.tool("kb.add", `{"collection":"support","source":`+quote(source)+`}`)

			if outcome.Class != tc.wantClass {
				t.Fatalf("outcome = %+v, want %s", outcome, tc.wantClass)
			}
			if tc.wantCode != "" && (outcome.Err == nil || !strings.Contains(outcome.Err.Error(), tc.wantCode)) {
				t.Errorf("error = %v, want the code %s", outcome.Err, tc.wantCode)
			}
			if outcome.Err != nil && (strings.Contains(outcome.Err.Error(), outside) || strings.Contains(outcome.Err.Error(), rig.workspace)) {
				t.Errorf("error %q names a host path", outcome.Err)
			}
			if sources := rig.sources(t); len(sources) != 0 || rig.kb.Site.Fetches() != 0 {
				t.Errorf("sources %+v fetches %d, want nothing stored or fetched", sources, rig.kb.Site.Fetches())
			}
		})
	}
}

func TestAgentAttachmentOfAnUnsupportedTypeIsRefused(t *testing.T) {
	cases := []struct {
		name      string
		mediaType string
		body      string
		wantCode  knowledge.ErrorCode
	}{
		{name: "a PDF", mediaType: "application/pdf", body: "%PDF-1.7 not text", wantCode: knowledge.ErrorCodeSourceUnsupported},
		{name: "audio", mediaType: "audio/ogg", body: "OggS", wantCode: knowledge.ErrorCodeSourceUnsupported},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newOpsRig(t)
			id := rig.attach(t, operationtest.SessionID, "upload.bin", tc.mediaType, tc.body)

			outcome := rig.tool("kb.add", `{"collection":"support","source":"artifact:`+id+`"}`)

			if outcome.Class != toolbroker.ResultExecutionFailed || outcome.Err == nil || !strings.Contains(outcome.Err.Error(), string(tc.wantCode)) {
				t.Fatalf("outcome = %+v, want %s", outcome, tc.wantCode)
			}
			if sources := rig.sources(t); len(sources) != 0 {
				t.Errorf("sources = %+v, want none", sources)
			}
		})
	}
}

func TestKnowledgeOperationsWriteOnlyInsideCommit(t *testing.T) {
	const byCollection = `{"collection":"support"}`
	const bySource = `{"collection":"support","source":"` + knowledgetest.PageURL + `"}`
	cases := []struct {
		name  string
		id    string
		input string
		seed  bool
	}{
		{name: "add", id: "kb.add", input: `{"collection":"support","url":"` + knowledgetest.PageURL + `"}`},
		{name: "refresh", id: "kb.refresh", input: byCollection, seed: true},
		{name: "remove", id: "kb.remove", input: bySource, seed: true},
		{name: "unembed", id: "kb.unembed", input: byCollection, seed: true},
		{name: "list", id: "kb.list", input: `{}`, seed: true},
		{name: "show", id: "kb.show", input: byCollection, seed: true},
		{name: "check", id: "kb.check", input: `{}`, seed: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newOpsRig(t)
			if tc.seed {
				seedSource(t, rig)
			}

			operationtest.RequireWritesOnlyInCommit(t, &operationtest.WriteProbe{
				Registry: rig.Registry, DB: rig.DB, ID: tc.id, Input: json.RawMessage(tc.input),
				Outside: func() string { return strconv.Itoa(rig.kb.Provider.Sent()) },
			})
		})
	}
}

func TestKnowledgeEmbedRepeatedSendsNothingAndChangesNothing(t *testing.T) {
	rig := newOpsRig(t)
	seedSource(t, rig)
	if _, err := rig.invoke(t, operation.SurfaceChat, "kb.embed", `{"collection":"support"}`); err != nil {
		t.Fatalf("first embed: %v", err)
	}
	before, sent := operationtest.Snapshot(t, rig.DB), rig.kb.Provider.Sent()

	again, err := rig.invoke(t, operation.SurfaceChat, "kb.embed", `{"collection":"support"}`)

	if err != nil || !strings.Contains(string(again.Data), `"sent":0`) {
		t.Fatalf("repeated embed = %s, %v, want nothing sent", again.Data, err)
	}
	if rig.kb.Provider.Sent() != sent || operationtest.Snapshot(t, rig.DB) != before {
		t.Error("a repeated embed sent text or changed the database")
	}
}

func TestKnowledgeOperationCards(t *testing.T) {
	t.Run("embed names the destination and the chunk count", func(t *testing.T) {
		rig := newOpsRig(t)
		seedSource(t, rig)

		outcome := rig.tool("kb.embed", `{"collection":"support"}`)

		if outcome.Class != toolbroker.ResultOK {
			t.Fatalf("outcome = %+v, want ok", outcome)
		}
		cards := rig.Prompts()
		if len(cards) != 1 {
			t.Fatalf("cards = %d, want 1", len(cards))
		}
		for _, want := range []string{"would send 2 of 2 chunks", "destination: embeddings.example.test:443 (text leaves this host)", "model test-model at 3 dimensions"} {
			if !strings.Contains(cards[0].Arguments, want) {
				t.Errorf("the card lacks %q:\n%s", want, cards[0].Arguments)
			}
		}
		if rig.kb.Provider.Sent() != 2 {
			t.Errorf("chunks sent = %d, want 2", rig.kb.Provider.Sent())
		}
	})
	t.Run("remove lists what it deletes", func(t *testing.T) {
		rig := newOpsRig(t)
		seedSource(t, rig)

		outcome := rig.tool("kb.remove", `{"collection":"support","source":"`+knowledgetest.PageURL+`"}`)

		if outcome.Class != toolbroker.ResultOK {
			t.Fatalf("outcome = %+v, want ok", outcome)
		}
		cards := rig.Prompts()
		if len(cards) != 1 || !strings.Contains(cards[0].Arguments, "Deletes:") || !strings.Contains(cards[0].Arguments, "would remove 2 chunks of the source "+knowledgetest.PageURL) {
			t.Fatalf("cards = %+v, want one listing the removal under Deletes:", cards)
		}
		if sources := rig.sources(t); len(sources) != 0 {
			t.Errorf("sources = %+v, want the source removed", sources)
		}
	})
}

func TestDestructiveApplyBindsPreviewDigest(t *testing.T) {
	rig := newOpsRig(t)
	seedSource(t, rig)
	input := `{"collection":"support","source":"` + knowledgetest.PageURL + `"}`
	owner := operation.Caller{Surface: operation.SurfaceChat, PrincipalID: operationtest.OwnerID, Class: principal.ClassOwner, SessionID: operationtest.SessionID, Direct: true}
	preview, err := rig.Registry.Preview(t.Context(), "kb.remove", &operation.Call{Caller: owner, Input: json.RawMessage(input)})
	if err != nil || preview.Digest == "" || len(preview.Lines) == 0 {
		t.Fatalf("Preview = %+v, %v", preview, err)
	}
	rig.kb.Site.Serve("text/markdown", "# Refunds\nRefunds are now issued within sixty days.\n\n# Shipping\nShipping takes five days.\n\n# Returns\nReturns are free.\n")
	prepared, err := rig.kb.Service.PrepareURL(t.Context(), knowledgetest.Collection, knowledgetest.PageURL)
	if err != nil {
		t.Fatalf("PrepareURL: %v", err)
	}
	if _, err := rig.kb.Service.Write(t.Context(), knowledge.ActionRefresh, prepared); err != nil {
		t.Fatalf("Write: %v", err)
	}

	_, err = rig.Registry.Invoke(t.Context(), "kb.remove", &operation.Call{
		Caller: owner, Input: json.RawMessage(input), IdempotencyKey: "remove-1", RequestDigest: operation.Digest(json.RawMessage(input)), PreviewDigest: preview.Digest,
	})

	if !fault.HasCode(err, operation.ErrorCodePreviewStale) {
		t.Fatalf("Invoke = %v, want %s", err, operation.ErrorCodePreviewStale)
	}
	if sources := rig.sources(t); len(sources) != 1 {
		t.Errorf("sources = %+v, want the source kept after the stale apply", sources)
	}
}
