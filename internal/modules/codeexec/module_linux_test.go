//go:build linux

package codeexec

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/configkit"
	"github.com/anggasct/aura/internal/platform/effect"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/sandbox"
	"github.com/anggasct/aura/internal/platform/sqlite"
	"github.com/anggasct/aura/internal/platform/storage"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/turn"
	turnstore "github.com/anggasct/aura/internal/platform/turn/store"
	"github.com/anggasct/aura/internal/store/storetest"
)

const codeArguments = `{"runtime":"python","source":"print(1)"}`

type moduleBrokerFixture struct {
	broker  *toolbroker.Broker
	mod     *Module
	stub    *stubSandbox
	prompts int
}

func newModuleBrokerFixture(t *testing.T, approve bool, mutateConfig func(*Config)) *moduleBrokerFixture {
	t.Helper()
	dataRoot := t.TempDir()
	t.Cleanup(func() { removeTree(dataRoot) })
	dbPath := filepath.Join(t.TempDir(), "aura.db")
	storetest.Seed(t, dbPath)
	db, err := sqlite.Open(t.Context(), dbPath, sqlite.Options{})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	session := turn.Session{ID: "session-1", OwnerID: "owner-1"}
	if err := turnstore.NewSessionService(db).Create(t.Context(), &session); err != nil {
		t.Fatalf("create session: %v", err)
	}
	journal, err := effect.NewJournal(db, effect.Options{Logger: slog.New(slog.DiscardHandler), Turns: turnstore.NewTurnStore(db)})
	if err != nil {
		t.Fatalf("NewJournal: %v", err)
	}
	effects, err := effect.NewExecutor(journal)
	if err != nil {
		t.Fatalf("NewExecutor: %v", err)
	}
	artifacts := storage.NewArtifactStore(db, t.TempDir(), storage.DefaultArtifactQuotaBytes)

	archive := rigArchive(t, "python")
	archivePath := filepath.Join(t.TempDir(), "python.tar.gz")
	if err := os.WriteFile(archivePath, archive, 0o600); err != nil {
		t.Fatal(err)
	}
	manifest := rigManifest(t, map[string][]byte{"python": archive}, []rigRuntime{{id: "python", version: rigPythonVersion, install: true}})

	cfg := Default()
	if mutateConfig != nil {
		mutateConfig(&cfg)
	}
	mod, err := New(t.Context(), &ModuleDeps{
		Config:    &cfg,
		DataRoot:  dataRoot,
		Manifest:  manifest,
		Artifacts: artifacts,
		Logger:    slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("codeexec.New: %v", err)
	}
	if _, err := mod.Store().Install(t.Context(), "python", archivePath); err != nil {
		t.Fatalf("install python: %v", err)
	}
	stub := &stubSandbox{
		script: []sandbox.Result{{Stdout: "3.12.16\n"}},
		result: sandbox.Result{Stdout: "1\n"},
	}
	mod.runner.sandbox = stub.run
	mod.VerifyRun(t.Context())
	if !mod.Verified().Verified("python") {
		t.Fatalf("python runtime was not verified: %+v", mod.Verified().Statuses())
	}
	stub.mu.Lock()
	stub.calls = nil
	stub.mu.Unlock()

	fixture := &moduleBrokerFixture{mod: mod, stub: stub}
	broker, err := toolbroker.New(&toolbroker.Options{
		Artifacts:        artifacts,
		Effects:          effects,
		HostCapabilities: []string{CapabilityName},
		ApprovalDecider: func(context.Context, *toolbroker.ApprovalPrompt) (bool, error) {
			fixture.prompts++
			return approve, nil
		},
	})
	if err != nil {
		t.Fatalf("toolbroker.New: %v", err)
	}
	if err := broker.RegisterProvider(mod); err != nil {
		t.Fatalf("RegisterProvider: %v", err)
	}
	fixture.broker = broker
	return fixture
}

func (f *moduleBrokerFixture) call(t *testing.T, arguments string) (json.RawMessage, error) {
	t.Helper()
	result, err := f.broker.Execute(t.Context(), &toolbroker.ToolRequest{
		RequestID:             "call-" + ToolName,
		TurnID:                "turn-1",
		SessionID:             "session-1",
		PrincipalID:           "owner-1",
		ToolName:              ToolName,
		ToolVersion:           "v1",
		Arguments:             json.RawMessage(arguments),
		Capabilities:          []string{CapabilityName},
		Trust:                 approval.TrustDerivedUntrusted,
		IdempotencyKey:        "test/" + ToolName,
		AllocateEventSequence: true,
		EventInvocation:       "inv-1",
		EventBranch:           "main",
		EventAuthor:           "agent",
	})
	if err != nil {
		return nil, err
	}
	return result.Output, nil
}

func TestCodeExecuteNeedsApprovalLikeExec(t *testing.T) {
	fixture := newModuleBrokerFixture(t, false, nil)
	_, err := fixture.call(t, codeArguments)
	if !fault.HasCode(err, toolbroker.ResultApprovalRequired) {
		t.Fatalf("code_execute error = %v, want approval_required", err)
	}
	if fixture.prompts != 1 || fixture.stub.count() != 0 {
		t.Fatalf("prompts = %d, sandbox calls = %d; want one prompt and no run", fixture.prompts, fixture.stub.count())
	}
}

func TestCodeExecuteRunsThroughTheInstalledRunner(t *testing.T) {
	fixture := newModuleBrokerFixture(t, true, nil)
	output, err := fixture.call(t, codeArguments)
	if err != nil {
		t.Fatalf("code_execute: %v", err)
	}
	var decoded struct {
		ResultClass string `json:"result_class"`
		Stdout      string `json:"stdout"`
	}
	if err := json.Unmarshal(output, &decoded); err != nil {
		t.Fatalf("decode output %s: %v", output, err)
	}
	if fixture.stub.count() != 1 || decoded.ResultClass != "ok" || decoded.Stdout != "1\n" {
		t.Fatalf("sandbox calls = %d, decoded = %+v", fixture.stub.count(), decoded)
	}
}

func TestCodeExecuteRejectsBadArgumentsBeforeAnyPrompt(t *testing.T) {
	cases := []struct {
		name      string
		arguments string
		mutate    func(*Config)
	}{
		{"unknown runtime", `{"runtime":"ruby","source":"x"}`, nil},
		{"unknown field", `{"runtime":"python","source":"x","env":{}}`, nil},
		{"missing source", `{"runtime":"python"}`, nil},
		{"configured limit", codeArguments, func(c *Config) { c.MaxSourceBytes = configkit.ByteSize(4) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newModuleBrokerFixture(t, true, tc.mutate)
			_, err := fixture.call(t, tc.arguments)
			if !fault.HasCode(err, toolbroker.ResultInvalidArgument) {
				t.Fatalf("code_execute error = %v, want invalid_argument", err)
			}
			if fixture.prompts != 0 || fixture.stub.count() != 0 {
				t.Fatalf("prompts = %d, sandbox calls = %d; want neither", fixture.prompts, fixture.stub.count())
			}
		})
	}
}

func TestCodeExecuteNeverReRunsAReplayedRequest(t *testing.T) {
	fixture := newModuleBrokerFixture(t, true, nil)
	first, err := fixture.call(t, codeArguments)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	replayed, err := fixture.call(t, codeArguments)
	if err != nil {
		t.Fatalf("replayed call: %v", err)
	}
	if !bytes.Equal(replayed, first) {
		t.Fatalf("replayed output = %s, want the first output %s", replayed, first)
	}
	if fixture.stub.count() != 1 {
		t.Fatalf("sandbox calls = %d, want the program to run once", fixture.stub.count())
	}
}

func TestCodeExecuteIsNeverOfferedToPublicPrincipals(t *testing.T) {
	fixture := newModuleBrokerFixture(t, true, nil)
	_, err := fixture.broker.Execute(t.Context(), &toolbroker.ToolRequest{
		RequestID:             "call-public",
		TurnID:                "turn-1",
		SessionID:             "session-1",
		PrincipalID:           "visitor-1",
		PrincipalClass:        principal.ClassPublic,
		ToolName:              ToolName,
		ToolVersion:           "v1",
		Arguments:             json.RawMessage(codeArguments),
		Capabilities:          []string{CapabilityName},
		Trust:                 approval.TrustDerivedUntrusted,
		IdempotencyKey:        "test/public",
		AllocateEventSequence: true,
		EventInvocation:       "inv-1",
		EventBranch:           "main",
		EventAuthor:           "agent",
	})
	if !fault.HasCode(err, toolbroker.ResultToolTierDenied) {
		t.Fatalf("code_execute for a public principal = %v, want tool_tier_denied", err)
	}
	if fixture.stub.count() != 0 || fixture.prompts != 0 {
		t.Fatalf("sandbox calls = %d, prompts = %d; want neither", fixture.stub.count(), fixture.prompts)
	}
}
