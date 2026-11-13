//go:build linux

package app

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	kitapproval "github.com/anggasct/aura/internal/channels/kit/approval"
	"github.com/anggasct/aura/internal/config"
	toolsbuiltin "github.com/anggasct/aura/internal/modules/builtin"
	"github.com/anggasct/aura/internal/modules/github"
	"github.com/anggasct/aura/internal/modules/workflow"
	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/telemetry"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/toolbroker/toolbrokertest"
	"github.com/anggasct/aura/internal/store"
)

const (
	opsPersona   = "ops"
	lobbyPersona = "lobby"
	gitHubToken  = "AURA_TEST_GITHUB_TOKEN"
	trustedID    = "discord:111"
	publicID     = "discord:222"
	recordedTool = "record_call"
)

func surfacePersonas(cfg *config.Config) {
	cfg.Tools.GitHub.Credentials = []config.GitHubCredential{{Name: "ci", Ref: "env://" + gitHubToken}}
	withProbePersona(cfg)
	cfg.Agents.Definitions = append(cfg.Agents.Definitions,
		config.AgentDefinition{
			ID: opsPersona, Description: "d", Instructions: "i", Audience: config.AudienceTrusted,
			Tools: []string{"read_file", "write_file", github.ToolComment}, PrincipalTools: []string{"write_file", github.ToolComment},
		},
		config.AgentDefinition{
			ID: lobbyPersona, Description: "d", Instructions: "i", Audience: config.AudiencePublic,
			Tools: []string{"read_file", "write_file", github.ToolComment}, PrincipalTools: []string{"read_file"}, PublicTools: []string{"read_file"},
		},
	)
}

type gitHubCommentStub struct {
	comments atomic.Int64
}

func startGitHubCommentStub(t *testing.T) *gitHubCommentStub {
	t.Helper()
	stub := &gitHubCommentStub{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /repos/org/repo/issues/7/comments", func(w http.ResponseWriter, _ *http.Request) {
		stub.comments.Add(1)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":99,"html_url":"https://github.com/org/repo/issues/7#issuecomment-99"}`))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	t.Setenv(gitHubToken, "ghp-test-token")
	t.Setenv("AURA_TEST_GITHUB_BASE", server.URL)
	return stub
}

func (r *surfaceRig) pointGitHubAtTheStub(t *testing.T) {
	t.Helper()
	for _, name := range github.ToolNames() {
		r.executor.UnregisterTool(name, github.ToolVersion)
	}
	err := github.Register(r.executor, &github.ToolOptions{
		BaseURL: os.Getenv("AURA_TEST_GITHUB_BASE"), HTTPClient: &http.Client{},
		Credentials: []github.Credential{{Name: "ci", Ref: "env://" + gitHubToken}},
	})
	if err != nil {
		t.Fatalf("register the github tools on the stub: %v", err)
	}
}

type recordedGrants struct {
	mu       sync.Mutex
	grants   []approval.ApprovalGrant
	requests []toolbroker.ToolRequest
	calls    int
	fail     error
}

func (g *recordedGrants) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls
}

func (g *recordedGrants) granted() []approval.ApprovalGrant {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]approval.ApprovalGrant(nil), g.grants...)
}

func (g *recordedGrants) sent() []toolbroker.ToolRequest {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]toolbroker.ToolRequest(nil), g.requests...)
}

func (r *surfaceRig) registerRecordingStub(t *testing.T) *recordedGrants {
	t.Helper()
	recorded := &recordedGrants{}
	toolbrokertest.Register(t, r.runner.broker, toolbrokertest.Stub{
		Name: recordedTool, RequiresApproval: true,
		Run: func(_ context.Context, req *toolbroker.ToolRequest, _ approval.Constraints) (toolbroker.ToolResult, error) {
			recorded.mu.Lock()
			defer recorded.mu.Unlock()
			recorded.calls++
			recorded.requests = append(recorded.requests, *req)
			if req.Approval != nil {
				recorded.grants = append(recorded.grants, *req.Approval)
			}
			if recorded.fail != nil {
				return toolbroker.ToolResult{}, recorded.fail
			}
			return toolbroker.ToolResult{Output: json.RawMessage(`{"ok":true}`)}, nil
		},
	})
	return recorded
}

type surfaceOrigin struct {
	principal string
	class     principal.Class
	agent     string
}

var (
	ownerOrigin   = surfaceOrigin{principal: principal.OwnerID, class: principal.ClassOwner, agent: opsPersona}
	trustedOrigin = surfaceOrigin{principal: trustedID, class: principal.ClassTrusted, agent: opsPersona}
	publicOrigin  = surfaceOrigin{principal: publicID, class: principal.ClassPublic, agent: lobbyPersona}
)

func surfaceCall(runID string, origin surfaceOrigin, kind workflow.ApprovalKind) *workflow.StepCall {
	call := ownerStepCall(runID, "step-1", 0)
	call.Origin = workflow.RunOrigin{PrincipalID: origin.principal, PrincipalClass: string(origin.class), AgentID: origin.agent}
	call.Approval.Kind = kind
	if kind == workflow.ApprovalProposal {
		call.Approval.ProposalID = "prop-" + runID
	}
	return call
}

