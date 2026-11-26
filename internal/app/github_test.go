package app

import (
	"testing"

	appconfig "github.com/anggasct/aura/internal/app/config"
	"github.com/anggasct/aura/internal/modules/github"
	"github.com/anggasct/aura/internal/platform/durable/durabletest"
	"github.com/anggasct/aura/internal/platform/fault"
	runtimeadk "github.com/anggasct/aura/internal/platform/turn/adk"
)

func registerGitHubModule(t *testing.T, executor *runtimeadk.BrokerExecutor, cfg *appconfig.Config) {
	t.Helper()
	mod, err := github.New(&github.ModuleDeps{Config: githubConfig(cfg)})
	if err != nil {
		t.Fatalf("github.New: %v", err)
	}
	if err := executor.Broker().RegisterProvider(mod); err != nil {
		t.Fatalf("RegisterProvider(github): %v", err)
	}
}

func TestWorkflowCorrelationStoreMapsErrorsAndDurableKey(t *testing.T) {
	_, wfStore := githubTestStore(t)
	runID := githubTestBinding(t, wfStore, durabletest.NewFake())
	adapted := newWorkflowCorrelationStore(wfStore)

	if err := adapted.BindCorrelation(t.Context(), nil); !fault.HasCode(err, github.ErrorCodeInvalidArgument) {
		t.Fatalf("BindCorrelation(nil) = %v, want %s", err, github.ErrorCodeInvalidArgument)
	}
	if _, err := adapted.ResolveCorrelation(t.Context(), github.Source, "check_suite.completed", "org/repo#999", ""); !fault.HasCode(err, github.ErrorCodeCorrelationUnmatched) {
		t.Fatalf("ResolveCorrelation(missing) = %v, want %s", err, github.ErrorCodeCorrelationUnmatched)
	}
	if err := adapted.DeleteCorrelation(t.Context(), github.Source, "check_suite.completed", "org/repo#999", ""); !fault.HasCode(err, github.ErrorCodeCorrelationUnmatched) {
		t.Fatalf("DeleteCorrelation(missing) = %v, want %s", err, github.ErrorCodeCorrelationUnmatched)
	}

	binding := &github.Correlation{
		Source:     github.Source,
		EventType:  "check_suite.completed",
		ExternalID: "org/repo#42",
		RunID:      runID,
		SignalName: "wait.hold",
		DedupeKey:  "evt-1",
	}
	if err := adapted.BindCorrelation(t.Context(), binding); err != nil {
		t.Fatalf("BindCorrelation: %v", err)
	}
	if err := adapted.BindCorrelation(t.Context(), binding); !fault.HasCode(err, github.ErrorCodeCorrelationConflict) {
		t.Fatalf("duplicate BindCorrelation = %v, want %s", err, github.ErrorCodeCorrelationConflict)
	}
	got, err := adapted.ResolveCorrelation(t.Context(), github.Source, "check_suite.completed", "org/repo#42", "evt-1")
	if err != nil || got != *binding {
		t.Fatalf("ResolveCorrelation = %+v, %v; want %+v", got, err, *binding)
	}
	durableKey, err := adapted.RunDurableKey(t.Context(), runID)
	if err != nil || durableKey == "" {
		t.Fatalf("RunDurableKey = %q, %v; want non-empty key", durableKey, err)
	}
	if err := adapted.DeleteCorrelation(t.Context(), github.Source, "check_suite.completed", "org/repo#42", "evt-1"); err != nil {
		t.Fatalf("DeleteCorrelation: %v", err)
	}
}
