//go:build linux

package knowledge_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/modules/knowledge"
	"github.com/anggasct/aura/internal/platform/operation"
	"github.com/anggasct/aura/internal/platform/operation/operationtest"
	"github.com/anggasct/aura/internal/platform/toolbroker"
)

func workspaceAddCases(documentBody string) []knowledgeAddCase {
	return []knowledgeAddCase{
		{
			name: "the agent reads a workspace path",
			run: func(t *testing.T, rig *opsRig) error {
				rig.writeWorkspaceFile(t, "docs/policy.md", documentBody)
				return requireOK(rig.tool("kb.add", `{"collection":"support","source":"docs/policy.md"}`))
			},
			wantURI:  func(*opsRig) string { return "workspace:docs/policy.md" },
			wantKind: knowledge.SourceKindFile,
		},
	}
}

func workspaceSourceRefusalCases(outside string) []agentSourceRefusalCase {
	return []agentSourceRefusalCase{
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
	}
}

func workspaceApprovedAddCases() []approvedAddCase {
	return []approvedAddCase{
		{name: "a workspace file", source: func(t *testing.T, rig *opsRig) string {
			rig.writeWorkspaceFile(t, "docs/policy.md", "# Policy\nRefunds are issued within thirty days.\n")
			return "docs/policy.md"
		}},
	}
}

func TestAnApprovedAddIsRefusedWhenTheFileChangedAfterThePreview(t *testing.T) {
	var rig *opsRig
	replace := func(context.Context, *toolbroker.ApprovalPrompt) (bool, error) {
		rig.writeWorkspaceFile(t, "docs/policy.md", "# Policy\nA different document than the one on the card.\n")
		return true, nil
	}
	rig = newOpsRigAnswering(t, replace)
	rig.writeWorkspaceFile(t, "docs/policy.md", "# Policy\nRefunds are issued within thirty days.\n")
	request := operationtest.RequestFor(operation.ToolName("kb.add"), json.RawMessage(`{"collection":"support","source":"docs/policy.md"}`), "stale-add")
	request.Tainted = true

	outcome := rig.Execute(t.Context(), request)

	if outcome.Class == toolbroker.ResultOK || outcome.Err == nil || !strings.Contains(outcome.Err.Error(), "operation_preview_stale") {
		t.Fatalf("outcome = %+v, want the add refused as stale", outcome)
	}
	if sources := rig.sources(t); len(sources) != 0 {
		t.Errorf("sources = %+v, want nothing stored", sources)
	}
	if actions := rig.kb.Ledger.Actions(); len(actions) != 0 {
		t.Errorf("owner actions = %+v, want none", actions)
	}
}

func TestRefreshWorksSourceBySourceOnEverySurface(t *testing.T) {
	cases := []struct {
		name         string
		run          func(t *testing.T, rig *opsRig) []byte
		wantHostFile string
		wantSkipped  bool
		wantPolicy   string
	}{
		{
			name: "the CLI refreshes the host file too",
			run: func(t *testing.T, rig *opsRig) []byte {
				result, err := rig.invoke(t, operation.SurfaceCLI, "kb.refresh", `{"collection":"support"}`)
				if err != nil {
					t.Fatalf("refresh: %v", err)
				}
				return result.Data
			},
			wantHostFile: "replaced",
			wantPolicy:   knowledge.StatusReplaced,
		},
		{
			name: "chat skips every file it cannot open",
			run: func(t *testing.T, rig *opsRig) []byte {
				result, err := rig.invoke(t, operation.SurfaceChat, "kb.refresh", `{"collection":"support"}`)
				if err != nil {
					t.Fatalf("refresh: %v", err)
				}
				return result.Data
			},
			wantSkipped: true,
		},
		{
			name: "the agent skips the host file",
			run: func(t *testing.T, rig *opsRig) []byte {
				outcome := rig.tool("kb.refresh", `{"collection":"support"}`)
				if err := requireOK(outcome); err != nil {
					t.Fatalf("refresh: %v", err)
				}
				return []byte(outcome.Output)
			},
			wantSkipped: true,
			wantPolicy:  knowledge.StatusReplaced,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newOpsRig(t)
			mixed := seedMixedCollection(t, rig, mixedFiles{host: true, workspace: true})
			chunksBefore := map[string]int{}
			for _, record := range rig.sources(t) {
				chunksBefore[record.URI] = record.Chunks
			}

			report := decodeReport(t, tc.run(t, rig))

			if got := report.statusOf("refunds"); got != knowledge.StatusUnchanged {
				t.Errorf("the reachable page = %q, want unchanged", got)
			}
			if got := report.statusOf("policy.md"); got != tc.wantPolicy {
				t.Errorf("the workspace file = %q, want %q", got, tc.wantPolicy)
			}
			if tc.wantPolicy == "" {
				if status, code := report.noteFor("policy.md"); status != "skipped" || code != string(operation.ErrorCodePolicyDenied) {
					t.Errorf("the workspace file note = %q %q, want a skip", status, code)
				}
			}
			if got := report.statusOf("notes.md"); got != tc.wantHostFile {
				t.Errorf("the host file = %q, want %q", got, tc.wantHostFile)
			}
			if status, code := report.noteFor("shipping"); status != "failed" || code == "" {
				t.Errorf("the dead page note = %q %q, want a failure with a code", status, code)
			}
			if status, _ := report.noteFor("faq.md"); status != "unchanged" {
				t.Errorf("the attachment note = %q, want unchanged", status)
			}
			if status, code := report.noteFor("notes.md"); tc.wantSkipped && (status != "skipped" || code != string(operation.ErrorCodePolicyDenied)) {
				t.Errorf("the host file note = %q %q, want a skip", status, code)
			}
			if !strings.Contains(report.Text, "refresh this source from the CLI") == tc.wantSkipped {
				t.Errorf("text %q, want the host skip reported = %v", report.Text, tc.wantSkipped)
			}
			for _, record := range rig.sources(t) {
				if record.Chunks == 0 || (record.URI == deadURL && record.Chunks != chunksBefore[deadURL]) {
					t.Errorf("source %s holds %d chunks, want the stored chunks kept", record.URI, record.Chunks)
				}
			}
			if _, err := os.Stat(mixed.hostFile); err != nil {
				t.Fatal(err)
			}
		})
	}
}
