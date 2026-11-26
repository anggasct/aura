package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/configkit"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/turn"
	"github.com/anggasct/aura/internal/store/storetest"
)

func testModuleConfig(root string) *Config {
	return &Config{
		Enabled:                 true,
		Roots:                   []string{root},
		AutoSelect:              true,
		MaxIndexedSkills:        16,
		MaxInstructionTokens:    100,
		MaxSessionContextTokens: 300,
		MaxResourceBytes:        8388608,
		QuarantineRetention:     configkit.Duration(time.Hour),
	}
}

func TestNewModuleRejectsNilDependencies(t *testing.T) {
	db := storetest.OpenMigrated(t)
	cfg := testModuleConfig(t.TempDir())
	log := &memoryActivationLog{}

	cases := map[string]*ModuleDeps{
		"nil deps":        nil,
		"nil config":      {DB: db, Activations: log},
		"nil database":    {Config: cfg, Activations: log},
		"nil activations": {Config: cfg, DB: db},
	}
	for name, deps := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := New(t.Context(), deps)
			if !fault.HasCode(err, ErrorCodeInvalidArgument) {
				t.Fatalf("err = %v, want %s", err, ErrorCodeInvalidArgument)
			}
		})
	}
}

func TestModuleWiresContextProviderRendererAndChatCommands(t *testing.T) {
	db := storetest.OpenMigrated(t)
	root := filepath.Join(t.TempDir(), "skills-root")
	skillDir := filepath.Join(root, "review-pr")
	if err := os.MkdirAll(skillDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: review-pr\ndescription: Review PRs.\n---\nMOD-RULES\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	log := &memoryActivationLog{}
	mod, err := New(t.Context(), &ModuleDeps{
		Config:      testModuleConfig(root),
		DB:          db,
		Activations: log,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if mod.Engine() == nil || mod.Renderer() == nil || mod.ChatCommands() == nil {
		t.Fatalf("module accessors returned nil: engine=%v renderer=%v commands=%v", mod.Engine(), mod.Renderer(), mod.ChatCommands())
	}

	report, err := mod.Engine().Doctor(t.Context())
	if err != nil || len(report.Rows) != 1 {
		t.Fatalf("doctor = %+v, %v", report, err)
	}
	if _, err := mod.Engine().Accept(t.Context(), report.Rows[0].ID, report.Rows[0].Digest, nil); err != nil {
		t.Fatalf("accept: %v", err)
	}

	pin, err := mod.ChatCommands().Activate(t.Context(), "review-pr")
	if err != nil {
		t.Fatalf("activate: %v", err)
	}
	caveat, body, err := mod.Renderer().RenderSkill(t.Context(), "session-1", &pin)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	log.record(&turn.SkillActivation{
		SkillID:       pin.SkillID,
		Digest:        pin.Digest,
		InvocationID:  pin.InvocationID,
		PolicyVersion: pin.PolicyVersion,
		TurnID:        "turn-1",
		Caveat:        caveat,
		Body:          body,
	})

	req := &turn.TurnRequest{
		SessionID:      "session-1",
		TurnID:         "turn-1",
		PrincipalID:    principal.OwnerID,
		PrincipalClass: principal.ClassOwner,
	}
	if mod.Required(req) {
		t.Fatal("Required() = true, want false")
	}
	blocks, err := mod.TurnContext(t.Context(), req)
	if err != nil || len(blocks) != 1 || blocks[0].Order != turn.ContextOrderSkills || !strings.Contains(blocks[0].Content, "MOD-RULES") {
		t.Fatalf("TurnContext = %+v, %v", blocks, err)
	}

	draft, err := mod.ChatCommands().StageDraft(t.Context(), "new-draft", "Staged via bridge.")
	if err != nil || draft.ID == "" || draft.Digest == "" {
		t.Fatalf("StageDraft = %+v, %v", draft, err)
	}

	var nilMod *Module
	if blocks, err := nilMod.TurnContext(t.Context(), req); err != nil || len(blocks) != 0 {
		t.Fatalf("nil Module.TurnContext = %v, %v", blocks, err)
	}
	if nilMod.Required(req) || nilMod.Renderer() != nil || nilMod.Engine() != nil || nilMod.ChatCommands() != nil {
		t.Fatal("nil Module accessors must return zero values")
	}
}
