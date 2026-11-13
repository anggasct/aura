//go:build linux

package app

import (
	"testing"

	"github.com/anggasct/aura/internal/channels/kit/approval"
	"github.com/anggasct/aura/internal/platform/principal"
)

func buildServerPhases(t *testing.T, extra string) *build {
	t.Helper()
	restoreDefaultLogger(t)
	t.Setenv("AURA_TEST_MODEL_KEY", "test-key")
	t.Setenv("AURA_TEST_OWNER_ROUTE_TOKEN", "123456:token")
	path := writeCompositionConfig(t, freeLoopbackAddress(t), compositionModelsYAML, extra)
	b := newBuild(serverCompositionOptions(loadCompositionConfig(t, path), &syncBuffer{}))
	t.Cleanup(func() {
		if b.group != nil {
			_ = b.group.Stop(t.Context())
		}
	})
	for _, step := range b.phases(t.Context()) {
		if err := step(); err != nil {
			t.Fatalf("build phase: %v", err)
		}
	}
	return b
}

func TestServerWithoutApprovalAdaptersBuildsNoWorkflowRoute(t *testing.T) {
	b := buildServerPhases(t, "")
	if b.server.workflowRoute != nil {
		t.Fatal("workflow route built without an approval adapter")
	}
	if b.wiring.decider != nil || b.wiring.reachable != nil {
		t.Fatal("approval decider or reachability built without an approval target")
	}
}

func TestServerWithAnApprovalAdapterHoldsWorkflowSessionsOnlyInFlight(t *testing.T) {
	extra := "owner:\n  identities: [\"telegram:777\"]\nchannels:\n  telegram:\n    enabled: true\n    accounts:\n      - name: pocket\n        bot_token_ref: env://AURA_TEST_OWNER_ROUTE_TOKEN\n        persona: main\n        allowed_user_ids: [\"777\"]\n"
	b := buildServerPhases(t, extra)
	route := b.server.workflowRoute
	if route == nil {
		t.Fatal("workflow route not built with an approval adapter")
	}
	if b.wiring.decider == nil || b.wiring.reachable == nil {
		t.Fatal("approval decider or reachability missing with an approval adapter")
	}
	const session = "wf-run-1"
	if b.wiring.reachable(session) {
		t.Fatal("workflow session reachable before it is held")
	}
	release := route.Hold(session, &approval.Requester{Principal: "telegram:777", Class: principal.ClassOwner, Persona: "main"})
	if !b.wiring.reachable(session) {
		t.Fatal("workflow session not reachable while a call is in flight")
	}
	release()
	if b.wiring.reachable(session) {
		t.Fatal("workflow session reachable after the call")
	}
}
