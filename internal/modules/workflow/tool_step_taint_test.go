package workflow

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/tools"
	"github.com/anggasct/aura/internal/platform/turn"
)

type sinkBroker struct {
	definitions map[string]tools.Definition
}

func (b *sinkBroker) Definition(name, _ string) (tools.Definition, bool) {
	definition, ok := b.definitions[name]
	return definition, ok
}

func (*sinkBroker) StandingApprovalAllowed(context.Context, *toolbroker.ToolRequest) (bool, error) {
	return true, nil
}

func (*sinkBroker) ExactCallText(string, json.RawMessage) (string, error) { return "", nil }

func (*sinkBroker) GrantUntil(context.Context, *toolbroker.ToolRequest, time.Time) (approval.ApprovalGrant, error) {
	return approval.ApprovalGrant{}, nil
}

func (*sinkBroker) Execute(_ context.Context, request *toolbroker.ToolRequest) (toolbroker.ToolResult, error) {
	return toolbroker.ToolResult{ToolName: request.ToolName, Output: json.RawMessage(`{"ok":true}`)}, nil
}

type ownerRunSession struct{}

func (ownerRunSession) Get(_ context.Context, id string) (turn.Session, error) {
	if id == "" {
		return turn.Session{}, sql.ErrNoRows
	}
	return turn.Session{ID: id, OwnerID: principal.OwnerID, AgentID: "main"}, nil
}

func TestToolStepRunnerMarksTheOutputOfAnExternalSinkTainted(t *testing.T) {
	registry, err := buildTestRegistry()
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name        string
		definition  tools.Definition
		wantTainted bool
	}{
		{name: "an external sink tool", definition: tools.Definition{Name: "web_fetch", Version: tools.Version, ExternalSink: true}, wantTainted: true},
		{name: "a workspace read", definition: tools.Definition{Name: "read_file", Version: tools.Version}, wantTainted: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner, err := NewToolStepRunner(&ToolStepRunnerDeps{
				Broker:   &sinkBroker{definitions: map[string]tools.Definition{tc.definition.Name: tc.definition}},
				Sessions: ownerRunSession{}, Registry: registry, Runs: newTestStore(t),
			})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
			defer cancel()
			ctx = WithStepCall(ctx, &StepCall{
				RunID: "run-labels", StepID: "fetch", DefinitionID: "labels", DefinitionVersion: 1, DefinitionDigest: "digest",
				Origin:   RunOrigin{PrincipalID: principal.OwnerID, PrincipalClass: string(principal.ClassOwner), AgentID: "main"},
				Approval: Approval{DefinitionID: "labels", DefinitionVersion: 1, Digest: "digest", Kind: ApprovalInstalled},
			})
			result, err := runner.Invoke(ctx, tc.definition.Name, json.RawMessage(`{"path":"a"}`))
			if err != nil {
				t.Fatalf("Invoke: %v", err)
			}
			if result.Tainted != tc.wantTainted {
				t.Fatalf("Tainted = %v, want %v", result.Tainted, tc.wantTainted)
			}
		})
	}
}