func (r *surfaceRig) seedProposal(t *testing.T, call *workflow.StepCall, claimedRun string) {
	t.Helper()
	now := time.Now().UTC()
	session := "proposal-session-" + call.RunID
	if err := store.NewSessionService(r.db).Create(t.Context(), &store.Session{ID: session, OwnerID: principal.OwnerID, AgentID: opsPersona, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("create proposal session: %v", err)
	}
	tool := "write_file"
	spec := &workflow.Spec{ID: "gen_" + call.Approval.ProposalID, Goal: "g", Version: 1, Source: workflow.SourceGenerated, Steps: []workflow.StepSpec{
		{ID: "act", Executor: workflow.ExecutorSpec{Kind: workflow.KindTool, ToolID: &tool}, Timeout: time.Second},
	}}
	proposal := &workflow.Proposal{
		ID: call.Approval.ProposalID, SessionID: session, AgentID: opsPersona, PrincipalID: principal.OwnerID,
		Tainted: true, Spec: spec, Digest: "d", PlanText: "plan", CreatedAt: now,
	}
	if err := workflow.NewStore(r.db).CreateProposal(t.Context(), proposal); err != nil {
		t.Fatalf("CreateProposal: %v", err)
	}
	if claimedRun != "" {
		if _, err := r.db.ExecContext(t.Context(), `UPDATE workflow_proposal SET run_id = ? WHERE id = ?`, claimedRun, proposal.ID); err != nil {
			t.Fatalf("claim the proposal: %v", err)
		}
	}
}

func (r *surfaceRig) standingGrantRows(t *testing.T) int {
	t.Helper()
	var count int
	if err := r.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM standing_grant`).Scan(&count); err != nil {
		t.Fatalf("count standing grants: %v", err)
	}
	return count
}

func fileExists(t *testing.T, workspace, name string) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(workspace, name))
	return err == nil
}

func commentArgs() json.RawMessage {
	return json.RawMessage(`{"repo":"org/repo","number":7,"body":"hi","credential_name":"ci"}`)
}

func (o *workflowOwnerCards) asked() []workflowCard {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]workflowCard(nil), o.cards...)
}

func (l *observationLog) all() []toolbroker.Observation {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]toolbroker.Observation(nil), l.seen...)
}

func (l *observationLog) last() toolbroker.Observation {
	observations := l.all()
	if len(observations) == 0 {
		return toolbroker.Observation{}
	}
	return observations[len(observations)-1]
}

func codeIs(t *testing.T, err error, want workflow.ErrorCode) {
	t.Helper()
	if code, ok := workflow.CodeOf(err); !ok || code != want {
		t.Fatalf("error = %v, want %s", err, want)
	}
}

func TestWorkflowAttachRule(t *testing.T) {
	const (
		attached = "attached"
		card     = "card"
		plain    = "plain"
		refused  = "refused"
	)
	stub := startGitHubCommentStub(t)
	owner := &workflowOwnerCards{}
	rig := newSurfaceRig(t, &surfaceRigOptions{configure: surfacePersonas, owner: owner})
	rig.pointGitHubAtTheStub(t)
	if err := os.WriteFile(filepath.Join(rig.workspace, "seed.txt"), []byte("seed"), 0o600); err != nil {
		t.Fatalf("seed the workspace: %v", err)
	}

	tests := []struct {
		name          string
		origin        surfaceOrigin
		originTainted bool
		approvalTaint bool
		kind          workflow.ApprovalKind
		tool          string
		args          json.RawMessage
		claimed       bool
		want          string
		wantCode      string

		wantWorkflowCode workflow.ErrorCode
	}{
		{name: "owner installed write_file", origin: ownerOrigin, kind: workflow.ApprovalInstalled, tool: "write_file", want: attached},
		{name: "owner proposal write_file", origin: ownerOrigin, kind: workflow.ApprovalProposal, tool: "write_file", want: attached},
		{name: "owner routine write_file", origin: ownerOrigin, kind: workflow.ApprovalRoutine, tool: "write_file", want: attached},
		{name: "owner installed github_comment", origin: ownerOrigin, kind: workflow.ApprovalInstalled, tool: github.ToolComment, want: attached},
		{name: "owner proposal github_comment", origin: ownerOrigin, kind: workflow.ApprovalProposal, tool: github.ToolComment, want: attached},
		{name: "owner routine github_comment", origin: ownerOrigin, kind: workflow.ApprovalRoutine, tool: github.ToolComment, want: attached},
		{name: "trusted installed write_file in its principal_tools", origin: trustedOrigin, kind: workflow.ApprovalInstalled, tool: "write_file", want: attached},
		{name: "trusted proposal write_file in its principal_tools", origin: trustedOrigin, kind: workflow.ApprovalProposal, tool: "write_file", want: attached},
		{name: "trusted routine write_file in its principal_tools", origin: trustedOrigin, kind: workflow.ApprovalRoutine, tool: "write_file", want: attached},
		{name: "trusted installed github_comment", origin: trustedOrigin, kind: workflow.ApprovalInstalled, tool: github.ToolComment, want: card},
		{name: "trusted proposal github_comment", origin: trustedOrigin, kind: workflow.ApprovalProposal, tool: github.ToolComment, want: card},
		{name: "trusted routine github_comment", origin: trustedOrigin, kind: workflow.ApprovalRoutine, tool: github.ToolComment, want: card},
		{name: "trusted write_file with invalid arguments", origin: trustedOrigin, kind: workflow.ApprovalInstalled, tool: "write_file", args: json.RawMessage(`{"path":""}`), want: refused, wantCode: string(toolbroker.ResultInvalidArgument)},
		{name: "public installed write_file", origin: publicOrigin, kind: workflow.ApprovalInstalled, tool: "write_file", want: refused, wantWorkflowCode: workflow.ErrorCodeCapabilityExceeded},
		{name: "public installed read_file", origin: publicOrigin, kind: workflow.ApprovalInstalled, tool: "read_file", args: json.RawMessage(`{"path":"seed.txt"}`), want: plain},
		{name: "public proposal read_file", origin: publicOrigin, kind: workflow.ApprovalProposal, tool: "read_file", args: json.RawMessage(`{"path":"seed.txt"}`), want: plain},
		{name: "owner class under another principal id", origin: surfaceOrigin{principal: trustedID, class: principal.ClassOwner, agent: opsPersona}, kind: workflow.ApprovalInstalled, tool: github.ToolComment, want: card},
		{name: "owner id under the trusted class", origin: surfaceOrigin{principal: principal.OwnerID, class: principal.ClassTrusted, agent: opsPersona}, kind: workflow.ApprovalInstalled, tool: github.ToolComment, want: card},
		{name: "owner tainted origin installed", origin: ownerOrigin, originTainted: true, kind: workflow.ApprovalInstalled, tool: "write_file", want: card},
		{name: "owner tainted approval installed", origin: ownerOrigin, approvalTaint: true, kind: workflow.ApprovalInstalled, tool: "write_file", want: card},
		{name: "owner tainted re-run of an untainted proposal", origin: ownerOrigin, originTainted: true, kind: workflow.ApprovalProposal, tool: "write_file", want: card},
		{name: "owner tainted routine record", origin: ownerOrigin, originTainted: true, approvalTaint: true, kind: workflow.ApprovalRoutine, tool: "write_file", want: card},
		{name: "owner tainted proposal in its claimed run", origin: ownerOrigin, originTainted: true, approvalTaint: true, kind: workflow.ApprovalProposal, tool: "write_file", claimed: true, want: attached},
		{name: "owner tainted proposal claimed by another run", origin: ownerOrigin, originTainted: true, approvalTaint: true, kind: workflow.ApprovalProposal, tool: "write_file", want: refused, wantWorkflowCode: workflow.ErrorCodeApprovalSpent},
		{name: "trusted tainted write_file", origin: trustedOrigin, originTainted: true, kind: workflow.ApprovalInstalled, tool: "write_file", want: card},
	}
	for index, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			runID := "run-attach-" + strconv.Itoa(index)
			call := surfaceCall(runID, tc.origin, tc.kind)
			call.Origin.Tainted, call.Approval.Tainted = tc.originTainted, tc.approvalTaint
			if tc.kind == workflow.ApprovalProposal && tc.approvalTaint {
				claimed := ""
				if tc.claimed {
					claimed = runID
				}
				rig.seedProposal(t, call, claimed)
			}
			args := tc.args
			if args == nil {
				args = writeFileArgs(t, runID+".txt", "content")
				if tc.tool == github.ToolComment {
					args = commentArgs()
				}
			}
			deciderBefore, cardsBefore, commentsBefore := rig.deciderCalls.Load(), len(owner.asked()), stub.comments.Load()
			observedBefore := len(rig.observed.all())
			_, err := rig.runner.Invoke(stepContextFor(t, rig.db, call), tc.tool, args)
			decided, cards := rig.deciderCalls.Load()-deciderBefore, len(owner.asked())-cardsBefore
			switch tc.want {
			case attached:
				if err != nil {
					t.Fatalf("Invoke: %v", err)
				}
				if decided != 0 || cards != 0 {
					t.Fatalf("decider asked %d times and owner %d times, want neither", decided, cards)
				}
				if got := rig.observed.last().Approval; got != toolbroker.ApprovalAttached {
					t.Fatalf("approval state = %q, want %q", got, toolbroker.ApprovalAttached)
				}
			case card:
				if err != nil {
					t.Fatalf("Invoke: %v", err)
				}
				if decided != 1 || cards != 1 {
					t.Fatalf("decider asked %d times and owner %d times, want one each", decided, cards)
				}
				if got := rig.observed.last().Approval; got != toolbroker.ApprovalApproved {
					t.Fatalf("approval state = %q, want %q", got, toolbroker.ApprovalApproved)
				}
			case plain:
				if err != nil {
					t.Fatalf("Invoke: %v", err)
				}
				if decided != 0 || cards != 0 {
					t.Fatalf("decider asked %d times and owner %d times, want neither", decided, cards)
				}
				if got := rig.observed.last().Approval; got != toolbroker.ApprovalAuto {
					t.Fatalf("approval state = %q, want %q: a public call is never attached", got, toolbroker.ApprovalAuto)
				}
			case refused:
				if tc.wantWorkflowCode != "" {
					codeIs(t, err, tc.wantWorkflowCode)
				} else if code, ok := toolbroker.CodeOf(err); !ok || string(code) != tc.wantCode {
					t.Fatalf("Invoke error = %v, want %s", err, tc.wantCode)
				}
				if cards != 0 {
					t.Fatalf("owner asked %d times for a refused call, want 0", cards)
				}
				if tc.wantCode == string(toolbroker.ResultInvalidArgument) && len(rig.observed.all()) != observedBefore {
					t.Fatal("the broker ran a call whose arguments are invalid")
				}
			}
			if tc.want == refused || tc.want == plain {
				return
			}
			if tc.tool == github.ToolComment && stub.comments.Load()-commentsBefore != 1 {
				t.Fatalf("github comments performed = %d, want 1", stub.comments.Load()-commentsBefore)
			}
			if tc.tool == "write_file" && !fileExists(t, rig.workspace, runID+".txt") {
				t.Fatal("the approved write was not performed")
			}
		})
	}
}

func TestApprovedProposalStepRunsOnItsAttachedGrant(t *testing.T) {
	owner := &workflowOwnerCards{}
	rig := newSurfaceRig(t, &surfaceRigOptions{configure: surfacePersonas, owner: owner})
	call := surfaceCall("run-proposal", ownerOrigin, workflow.ApprovalProposal)
	if _, err := rig.runner.Invoke(stepContextFor(t, rig.db, call), "write_file", writeFileArgs(t, "plan.txt", "approved")); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !fileExists(t, rig.workspace, "plan.txt") {
		t.Fatal("the approved proposal step wrote nothing")
	}
	if got := rig.deciderCalls.Load(); got != 0 {
		t.Fatalf("decider asked %d times, want 0", got)
	}
	if got := rig.observed.last().Approval; got != toolbroker.ApprovalAttached {
		t.Fatalf("approval state = %q, want %q", got, toolbroker.ApprovalAttached)
	}
	if rows := rig.standingGrantRows(t); rows != 0 {
		t.Fatalf("standing_grant rows = %d, want 0", rows)
	}
}

func TestOwnerRoutineStepRunsOnItsAttachedGrant(t *testing.T) {
	owner := &workflowOwnerCards{}
	rig := newSurfaceRig(t, &surfaceRigOptions{configure: surfacePersonas, owner: owner})

	untainted := surfaceCall("run-routine", ownerOrigin, workflow.ApprovalRoutine)
	if _, err := rig.runner.Invoke(stepContextFor(t, rig.db, untainted), "write_file", writeFileArgs(t, "routine.txt", "weekly")); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if got := rig.observed.last().Approval; got != toolbroker.ApprovalAttached || rig.deciderCalls.Load() != 0 || len(owner.asked()) != 0 {
		t.Fatalf("untainted routine: approval %q, decider %d, cards %d; want attached with no question", got, rig.deciderCalls.Load(), len(owner.asked()))
	}

	legacy := surfaceCall("run-legacy-routine", ownerOrigin, workflow.ApprovalRoutine)
	legacy.Approval.Tainted, legacy.Origin.Tainted = true, true
	if _, err := rig.runner.Invoke(stepContextFor(t, rig.db, legacy), "write_file", writeFileArgs(t, "legacy.txt", "weekly")); err != nil {
		t.Fatalf("Invoke legacy routine: %v", err)
	}
	if cards := owner.asked(); len(cards) != 1 || cards[0].prompt.ReasonCode != approval.ReasonUntrustedInput {
		t.Fatalf("legacy tainted routine cards = %+v, want one with the untrusted-input reason", cards)
	}
}

func TestNonOwnerInstalledWriteFileIsAttached(t *testing.T) {
	owner := &workflowOwnerCards{}
	rig := newSurfaceRig(t, &surfaceRigOptions{configure: surfacePersonas, owner: owner})
	call := surfaceCall("run-delegated", trustedOrigin, workflow.ApprovalInstalled)
	if _, err := rig.runner.Invoke(stepContextFor(t, rig.db, call), "write_file", writeFileArgs(t, "delegated.txt", "by a trusted start")); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !fileExists(t, rig.workspace, "delegated.txt") {
		t.Fatal("the delegated start wrote nothing")
	}
	if got := rig.deciderCalls.Load(); got != 0 {
		t.Fatalf("decider asked %d times, want 0", got)
	}
}

func TestDelegatedInstalledGitHubStepAsksTheOwnerPerCall(t *testing.T) {
	stub := startGitHubCommentStub(t)
	owner := &workflowOwnerCards{}
	rig := newSurfaceRig(t, &surfaceRigOptions{configure: surfacePersonas, owner: owner})
	rig.pointGitHubAtTheStub(t)
	wantText, err := rig.runner.broker.ExactCallText(github.ToolComment, commentArgs())
	if err != nil {
		t.Fatalf("ExactCallText: %v", err)
	}

	t.Run("an approved card performs the effect once", func(t *testing.T) {
		call := surfaceCall("run-card-approved", trustedOrigin, workflow.ApprovalInstalled)
		if _, err := rig.runner.Invoke(stepContextFor(t, rig.db, call), github.ToolComment, commentArgs()); err != nil {
			t.Fatalf("Invoke: %v", err)
		}
		cards := owner.asked()
		if len(cards) != 1 {
			t.Fatalf("owner asked %d times, want once", len(cards))
		}
		if cards[0].prompt.Arguments != wantText {
			t.Fatalf("card text = %q, want the exact call text %q", cards[0].prompt.Arguments, wantText)
		}
		wantRequester := kitapproval.Requester{Principal: trustedID, Class: principal.ClassTrusted, Persona: opsPersona}
		if cards[0].requester != wantRequester {
			t.Fatalf("card requester = %+v, want %+v", cards[0].requester, wantRequester)
		}
		if cards[0].prompt.StandingGrantAllowed {
			t.Fatal("a workflow card offers a standing grant")
		}
		if got := stub.comments.Load(); got != 1 {
			t.Fatalf("github comments = %d, want 1", got)
		}
		if rig.route.HoldsApprovalTarget(workflow.RunSessionID("run-card-approved")) {
			t.Fatal("the run session is still held after the call")
		}
	})

	t.Run("a rejected card fails the step with no effect", func(t *testing.T) {
		owner.mu.Lock()
		owner.answer = func(*kitapproval.Prompt) bool { return false }
		owner.mu.Unlock()
		before := stub.comments.Load()
		call := surfaceCall("run-card-rejected", trustedOrigin, workflow.ApprovalInstalled)
		_, err := rig.runner.Invoke(stepContextFor(t, rig.db, call), github.ToolComment, commentArgs())
		codeIs(t, err, workflow.ErrorCodeApprovalRejected)
		if stub.comments.Load() != before {
			t.Fatal("a rejected call performed its effect")
		}
	})

	t.Run("the same definition started by the owner runs with no card", func(t *testing.T) {
		cardsBefore, before := len(owner.asked()), stub.comments.Load()
		call := surfaceCall("run-owner-started", ownerOrigin, workflow.ApprovalInstalled)
		if _, err := rig.runner.Invoke(stepContextFor(t, rig.db, call), github.ToolComment, commentArgs()); err != nil {
			t.Fatalf("Invoke: %v", err)
		}
		if len(owner.asked()) != cardsBefore || stub.comments.Load() != before+1 {
			t.Fatalf("owner-started call: cards %d -> %d, comments %d -> %d; want no card and one effect", cardsBefore, len(owner.asked()), before, stub.comments.Load())
		}
	})
}

func TestTaintedReRunOfAnUntaintedProposalAsksPerCall(t *testing.T) {
	asProposal := func(runID string) *workflow.StepCall {
		call := surfaceCall(runID, ownerOrigin, workflow.ApprovalProposal)
		call.Origin.Tainted = true
		return call
	}

	t.Run("an owner approver gets a card with the untrusted-input reason", func(t *testing.T) {
		owner := &workflowOwnerCards{}
		rig := newSurfaceRig(t, &surfaceRigOptions{configure: surfacePersonas, owner: owner})
		if _, err := rig.runner.Invoke(stepContextFor(t, rig.db, asProposal("run-rerun")), "write_file", writeFileArgs(t, "rerun.txt", "again")); err != nil {
			t.Fatalf("Invoke: %v", err)
		}
		cards := owner.asked()
		if len(cards) != 1 || cards[0].prompt.ReasonCode != approval.ReasonUntrustedInput {
			t.Fatalf("cards = %+v, want one with the untrusted-input reason", cards)
		}
	})

	t.Run("no owner approver refuses the call at the broker and shows no card", func(t *testing.T) {
		rig := newSurfaceRig(t, &surfaceRigOptions{configure: surfacePersonas})
		_, err := rig.runner.Invoke(stepContextFor(t, rig.db, asProposal("run-rerun-bare")), "write_file", writeFileArgs(t, "bare.txt", "again"))
		codeIs(t, err, workflow.ErrorCodeApprovalRejected)
		if !strings.Contains(err.Error(), "nobody can be asked") {
			t.Fatalf("error = %v, want the broker's untrusted-input refusal", err)
		}
		if got := rig.deciderCalls.Load(); got != 0 {
			t.Fatalf("decider asked %d times, want 0", got)
		}
		if fileExists(t, rig.workspace, "bare.txt") {
			t.Fatal("the refused call wrote its file")
		}
	})
}

func TestTaintedProposalsOwnRunIsAttached(t *testing.T) {
	owner := &workflowOwnerCards{}
	rig := newSurfaceRig(t, &surfaceRigOptions{configure: surfacePersonas, owner: owner})
	recorded := rig.registerRecordingStub(t)

	own := surfaceCall("run-own", ownerOrigin, workflow.ApprovalProposal)
	own.Origin.AgentID = probePersona
	own.Origin.Tainted, own.Approval.Tainted = true, true
	rig.seedProposal(t, own, "run-own")
	if _, err := rig.runner.Invoke(stepContextFor(t, rig.db, own), recordedTool, json.RawMessage(`{"n":1}`)); err != nil {
		t.Fatalf("Invoke in the claimed run: %v", err)
	}
	if recorded.count() != 1 || len(recorded.granted()) != 1 || rig.deciderCalls.Load() != 0 {
		t.Fatalf("claimed run: adapter %d, grants %d, decider %d; want one attached call and no question", recorded.count(), len(recorded.granted()), rig.deciderCalls.Load())
	}

	other := surfaceCall("run-other", ownerOrigin, workflow.ApprovalProposal)
	other.Origin.AgentID = probePersona
	other.Origin.Tainted, other.Approval.Tainted = true, true
	other.Approval.ProposalID = own.Approval.ProposalID
	_, err := rig.runner.Invoke(stepContextFor(t, rig.db, other), recordedTool, json.RawMessage(`{"n":2}`))
	codeIs(t, err, workflow.ErrorCodeApprovalSpent)
	if recorded.count() != 1 || rig.deciderCalls.Load() != 0 || len(owner.asked()) != 0 {
		t.Fatalf("another run: adapter %d, decider %d, cards %d; want the adapter still at 1 and no question", recorded.count(), rig.deciderCalls.Load(), len(owner.asked()))
	}

	unclaimed := surfaceCall("run-unclaimed", ownerOrigin, workflow.ApprovalProposal)
	unclaimed.Origin.AgentID = probePersona
	unclaimed.Origin.Tainted, unclaimed.Approval.Tainted = true, true
	rig.seedProposal(t, unclaimed, "")
	_, err = rig.runner.Invoke(stepContextFor(t, rig.db, unclaimed), recordedTool, json.RawMessage(`{"n":3}`))
	codeIs(t, err, workflow.ErrorCodeApprovalSpent)

	missing := surfaceCall("run-missing", ownerOrigin, workflow.ApprovalProposal)
	missing.Origin.AgentID = probePersona
	missing.Origin.Tainted, missing.Approval.Tainted = true, true
	_, err = rig.runner.Invoke(stepContextFor(t, rig.db, missing), recordedTool, json.RawMessage(`{"n":4}`))
	codeIs(t, err, workflow.ErrorCodeApprovalSpent)
	if recorded.count() != 1 {
		t.Fatalf("adapter ran %d times in total, want 1", recorded.count())
	}
}

func TestAttachedGrantIsBoundToTheStepAttempt(t *testing.T) {
	rig := newSurfaceRig(t, &surfaceRigOptions{configure: surfacePersonas})
	recorded := rig.registerRecordingStub(t)
	args := json.RawMessage(`{"n":1}`)

	var deadlines []time.Time
	for attempt := range 2 {
		call := surfaceCall("run-attempts", ownerOrigin, workflow.ApprovalInstalled)
		call.Origin.AgentID = probePersona
		call.Attempt = attempt
		ctx := stepContextFor(t, rig.db, call)
		deadline, _ := ctx.Deadline()
		deadlines = append(deadlines, deadline)
		if _, err := rig.runner.Invoke(ctx, recordedTool, args); err != nil {
			t.Fatalf("Invoke attempt %d: %v", attempt, err)
		}
	}
	grants := recorded.granted()
	if len(grants) != 2 {
		t.Fatalf("recorded grants = %d, want 2", len(grants))
	}
	for index, grant := range grants {
		if !grant.ExpiresAt.Equal(deadlines[index]) {
			t.Fatalf("grant %d expires at %v, want the step deadline %v", index, grant.ExpiresAt, deadlines[index])
		}
	}
	if grants[0].Nonce == grants[1].Nonce {
		t.Fatal("two attempts reused one grant nonce")
	}
	replay := recorded.sent()[0]
	replay.Approval = &grants[0]
	_, err := rig.runner.broker.Execute(t.Context(), &replay)
	if err == nil {
		t.Fatal("a consumed grant was accepted a second time")
	}
	if recorded.count() != 2 {
		t.Fatalf("adapter ran %d times, want 2", recorded.count())
	}
}

func TestUnshowableCallIsNotGranted(t *testing.T) {
	rig := newSurfaceRig(t, &surfaceRigOptions{configure: surfacePersonas})
	const secret = "s3cr3t-registered-later"
	rig.runner.broker.RegisterSecret(secret)
	call := surfaceCall("run-unshowable", ownerOrigin, workflow.ApprovalProposal)
	before := len(rig.observed.all())
	_, err := rig.runner.Invoke(stepContextFor(t, rig.db, call), "write_file", writeFileArgs(t, "leak.txt", "token "+secret))
	codeIs(t, err, workflow.ErrorCodePlanUnshowable)
	if len(rig.observed.all()) != before {
		t.Fatal("the broker was called for a call that cannot be shown")
	}
	if fileExists(t, rig.workspace, "leak.txt") {
		t.Fatal("the unshowable call wrote its file")
	}
}

func TestForgedStepCallIsRefused(t *testing.T) {
	rig := newSurfaceRig(t, &surfaceRigOptions{configure: surfacePersonas})
	recorded := rig.registerRecordingStub(t)
	tests := []struct {
		name  string
		forge func(call *workflow.StepCall)
	}{
		{name: "a known kind with an empty digest", forge: func(call *workflow.StepCall) {
			call.Approval.Digest, call.DefinitionDigest = "", ""
		}},
		{name: "a definition version of zero", forge: func(call *workflow.StepCall) {
			call.Approval.DefinitionVersion, call.DefinitionVersion = 0, 0
		}},
		{name: "an empty definition id", forge: func(call *workflow.StepCall) {
			call.Approval.DefinitionID, call.DefinitionID = "", ""
		}},
		{name: "a digest that differs from the run's", forge: func(call *workflow.StepCall) {
			call.Approval.Digest = "another-digest"
		}},
		{name: "a definition id that differs from the run's", forge: func(call *workflow.StepCall) {
			call.Approval.DefinitionID = "another-definition"
		}},
		{name: "a definition version that differs from the run's", forge: func(call *workflow.StepCall) {
			call.Approval.DefinitionVersion = 2
		}},
	}
	for index, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			call := surfaceCall("run-forged-"+strconv.Itoa(index), ownerOrigin, workflow.ApprovalInstalled)
			call.Origin.AgentID = probePersona
			tc.forge(call)
			adapterBefore, deciderBefore := recorded.count(), rig.deciderCalls.Load()
			_, err := rig.runner.Invoke(stepContextFor(t, rig.db, call), recordedTool, json.RawMessage(`{"n":1}`))
			codeIs(t, err, workflow.ErrorCodeApprovalMissing)
			if recorded.count() != adapterBefore || rig.deciderCalls.Load() != deciderBefore {
				t.Fatalf("adapter %d, decider %d; want both unchanged", recorded.count()-adapterBefore, rig.deciderCalls.Load()-deciderBefore)
			}
		})
	}
}

type driveStep struct {
	id    string
	tool  string
	args  json.RawMessage
	retry int
}

func driveToolRun(t *testing.T, rig *surfaceRig, id string, origin *workflow.RunOrigin, step *driveStep) []*workflow.StepRun {
	t.Helper()
	disk := workflow.NewStore(rig.db)
	fake := durable.NewFake()
	interpreter := workflow.NewInterpreter(disk, fake, &workflow.Options{Tools: rig.runner, Logger: slog.New(slog.DiscardHandler)})
	tool := step.tool
	spec := &workflow.Spec{
		ID: id, Goal: "Drive a tool step", Version: 1, Source: workflow.SourceDefined,
		Steps: []workflow.StepSpec{
			{ID: "approve", Timeout: 5 * time.Second, Executor: workflow.ExecutorSpec{Kind: workflow.KindApproval}},
			{
				ID: step.id, DependsOn: []string{"approve"}, Timeout: 2 * time.Minute,
				Executor: workflow.ExecutorSpec{Kind: workflow.KindTool, ToolID: &tool, ToolArgs: step.args},
				Retry:    workflow.RetryPolicy{Attempts: step.retry, Backoff: time.Millisecond},
			},
		},
	}
	deps := &workflow.ValidationDeps{
		Agents:         rig.registry,
		KnownTools:     append(toolsbuiltin.DefinitionNames(), recordedTool),
		EffectfulTools: toolsbuiltin.EffectfulToolNames(),
		CheckToolCall:  acceptToolCall,
		StepAllowed:    acceptStepAllowed,
	}
	if err := interpreter.Install(t.Context(), []*workflow.Spec{spec}, deps); err != nil {
		t.Fatalf("Install: %v", err)
	}
	summary, err := interpreter.Start(t.Context(), id, nil, origin)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor := func(status string) *workflow.RunSummary {
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if run, err := disk.Run(t.Context(), summary.ID); err == nil && run.Status == status {
				return run
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("run %s never reached %s", summary.ID, status)
		return nil
	}
	run := waitFor(workflow.RunSuspended)
	if err := fake.Signal(t.Context(), durable.RunRef{Key: run.DurableKey}, "approval.approve", []byte(`{"decision":"approve"}`)); err != nil {
		t.Fatalf("Signal approve: %v", err)
	}
	waitFor(workflow.RunFailed)
	steps, err := disk.Steps(t.Context(), summary.ID)
	if err != nil {
		t.Fatalf("Steps: %v", err)
	}
	return steps
}

func stepRunOf(t *testing.T, steps []*workflow.StepRun, id string) *workflow.StepRun {
	t.Helper()
	for _, step := range steps {
		if step.StepID == id {
			return step
		}
	}
	t.Fatalf("no step %s in %+v", id, steps)
	return nil
}

func TestARejectedWorkflowCallIsNotAskedAgain(t *testing.T) {
	t.Run("an owner who says no is asked once", func(t *testing.T) {
		owner := &workflowOwnerCards{answer: func(*kitapproval.Prompt) bool { return false }}
		rig := newSurfaceRig(t, &surfaceRigOptions{configure: surfacePersonas, owner: owner})
		origin := &workflow.RunOrigin{PrincipalID: principal.OwnerID, PrincipalClass: string(principal.ClassOwner), AgentID: opsPersona, Tainted: true}
		steps := driveToolRun(t, rig, "rejected-once", origin, &driveStep{id: "write", tool: "write_file", args: writeFileArgs(t, "no.txt", "no"), retry: 2})
		write := stepRunOf(t, steps, "write")
		if write.ErrorCode != string(workflow.ErrorCodeApprovalRejected) || write.Attempt != 1 {
			t.Fatalf("write step = code %q after %d attempts, want %s after 1", write.ErrorCode, write.Attempt, workflow.ErrorCodeApprovalRejected)
		}
		if cards := len(owner.asked()); cards != 1 {
			t.Fatalf("owner asked %d times, want once", cards)
		}
		if fileExists(t, rig.workspace, "no.txt") {
			t.Fatal("the rejected call wrote its file")
		}
	})

	t.Run("an execution failure is still retried", func(t *testing.T) {
		rig := newSurfaceRig(t, &surfaceRigOptions{configure: surfacePersonas})
		recorded := rig.registerRecordingStub(t)
		recorded.fail = os.ErrInvalid
		origin := &workflow.RunOrigin{PrincipalID: principal.OwnerID, PrincipalClass: string(principal.ClassOwner), AgentID: probePersona}
		steps := driveToolRun(t, rig, "failing-retried", origin, &driveStep{id: "record", tool: recordedTool, args: json.RawMessage(`{"n":1}`), retry: 2})
		record := stepRunOf(t, steps, "record")
		if record.Attempt != 3 || recorded.count() != 3 {
			t.Fatalf("record step ran %d attempts and the adapter %d times, want 3 each", record.Attempt, recorded.count())
		}
	})
}

func TestWorkflowEmailSendIsRefusedBeforeTheBroker(t *testing.T) {
	owner := &workflowOwnerCards{}
	rig := newSurfaceRig(t, &surfaceRigOptions{configure: surfacePersonas, owner: owner})
	args := json.RawMessage(`{"to":"a@mail.example","subject":"s","body":"b"}`)
	for _, origin := range []surfaceOrigin{ownerOrigin, trustedOrigin} {
		t.Run(string(origin.class), func(t *testing.T) {
			call := surfaceCall("run-email-"+string(origin.class), origin, workflow.ApprovalInstalled)
			_, err := rig.runner.Invoke(stepContextFor(t, rig.db, call), toolsbuiltin.EmailSendToolName, args)
			codeIs(t, err, workflow.ErrorCodeCapabilityUnavailable)
			if got := len(rig.observed.all()); got != 0 {
				t.Fatalf("the broker saw %d calls, want none", got)
			}
			if rig.deciderCalls.Load() != 0 || len(owner.asked()) != 0 {
				t.Fatalf("decider %d, owner %d; want neither asked", rig.deciderCalls.Load(), len(owner.asked()))
			}
		})
	}
}

func TestWorkflowStepInsideADurableTurnIsRefused(t *testing.T) {
	rig := newSurfaceRig(t, &surfaceRigOptions{configure: surfacePersonas})
	recorded := rig.registerRecordingStub(t)
	call := surfaceCall("run-scoped", ownerOrigin, workflow.ApprovalInstalled)
	call.Origin.AgentID = probePersona
	stepCtx := stepContextFor(t, rig.db, call)

	fake := durable.NewFake()
	done := make(chan error, 1)
	fake.RegisterHandler("scoped", func(ctx context.Context, inv durable.Invocation) error {
		scoped := durable.WithTurnScope(stepCtx, durable.NewTurnScope(inv))
		_, err := rig.runner.Invoke(scoped, recordedTool, json.RawMessage(`{"n":1}`))
		done <- err
		return nil
	})
	if _, err := fake.Start(t.Context(), durable.StartRequest{Handler: "scoped", Key: "scoped-key", Payload: []byte(`{}`)}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	select {
	case err := <-done:
		codeIs(t, err, workflow.ErrorCodeExecutorInvalid)
	case <-time.After(5 * time.Second):
		t.Fatal("the scoped invocation never ran")
	}
	if recorded.count() != 0 {
		t.Fatalf("adapter ran %d times inside a durable turn, want 0", recorded.count())
	}
}

func TestWorkflowToolCallsAreObserved(t *testing.T) {
	rig := newSurfaceRig(t, &surfaceRigOptions{configure: surfacePersonas})
	call := surfaceCall("run-observed", ownerOrigin, workflow.ApprovalInstalled)
	if _, err := rig.runner.Invoke(stepContextFor(t, rig.db, call), "write_file", writeFileArgs(t, "seen.txt", "seen")); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	spans := rig.spans.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("spans = %d, want the step's one tool span", len(spans))
	}
	attrs := map[string]string{}
	for _, kv := range spans[0].Attributes {
		attrs[string(kv.Key)] = kv.Value.String()
	}
	if attrs[telemetry.AttrWorkflowRunID] != "run-observed" || attrs[telemetry.AttrWorkflowStepID] != "step-1" {
		t.Fatalf("span attributes = %v, want the run and step ids", attrs)
	}
	var data metricdata.ResourceMetrics
	if err := rig.metrics.Collect(t.Context(), &data); err != nil {
		t.Fatalf("collect: %v", err)
	}
	points := 0
	for _, scope := range data.ScopeMetrics {
		for _, metric := range scope.Metrics {
			var sets []attribute.Set
			switch series := metric.Data.(type) {
			case metricdata.Sum[int64]:
				for _, point := range series.DataPoints {
					sets = append(sets, point.Attributes)
				}
			case metricdata.Histogram[float64]:
				for _, point := range series.DataPoints {
					sets = append(sets, point.Attributes)
				}
			}
			for _, set := range sets {
				points++
				for _, key := range []string{telemetry.AttrWorkflowRunID, telemetry.AttrWorkflowStepID} {
					if _, present := set.Value(attribute.Key(key)); present {
						t.Errorf("metric %s carries the label %s", metric.Name, key)
					}
				}
			}
		}
	}
	if points == 0 {
		t.Fatal("the tool call recorded no metric data point")
	}
}

func TestWorkflowStepRefusalsAreLogged(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	rig := newSurfaceRig(t, &surfaceRigOptions{configure: surfacePersonas, logger: logger})
	call := surfaceCall("run-logged", ownerOrigin, workflow.ApprovalInstalled)
	call.Approval.Digest = "forged"
	_, err := rig.runner.Invoke(stepContextFor(t, rig.db, call), "write_file", writeFileArgs(t, "logged.txt", "x"))
	codeIs(t, err, workflow.ErrorCodeApprovalMissing)
	var entry map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &entry); err != nil {
		t.Fatalf("decode the log line %q: %v", logs.String(), err)
	}
	want := map[string]any{"level": "WARN", "component": "workflow", "run_id": "run-logged", "step_id": "step-1", "tool": "write_file", "code": string(workflow.ErrorCodeApprovalMissing)}
	for key, value := range want {
		if entry[key] != value {
			t.Fatalf("log field %s = %v, want %v (line %s)", key, entry[key], value, logs.String())
		}
	}
}

func TestNewWorkflowToolRunnerFailsClosedWithoutItsDependencies(t *testing.T) {
	rig := newSurfaceRig(t, &surfaceRigOptions{configure: surfacePersonas})
	full := func() *workflowToolRunnerInput {
		return &workflowToolRunnerInput{
			Executor: rig.executor, Sessions: store.NewSessionService(rig.db), Registry: rig.registry, Runs: workflow.NewStore(rig.db),
		}
	}
	tests := []struct {
		name    string
		mutate  func(in *workflowToolRunnerInput)
		wantErr bool
	}{
		{name: "every dependency present without a route", mutate: func(*workflowToolRunnerInput) {}},
		{name: "a nil executor", mutate: func(in *workflowToolRunnerInput) { in.Executor = nil }, wantErr: true},
		{name: "an executor without a broker", mutate: func(in *workflowToolRunnerInput) { in.Executor = &toolsbuiltin.Executor{} }, wantErr: true},
		{name: "nil sessions", mutate: func(in *workflowToolRunnerInput) { in.Sessions = nil }, wantErr: true},
		{name: "a nil registry", mutate: func(in *workflowToolRunnerInput) { in.Registry = nil }, wantErr: true},
		{name: "a nil workflow store", mutate: func(in *workflowToolRunnerInput) { in.Runs = nil }, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := full()
			tc.mutate(in)
			runner, err := newWorkflowToolRunner(in)
			if tc.wantErr {
				if err == nil || runner != nil {
					t.Fatalf("newWorkflowToolRunner = %v, %v; want a construction error", runner, err)
				}
				return
			}
			if err != nil || runner == nil {
				t.Fatalf("newWorkflowToolRunner = %v, %v; want a runner", runner, err)
			}
		})
	}
	if _, err := newWorkflowToolRunner(nil); err == nil {
		t.Fatal("newWorkflowToolRunner accepted a nil input")
	}
}
