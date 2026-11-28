package app

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/anggasct/aura/internal/modules/mcp"
	"github.com/anggasct/aura/internal/modules/schedule"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/turn"
	turnstore "github.com/anggasct/aura/internal/platform/turn/store"

	appconfig "github.com/anggasct/aura/internal/app/config"
)

func runScheduled(t *testing.T, requests ...*schedule.TurnRequest) (*fakeCronEngine, []schedule.TurnResult) {
	t.Helper()
	db := cronTestDB(t)
	engine := &fakeCronEngine{}
	for range requests {
		engine.batches = append(engine.batches, cronCompletedBatch("turn-1", "done"))
	}
	runner := &engineTurnRunner{engine: engine, sessions: turnstore.NewSessionService(db)}
	var results []schedule.TurnResult
	for _, request := range requests {
		result, err := runner.RunTurn(t.Context(), request)
		if err != nil {
			t.Fatalf("RunTurn(%s): %v", request.SessionID, err)
		}
		results = append(results, result)
	}
	return engine, results
}

func scheduledRun(job, fire, context, taint string) *schedule.TurnRequest {
	return &schedule.TurnRequest{
		SessionID: "cron:" + job + ":" + fire, JobID: job, PrincipalID: principal.OwnerID, Origin: "schedule",
		Prompt: "summarize", Context: context, Timezone: "Asia/Jakarta", TaintSource: taint, IdempotencyKey: "key-" + fire,
	}
}

func TestScheduleCreatedOnATaintedConversationRunsTaintedThroughTheEngine(t *testing.T) {
	engine, _ := runScheduled(t,
		scheduledRun("job-1", "20260912T210000.000000000", schedule.ContextFull, "a webhook"),
		scheduledRun("job-2", "20260912T210000.000000000", schedule.ContextFull, ""),
	)
	tainted, clean := engine.calls[0], engine.calls[1]
	mark, isTainted := tainted.AdmissionTaint()
	if !isTainted || mark.Source != turn.TaintSourceScheduleOrigin || tainted.JobTaint != "a webhook" || tainted.JobID != "job-1" || tainted.WebOnly {
		t.Errorf("tainted job turn = %+v (%v, %t), want a schedule_origin admission", tainted, mark, isTainted)
	}
	if _, isTainted := clean.AdmissionTaint(); isTainted || clean.Timezone != "Asia/Jakarta" {
		t.Errorf("clean job turn = %+v, want a clean admission", clean)
	}
}

func TestWebOnlyRunIsTaintedFromAdmissionThroughTheEngine(t *testing.T) {
	engine, _ := runScheduled(t,
		scheduledRun("job-1", "20260912T210000.000000000", schedule.ContextWebOnly, ""),
		scheduledRun("job-2", "20260912T210000.000000000", schedule.ContextWebOnly, "a webhook"),
	)
	clean, tainted := engine.calls[0], engine.calls[1]
	if mark, isTainted := clean.AdmissionTaint(); !isTainted || mark.Source != turn.TaintSourceWebOnly || !clean.WebOnly || mark.Detail != "job-1" {
		t.Errorf("web-only turn = %+v (%v, %t), want a web_only admission naming the job", clean, mark, isTainted)
	}
	if mark, isTainted := tainted.AdmissionTaint(); !isTainted || mark.Source != turn.TaintSourceScheduleOrigin || !tainted.WebOnly || tainted.JobTaint == "" {
		t.Errorf("tainted web-only turn = %+v (%v, %t), want the job taint to take precedence and stay web-only", tainted, mark, isTainted)
	}
}

func TestOneRunsTaintDoesNotReachTheNextRunThroughTheEngine(t *testing.T) {
	db := cronTestDB(t)
	sessions := turnstore.NewSessionService(db)
	engine := &fakeCronEngine{batches: [][]turn.Event{cronCompletedBatch("turn-1", "a"), cronCompletedBatch("turn-2", "b")}}
	runner := &engineTurnRunner{engine: engine, sessions: sessions}
	first := scheduledRun("job-1", "20260912T210000.000000000", schedule.ContextFull, "a webhook")
	second := scheduledRun("job-1", "20260913T210000.000000000", schedule.ContextFull, "")
	for _, request := range []*schedule.TurnRequest{first, second} {
		if _, err := runner.RunTurn(t.Context(), request); err != nil {
			t.Fatalf("RunTurn: %v", err)
		}
	}
	for _, id := range []string{first.SessionID, second.SessionID} {
		if _, err := sessions.Get(t.Context(), id); err != nil {
			t.Errorf("run session %s was not created: %v", id, err)
		}
	}
	if engine.calls[0].SessionID == engine.calls[1].SessionID {
		t.Error("two runs share a session")
	}
	if _, isTainted := engine.calls[1].AdmissionTaint(); isTainted {
		t.Error("the run after a tainted run was admitted tainted")
	}
}

func TestWebOnlyToolRestrictionNamesTheModuleTools(t *testing.T) {
	want := turn.WebOnlyRestriction().Allowed
	got := []string{"web_search", "web_fetch"}
	if len(want) != len(got) || want[0] != got[0] || want[1] != got[1] {
		t.Fatalf("restriction = %v, want %v", want, got)
	}
	for _, name := range want {
		found := false
		for _, defined := range definitionNames() {
			found = found || defined == name
		}
		if !found {
			t.Errorf("the web-only restriction allows %q, which is no built-in tool", name)
		}
	}
}

func TestMCPWorkingDirMayNotHoldTheConfirmKey(t *testing.T) {
	root := t.TempDir()
	data := filepath.Join(root, "work", "data")
	if err := os.MkdirAll(data, 0o700); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(root, "elsewhere")
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}
	stdio := []mcp.ServerConfig{{Name: "notes", Transport: mcp.TransportStdio, Command: "notes-server"}}
	remote := []mcp.ServerConfig{{Name: "docs", Transport: mcp.TransportStreamableHTTP, URL: "https://docs.example.com/mcp"}}
	pluginStdio := []mcp.PluginServer{{ID: "notes", Server: mcp.ServerConfig{Name: "notes", Transport: mcp.TransportStdio, Command: "x"}}}
	cases := map[string]struct {
		cwd     string
		servers []mcp.ServerConfig
		plugins []mcp.PluginServer
		refused bool
	}{
		"a stdio server in the data directory":     {cwd: data, servers: stdio, refused: true},
		"a stdio server above the data directory":  {cwd: filepath.Join(root, "work"), servers: stdio, refused: true},
		"a plugin stdio backend above the data":    {cwd: root, plugins: pluginStdio, refused: true},
		"a stdio server in another directory":      {cwd: other, servers: stdio},
		"a stdio server in a directory under data": {cwd: filepath.Join(data, "sub"), servers: stdio},
		"a remote server above the data directory": {cwd: root, servers: remote},
		"no server above the data directory":       {cwd: root},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if err := os.MkdirAll(tc.cwd, 0o700); err != nil {
				t.Fatal(err)
			}
			t.Chdir(tc.cwd)
			cfg := &appconfig.Config{MCP: &mcp.Config{Servers: tc.servers}}
			cfg.Storage.Path = data
			err := checkMCPWorkingDir(cfg, tc.plugins)
			if tc.refused != (err != nil) {
				t.Errorf("checkMCPWorkingDir = %v, want refused=%t", err, tc.refused)
			}
		})
	}
}
