//go:build !linux

package knowledge_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/modules/knowledge"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/operation"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/workspace"
)

func workspaceAddCases(string) []knowledgeAddCase {
	return nil
}

func workspaceSourceRefusalCases(outside string) []agentSourceRefusalCase {
	return []agentSourceRefusalCase{
		{
			name:      "an absolute host path",
			source:    func(*testing.T, *opsRig) string { return outside },
			wantClass: toolbroker.ResultExecutionFailed,
			wantCode:  "source_unavailable",
		},
		{
			name:      "a path climbing out of the workspace",
			source:    func(*testing.T, *opsRig) string { return "../outside-secret.md" },
			wantClass: toolbroker.ResultExecutionFailed,
			wantCode:  "source_unavailable",
		},
		{
			name: "a symlink leaving the workspace",
			source: func(t *testing.T, rig *opsRig) string {
				if err := os.Symlink(outside, filepath.Join(rig.workspace, "link.md")); err != nil {
					t.Fatal(err)
				}
				return "link.md"
			},
			wantClass: toolbroker.ResultExecutionFailed,
			wantCode:  "source_unavailable",
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
	return nil
}

func TestAgentWorkspaceAddFailsClosedWithoutLinux(t *testing.T) {
	rig := newOpsRig(t)
	rig.writeWorkspaceFile(t, "docs/policy.md", policyBefore)

	outcome := rig.tool("kb.add", `{"collection":"support","source":"docs/policy.md"}`)

	if outcome.Class != toolbroker.ResultExecutionFailed || outcome.Err == nil || !strings.Contains(outcome.Err.Error(), "source_unavailable") {
		t.Fatalf("outcome = %+v, want source_unavailable refusal", outcome)
	}
	if strings.Contains(outcome.Err.Error(), rig.workspace) {
		t.Errorf("error %q names the workspace path", outcome.Err)
	}
	_, err := rig.invoke(t, operation.SurfaceCLI, "kb.add", `{"collection":"support","source":"workspace:docs/policy.md"}`)
	if !fault.HasCode(err, operation.ErrorCodeSourceUnavailable) || !fault.HasCode(err, workspace.ErrorCodeUnsupported) {
		t.Fatalf("CLI workspace add = %v, want %s wrapping %s", err, operation.ErrorCodeSourceUnavailable, workspace.ErrorCodeUnsupported)
	}
	if sources := rig.sources(t); len(sources) != 0 {
		t.Errorf("sources = %+v, want nothing stored", sources)
	}
}

func TestRefreshWorksSourceBySourceWithoutLinuxWorkspace(t *testing.T) {
	cases := []struct {
		name         string
		run          func(t *testing.T, rig *opsRig) []byte
		wantHostFile string
		wantSkipped  bool
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
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newOpsRig(t)
			mixed := seedMixedCollection(t, rig, mixedFiles{host: true, workspace: false})
			chunksBefore := map[string]int{}
			for _, record := range rig.sources(t) {
				chunksBefore[record.URI] = record.Chunks
			}

			report := decodeReport(t, tc.run(t, rig))

			if got := report.statusOf("refunds"); got != knowledge.StatusUnchanged {
				t.Errorf("the reachable page = %q, want unchanged", got)
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
