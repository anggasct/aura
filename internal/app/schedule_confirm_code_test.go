package app

import (
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/modules/schedule"
	"github.com/anggasct/aura/internal/platform/principal"
)

func columnsHolding(t *testing.T, db *sql.DB, needle string) []string {
	t.Helper()
	tables, err := db.QueryContext(t.Context(), `SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	var names []string
	for tables.Next() {
		var name string
		if err := tables.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	if err := tables.Err(); err != nil {
		t.Fatal(err)
	}
	_ = tables.Close()
	var found []string
	for _, table := range names {
		columns, err := db.QueryContext(t.Context(), fmt.Sprintf(`SELECT name FROM pragma_table_info('%s')`, table))
		if err != nil {
			t.Fatalf("columns of %s: %v", table, err)
		}
		var cols []string
		for columns.Next() {
			var col string
			if err := columns.Scan(&col); err != nil {
				t.Fatal(err)
			}
			cols = append(cols, col)
		}
		_ = columns.Close()
		for _, col := range cols {
			var hit int
			query := fmt.Sprintf(`SELECT EXISTS (SELECT 1 FROM "%s" WHERE instr(CAST("%s" AS TEXT), ?) > 0)`, table, col)
			if err := db.QueryRowContext(t.Context(), query, needle).Scan(&hit); err != nil {
				t.Fatalf("search %s.%s: %v", table, col, err)
			}
			if hit == 1 {
				found = append(found, table+"."+col)
			}
		}
	}
	return found
}

func TestConfirmCodeFromChatReachesOnlyTheReplyJournal(t *testing.T) {
	rig := newChatRigE2E(t)
	owner := chatSender{channel: principal.ChannelTelegram, class: principal.ClassOwner, direct: true}
	id := seedTaintedJob(t, rig)

	invocation, code := previewCode(t, rig.say(t, owner, "/cron confirm "+id))
	if applied := rig.say(t, owner, invocation+" --apply "+code); !strings.Contains(strings.Join(applied, "\n"), "tainted: false") {
		t.Fatalf("apply replied %q, want the confirmed job", applied)
	}
	held := slices.DeleteFunc(columnsHolding(t, rig.DB, code), func(column string) bool { return strings.HasPrefix(column, "effect_intent.") })
	if len(held) != 0 {
		t.Errorf("the confirmation code is stored in %v, want only the journal of replies to the owner", held)
	}
}

func TestConfirmCodeFromTheCLIIsStoredInNoTable(t *testing.T) {
	gf := scheduledWorkflowFixture(t)
	db := openAccessDB(t, gf.ConfigPath)
	jobs, err := schedule.NewSQLiteStore(db)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	const id = "job_00000000000000d2"
	if err := jobs.InsertJob(t.Context(), &schedule.Job{
		ID: id, Name: "legacy", CronExpression: "0 3 * * 1", Timezone: "UTC", Prompt: "summarize", Context: schedule.ContextFull,
		OriginChannel: "discord", OriginDestination: "default", OverlapPolicy: schedule.OverlapSkip,
		State: schedule.JobActive, Version: 1, CreatedAt: now, UpdatedAt: now, Tainted: true, TaintSource: "a webhook",
	}); err != nil {
		t.Fatalf("InsertJob: %v", err)
	}
	preview, err := runScheduleCommand(t, gf, "confirm", id)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	match := cliConfirmLine.FindStringSubmatch(lastLine(preview))
	if match == nil {
		t.Fatalf("preview = %q, want the code line", preview)
	}
	if out, err := runScheduleCommand(t, gf, "confirm", id, "--apply", match[2]); err != nil || !strings.Contains(out, "tainted: false") {
		t.Fatalf("apply = %q, %v, want the confirmed job", out, err)
	}
	if held := columnsHolding(t, db, match[2]); len(held) != 0 {
		t.Errorf("the confirmation code is stored in %v, want it in no table", held)
	}
}
