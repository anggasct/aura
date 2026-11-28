package runtimeadk

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/turn"
	runtimeingress "github.com/anggasct/aura/internal/platform/turn/ingress"
)

type webOnlyRig struct {
	model    *declarationModel
	builtin  *fakeBuiltinExecutor
	provider *stubContextProvider
}

func newWebOnlyRig(t *testing.T, call string) (*webOnlyRig, *ADKExecutor) {
	t.Helper()
	rig := &webOnlyRig{model: &declarationModel{call: call}, builtin: accessBuiltins(), provider: &stubContextProvider{
		blocks: []turn.ContextBlock{{Order: 150, Content: "PRIVATE-PROFILE-BLOCK", Caveat: "PRIVATE-CAVEAT"}},
	}}
	schema := json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)
	rig.builtin.definitions = append(rig.builtin.definitions, BuiltinToolDefinition{Name: "web_fetch", Version: "v1", Description: "fetch", Schema: schema})
	clock := func() time.Time { return time.Date(2026, 9, 12, 14, 30, 0, 0, time.UTC) }
	return rig, newAccessExecutor(t, rig.model, rig.builtin, WithOwnerContext("PRIVATE-OWNER-CONTEXT"), WithContextProvider(rig.provider), WithClock(clock))
}

func webOnlyTurn() *turn.TurnRequest {
	req := classTurn(principal.ClassOwner)
	req.Origin = turn.OriginSchedule
	req.WebOnly, req.JobID, req.Timezone = true, "job-1", "Asia/Jakarta"
	req.Knowledge = []string{"private-notes"}
	req.Parts = []runtimeingress.InputPart{{Text: "what shipped this week"}}
	return req
}

func TestWebOnlyRunHasNoPrivateContextOrTools(t *testing.T) {
	rig, executor := newWebOnlyRig(t, "")
	drainTurn(t, executor, webOnlyTurn())

	if len(rig.model.declarations) == 0 || !slices.Equal(rig.model.declarations[0], []string{"web_fetch", "web_search"}) {
		t.Fatalf("tools offered = %v, want only web_fetch and web_search whatever the persona lists", rig.model.declarations)
	}
	system := strings.Join(rig.model.systems, "\n")
	for _, private := range []string{"SOUL", "PRIVATE-OWNER-CONTEXT", "PRIVATE-PROFILE-BLOCK", "PRIVATE-CAVEAT"} {
		if strings.Contains(system, private) {
			t.Errorf("the instruction carries %q:\n%s", private, system)
		}
	}
	for _, want := range []string{"web_search", "web_fetch", "Saturday, 12 September 2026 21:30", "Asia/Jakarta"} {
		if !strings.Contains(system, want) {
			t.Errorf("the instruction lacks %q:\n%s", want, system)
		}
	}
	var texts []string
	for _, content := range rig.model.contents {
		for _, part := range content.Parts {
			texts = append(texts, part.Text)
		}
	}
	if !slices.Equal(texts, []string{"what shipped this week"}) || rig.provider.calls != 0 {
		t.Errorf("model input = %q, provider calls = %d, want only the job prompt and no context provider", texts, rig.provider.calls)
	}
	t.Run("a full run of the same persona keeps its context", func(t *testing.T) {
		rig, executor := newWebOnlyRig(t, "")
		req := webOnlyTurn()
		req.WebOnly = false
		drainTurn(t, executor, req)
		system := strings.Join(rig.model.systems, "\n")
		if !strings.Contains(system, "SOUL") || !strings.Contains(system, "PRIVATE-OWNER-CONTEXT") || rig.provider.calls != 1 {
			t.Errorf("a full run lost its context (provider calls %d):\n%s", rig.provider.calls, system)
		}
	})
	t.Run("an unknown timezone falls back to UTC", func(t *testing.T) {
		rig, executor := newWebOnlyRig(t, "")
		req := webOnlyTurn()
		req.Timezone = "Mars/Olympus"
		drainTurn(t, executor, req)
		if system := strings.Join(rig.model.systems, "\n"); !strings.Contains(system, "14:30") || !strings.Contains(system, "UTC") {
			t.Errorf("the instruction = %s, want the date in UTC", system)
		}
	})
}

func TestWebOnlyRunIsTaintedFromAdmission(t *testing.T) {
	cases := map[string]struct {
		jobTaint   string
		wantRelief bool
	}{
		"a clean job":   {wantRelief: true},
		"a tainted job": {jobTaint: "a webhook"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			rig, executor := newWebOnlyRig(t, "web_fetch")
			req := webOnlyTurn()
			req.JobTaint = tc.jobTaint
			drainTurn(t, executor, req)
			if len(rig.builtin.requests) != 1 {
				t.Fatalf("builtin requests = %d, want the one web_fetch", len(rig.builtin.requests))
			}
			got := rig.builtin.requests[0]
			if !got.Tainted || got.WebOnlyRelief != tc.wantRelief || len(got.Knowledge) != 0 {
				t.Errorf("request = tainted %t relief %t knowledge %v, want tainted, relief %t and no knowledge", got.Tainted, got.WebOnlyRelief, got.Knowledge, tc.wantRelief)
			}
			wantSource := "a web-only scheduled run"
			if tc.jobTaint != "" {
				wantSource = "a schedule created in a conversation with untrusted content"
			}
			if got.TaintSource != wantSource {
				t.Errorf("taint source = %q, want %q", got.TaintSource, wantSource)
			}
		})
	}
}
