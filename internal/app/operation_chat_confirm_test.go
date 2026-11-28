package app

import (
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/modules/schedule"
	"github.com/anggasct/aura/internal/platform/command"
	"github.com/anggasct/aura/internal/platform/principal"
)

var confirmLine = regexp.MustCompile(`^confirm with: (/cron confirm \S+) --apply (\S+)$`)

func seedTaintedJob(t *testing.T, rig *chatRigE2E) string {
	t.Helper()
	jobs, err := schedule.NewSQLiteStore(rig.DB)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	job := &schedule.Job{
		ID: "job_00000000000000c1", Name: "nightly", CronExpression: "0 21 * * *", Timezone: "UTC", Prompt: "summarize", Context: schedule.ContextFull,
		OriginChannel: "telegram", OriginDestination: "me", OverlapPolicy: schedule.OverlapSkip, State: schedule.JobActive, Version: 1,
		CreatedAt: now, UpdatedAt: now, Tainted: true, TaintSource: "a webhook",
	}
	if err := jobs.InsertJob(t.Context(), job); err != nil {
		t.Fatalf("InsertJob: %v", err)
	}
	return job.ID
}

func (r *chatRigE2E) taintMark(t *testing.T, id string) (tainted bool, version int) {
	t.Helper()
	if err := r.DB.QueryRowContext(t.Context(), `SELECT tainted, version FROM scheduled_job WHERE id = ?`, id).Scan(&tainted, &version); err != nil {
		t.Fatalf("read job: %v", err)
	}
	return tainted, version
}

func previewCode(t *testing.T, lines []string) (invocation, code string) {
	t.Helper()
	for _, line := range lines {
		if match := confirmLine.FindStringSubmatch(line); match != nil {
			return match[1], match[2]
		}
	}
	t.Fatalf("the reply %q has no confirmation line", lines)
	return "", ""
}

func TestCronConfirmThroughChatNeedsTheCodeTheOwnerWasShown(t *testing.T) {
	rig := newChatRigE2E(t)
	owner := chatSender{channel: principal.ChannelTelegram, class: principal.ClassOwner, direct: true}
	id := seedTaintedJob(t, rig)

	preview := rig.say(t, owner, "/cron confirm "+id)
	invocation, code := previewCode(t, preview)
	if !slices.ContainsFunc(preview, func(line string) bool { return strings.Contains(line, "a webhook") }) {
		t.Errorf("the preview %q does not name the taint source", preview)
	}
	if tainted, version := rig.taintMark(t, id); !tainted || version != 1 {
		t.Fatalf("the preview changed the job: tainted=%t version=%d", tainted, version)
	}
	for name, text := range map[string]string{
		"no code":      invocation + " --apply",
		"a wrong code": invocation + " --apply 0000-0000",
	} {
		reply := rig.say(t, owner, text)
		if !strings.Contains(strings.Join(reply, "\n"), "operation_confirmation_invalid") && !strings.Contains(strings.Join(reply, "\n"), "usage") {
			t.Errorf("%s replied %q, want a refusal", name, reply)
		}
		if tainted, _ := rig.taintMark(t, id); !tainted {
			t.Fatalf("%s cleared the mark", name)
		}
	}

	applied := rig.say(t, owner, invocation+" --apply "+code)
	if !slices.Contains(applied, "id: "+id) || !slices.Contains(applied, "tainted: false") {
		t.Fatalf("apply replied %q, want the confirmed job", applied)
	}
	if tainted, version := rig.taintMark(t, id); tainted || version != 2 {
		t.Errorf("after the apply tainted=%t version=%d, want a clean job at version 2", tainted, version)
	}
	rig.messages--
	again := rig.say(t, owner, invocation+" --apply "+code)
	if !slices.Equal(again, applied) {
		t.Errorf("a redelivered apply replied %q, want the journaled %q", again, applied)
	}
	if _, version := rig.taintMark(t, id); version != 2 {
		t.Errorf("a redelivered apply ran again: version %d", version)
	}
	if empty := rig.say(t, owner, "/cron confirm "+id); !slices.ContainsFunc(empty, func(line string) bool { return strings.Contains(line, "nothing to confirm") }) {
		t.Errorf("confirming a clean job replied %q, want nothing to confirm", empty)
	}
}

func TestCronConfirmThroughChatIsOwnerOnlyAndDirectOnly(t *testing.T) {
	rig := newChatRigE2E(t)
	id := seedTaintedJob(t, rig)
	for _, channel := range []string{principal.ChannelTelegram, principal.ChannelDiscord} {
		got := rig.say(t, chatSender{channel: channel, class: principal.ClassOwner}, "/cron confirm "+id)
		if !slices.Equal(got, []string{command.ReplyDirectOnly}) {
			t.Errorf("%s group chat replied %q, want only the scope refusal", channel, got)
		}
		for _, class := range []principal.Class{principal.ClassTrusted, principal.ClassPublic} {
			got := rig.say(t, chatSender{channel: channel, class: class, direct: true}, "/cron confirm "+id)
			if !slices.Equal(got, []string{command.ReplyDenied}) {
				t.Errorf("%s %s principal replied %q, want only the denial", channel, class, got)
			}
		}
	}
	if tainted, _ := rig.taintMark(t, id); !tainted {
		t.Error("a refused call cleared the mark")
	}
}
