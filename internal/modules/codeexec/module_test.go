package codeexec

import (
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/health"
	"github.com/anggasct/aura/internal/platform/storage"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/turn"
	turnstore "github.com/anggasct/aura/internal/platform/turn/store"
	"github.com/anggasct/aura/internal/store/storetest"
)

func TestNewCodeRunnerRequiresItsOptions(t *testing.T) {
	defaults := Default()
	artifacts := storage.NewArtifactStore(storetest.OpenMigrated(t), t.TempDir(), storage.DefaultArtifactQuotaBytes)
	invalid := Default()
	invalid.MaxProcesses = 0
	cases := []struct {
		name string
		deps *ModuleDeps
	}{
		{"nil deps", nil},
		{"missing config", &ModuleDeps{DataRoot: t.TempDir(), Artifacts: artifacts}},
		{"missing data root", &ModuleDeps{Config: &defaults, Artifacts: artifacts}},
		{"missing artifact store", &ModuleDeps{Config: &defaults, DataRoot: t.TempDir()}},
		{"invalid config limits", &ModuleDeps{Config: &invalid, DataRoot: t.TempDir(), Artifacts: artifacts}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mod, err := New(t.Context(), tc.deps)
			if err == nil || mod != nil {
				t.Fatalf("New = %v, %v; want an error and no module", mod, err)
			}
			if !fault.HasCode(err, ErrorCodeInvalidArgument) {
				t.Fatalf("New error = %v, want %s", err, ErrorCodeInvalidArgument)
			}
		})
	}
}

func TestCodeArtifactsMapStoreFailuresToCodes(t *testing.T) {
	db := storetest.OpenMigrated(t)
	if err := turnstore.NewSessionService(db).Create(t.Context(), &turn.Session{
		ID: "session-1", OwnerID: "owner-1", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	sink := storageArtifactSink{store: storage.NewArtifactStore(db, t.TempDir(), 8)}
	cases := []struct {
		name     string
		content  string
		session  string
		wantCode ErrorCode
		wantOK   bool
	}{
		{"stored", "abc", "session-1", "", true},
		{"over the quota", "abcdefghi", "session-1", ErrorCodeArtifactQuota, false},
		{"unknown session", "xyz", "session-missing", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			receipt, err := sink.Put(t.Context(), strings.NewReader(tc.content), &ArtifactMeta{
				ID: "art-" + tc.name, SessionID: tc.session, Filename: "a.txt", MediaType: "text/plain",
			})
			if tc.wantOK {
				if err != nil || receipt.ID != "art-"+tc.name || receipt.SizeBytes != int64(len(tc.content)) || receipt.Digest == "" {
					t.Fatalf("Put = %+v, %v", receipt, err)
				}
				return
			}
			code, coded := fault.CodeOf[ErrorCode](err)
			if err == nil || coded != (tc.wantCode != "") || code != tc.wantCode {
				t.Fatalf("Put error = %v (code %q, coded %v), want code %q", err, code, coded, tc.wantCode)
			}
			if strings.Contains(err.Error(), tc.session) {
				t.Fatalf("the error echoes store detail: %v", err)
			}
		})
	}
}

func TestModuleImplementsDiscoveryInterfaces(t *testing.T) {
	var _ toolbroker.ToolProvider = (*Module)(nil)
	var _ health.HealthProvider = (*Module)(nil)

	var nilMod *Module
	if nilMod.Tools() != nil || nilMod.HealthChecks() != nil || nilMod.Store() != nil || nilMod.Verified() != nil || nilMod.Runner() != nil {
		t.Fatal("nil module returned non-nil discovery outputs")
	}
	nilMod.VerifyRun(t.Context())

	defaults := Default()
	dataRoot := t.TempDir()
	t.Cleanup(func() { removeTree(dataRoot) })
	mod, err := New(t.Context(), &ModuleDeps{
		Config:       &defaults,
		DataRoot:     dataRoot,
		Artifacts:    storage.NewArtifactStore(storetest.OpenMigrated(t), t.TempDir(), storage.DefaultArtifactQuotaBytes),
		CheckTimeout: 5 * time.Second,
		Logger:       slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	regs := mod.Tools()
	if len(regs) != 1 || regs[0].Definition.Name != ToolName || regs[0].Adapter == nil || regs[0].ArgumentCheck == nil {
		t.Fatalf("Tools() = %+v", regs)
	}
	checks := mod.HealthChecks()
	if len(checks) != 1 || checks[0].ID != ComponentInterpreter || checks[0].Timeout != 5*time.Second {
		t.Fatalf("HealthChecks() = %+v", checks)
	}
}
