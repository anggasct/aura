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

func stringColumn(t *testing.T, db *sql.DB, query string) []string {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), query)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	defer func() { _ = rows.Close() }()
	var values []string
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			t.Fatal(err)
		}
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return values
}

func columnsHolding(t *testing.T, db *sql.DB, needle string) []string {
	t.Helper()
	var found []string
	for _, table := range stringColumn(t, db, `SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name`) {
		for _, column := range stringColumn(t, db, fmt.Sprintf(`SELECT name FROM pragma_table_info(%q)`, table)) {
			var hit bool
			query := fmt.Sprintf(`SELECT EXISTS (SELECT 1 FROM %q WHERE instr(CAST(%q AS TEXT), ?) > 0)`, table, column)
			if err := db.QueryRowContext(t.Context(), query, needle).Scan(&hit); err != nil {
				t.Fatalf("search %s.%s: %v", table, column, err)
			}
			if hit {
				found = append(found, table+"."+column)
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
	all := columnsHolding(t, rig.DB, code)
	if !slices.Contains(all, "effect_intent.result_json") {
		t.Fatalf("the search found the code in %v, want it to see the reply journal", all)
	}
	held := slices.DeleteFunc(all, func(column string) bool { return strings.HasPrefix(column, "effect_intent.") })
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
