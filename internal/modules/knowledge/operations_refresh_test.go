package knowledge_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/modules/knowledge"
	"github.com/anggasct/aura/internal/modules/knowledge/knowledgetest"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/operation"
	"github.com/anggasct/aura/internal/platform/operation/operationtest"
	"github.com/anggasct/aura/internal/platform/toolbroker"
)

const (
	deadURL      = "https://docs.example.test/shipping"
	policyBefore = "# Policy\nRefunds are issued within thirty days.\n"
	policyAfter  = "# Policy\nRefunds are issued within sixty days.\n"
	notesBefore  = "# Notes\nFirst version of the notes.\n"
	notesAfter   = "# Notes\nSecond version of the notes.\n"
)

type mixedCollection struct {
	hostFile   string
	attachment string
}

type refreshReport struct {
	Sources []struct {
		Status string `json:"status"`
		Label  string `json:"label"`
	} `json:"sources"`
	Notes []struct {
		Label  string `json:"label"`
		Status string `json:"status"`
		Code   string `json:"code"`
	} `json:"notes"`
	Text string `json:"text"`
}

type mixedFiles struct {
	host      bool
	workspace bool
}

func seedMixedCollection(t *testing.T, rig *opsRig, files mixedFiles) mixedCollection {
	t.Helper()
	seedSource(t, rig)
	prepared, err := rig.kb.Service.PrepareURL(t.Context(), knowledgetest.Collection, deadURL)
	if err != nil {
		t.Fatalf("PrepareURL: %v", err)
	}
	if _, err := rig.kb.Service.Write(t.Context(), knowledge.ActionAdd, prepared); err != nil {
		t.Fatalf("Write: %v", err)
	}
	wantSources := 3
	if files.workspace {
		wantSources++
		rig.writeWorkspaceFile(t, "docs/policy.md", policyBefore)
		if err := requireOK(rig.tool("kb.add", `{"collection":"support","source":"docs/policy.md"}`)); err != nil {
			t.Fatalf("add the workspace file: %v", err)
		}
	}
	mixed := mixedCollection{attachment: rig.attach(t, operationtest.SessionID, "faq.md", "text/markdown", "# Faq\nHow do refunds work?\n")}
	if err := requireOK(rig.tool("kb.add", `{"collection":"support","source":"artifact:`+mixed.attachment+`"}`)); err != nil {
		t.Fatalf("add the attachment: %v", err)
	}
	if files.host {
		wantSources++
		mixed.hostFile = filepath.Join(t.TempDir(), "notes.md")
		if err := os.WriteFile(mixed.hostFile, []byte(notesBefore), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := rig.invoke(t, operation.SurfaceCLI, "kb.add", `{"collection":"support","source":`+quote(mixed.hostFile)+`}`); err != nil {
			t.Fatalf("add the host file: %v", err)
		}
	}
	if got := len(rig.sources(t)); got != wantSources {
		t.Fatalf("seeded %d sources, want %d", got, wantSources)
	}
	if files.workspace {
		rig.writeWorkspaceFile(t, "docs/policy.md", policyAfter)
	}
	if files.host {
		if err := os.WriteFile(mixed.hostFile, []byte(notesAfter), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	rig.kb.Site.Withdraw(deadURL)
	return mixed
}

func (r *refreshReport) statusOf(label string) string {
	for i := range r.Sources {
		if strings.Contains(r.Sources[i].Label, label) {
			return r.Sources[i].Status
		}
	}
	return ""
}

func (r *refreshReport) noteFor(label string) (status, code string) {
	for i := range r.Notes {
		if strings.Contains(r.Notes[i].Label, label) {
			return r.Notes[i].Status, r.Notes[i].Code
		}
	}
	return "", ""
}

func decodeReport(t *testing.T, data []byte) *refreshReport {
	t.Helper()
	var report refreshReport
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatalf("decode %s: %v", data, err)
	}
	return &report
}

func TestRefreshReportsAStoredAttachmentUnchangedWithoutOpeningIt(t *testing.T) {
	rig := newOpsRig(t)
	id := rig.attach(t, operationtest.SessionID, "faq.md", "text/markdown", "# Faq\nHow do refunds work?\n")
	if err := requireOK(rig.tool("kb.add", `{"collection":"support","source":"artifact:`+id+`"}`)); err != nil {
		t.Fatalf("add: %v", err)
	}
	other := operationtest.RequestFor(operation.ToolName("kb.refresh"), json.RawMessage(`{"collection":"support"}`), "refresh-later-session")
	other.SessionID = otherSessionID
	if err := rig.artifacts.Unlink(t.Context(), id); err != nil {
		t.Fatalf("Unlink: %v", err)
	}

	outcome := rig.Execute(t.Context(), other)

	if err := requireOK(outcome); err != nil {
		t.Fatalf("refresh from a later session: %v", err)
	}
	report := decodeReport(t, []byte(outcome.Output))
	if status, _ := report.noteFor("faq.md"); status != "unchanged" || len(report.Sources) != 0 {
		t.Fatalf("report = %+v, want the attachment reported unchanged and nothing written", report)
	}
	if got := rig.sources(t); len(got) != 1 || got[0].Chunks == 0 {
		t.Errorf("sources = %+v, want the attachment chunks kept", got)
	}
}

func TestRefreshFailsOnlyWhenNoSourceCouldBeRefreshed(t *testing.T) {
	rig := newOpsRig(t)
	seedSource(t, rig)
	rig.kb.Site.Withdraw(knowledgetest.PageURL)
	actionsBefore := len(rig.kb.Ledger.Actions())

	_, err := rig.invoke(t, operation.SurfaceCLI, "kb.refresh", `{"collection":"support"}`)

	if !fault.HasCode(err, knowledge.ErrorCodeNothingRefreshed) {
		t.Fatalf("refresh = %v, want %s", err, knowledge.ErrorCodeNothingRefreshed)
	}
	if strings.Count(err.Error(), string(knowledge.ErrorCodeNothingRefreshed)) != 1 {
		t.Errorf("error %q prints the code more than once", err)
	}
	if got := rig.sources(t); len(got) != 1 || got[0].Chunks == 0 {
		t.Errorf("sources = %+v, want the stored chunks kept", got)
	}
	if len(rig.kb.Ledger.Actions()) != actionsBefore {
		t.Errorf("a refresh that refreshed nothing recorded an owner action")
	}
	outcome := rig.tool("kb.refresh", `{"collection":"support"}`)
	if outcome.Class != toolbroker.ResultExecutionFailed {
		t.Errorf("agent outcome = %+v, want a failure", outcome)
	}
}

func TestRefreshLeavesTheSameStateOnEverySurface(t *testing.T) {
	agent, cli, chat := newOpsRig(t), newOpsRig(t), newOpsRig(t)
	for _, rig := range []*opsRig{agent, cli, chat} {
		seedMixedCollection(t, rig, mixedFiles{})
	}

	if outcome := agent.tool("kb.refresh", `{"collection":"support"}`); outcome.Class != toolbroker.ResultOK {
		t.Fatalf("refresh through the broker = %+v, want ok", outcome)
	}
	if _, err := cli.invoke(t, operation.SurfaceCLI, "kb.refresh", `{"collection":"support"}`); err != nil {
		t.Fatalf("refresh on the CLI surface: %v", err)
	}
	if _, err := chat.invoke(t, operation.SurfaceChat, "kb.refresh", `{"collection":"support"}`); err != nil {
		t.Fatalf("refresh in chat: %v", err)
	}

	fromTool, fromCLI, fromChat := knowledgeState(t, agent.DB), knowledgeState(t, cli.DB), knowledgeState(t, chat.DB)
	if fromTool != fromCLI || fromTool != fromChat {
		t.Fatalf("the tool, the CLI and the chat left different state\ntool:\n%s\ncli:\n%s\nchat:\n%s", fromTool, fromCLI, fromChat)
	}
}
