package handoff

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/sqlite/sqlitetest"
	"github.com/anggasct/aura/internal/platform/turn/store"
)

func TestCommandsEmptyReplyTextReturnsUsageError(t *testing.T) {
	cmds := Commands(func(*cobra.Command, func(context.Context, *CommandDeps) error) error {
		t.Fatal("opener should not be called for empty reply text")
		return nil
	})
	replyCmd := cmds[0]
	replyCmd.SetArgs([]string{"sess-1", "   "})
	err := replyCmd.ExecuteContext(t.Context())
	if err == nil || err.Error() != "reply text must not be empty" {
		t.Fatalf("err = %v, want reply text must not be empty", err)
	}
	var flagged interface{ IsUsageError() bool }
	if !errors.As(err, &flagged) || !flagged.IsUsageError() {
		t.Fatalf("err = %T (%v), want IsUsageError() == true", err, err)
	}
}

func TestCommandsNilOpenerFailsWithInvalidArgument(t *testing.T) {
	cmds := Commands(nil)
	cmds[1].SetArgs([]string{"list"})
	err := cmds[1].ExecuteContext(t.Context())
	if !fault.HasCode(err, ErrorCodeInvalidArgument) {
		t.Fatalf("err = %v, want %s", err, ErrorCodeInvalidArgument)
	}
}

func TestCommandsListReplyAndRelease(t *testing.T) {
	db := sqlitetest.OpenMigrated(t, turnstore.Schema(), Schema())
	s := mustNewSQLiteStore(t, db)
	sessionID := "main/discord:office/dm:9"
	seedPersonaSession(t, db, sessionID, "discord:9", "main")
	opened := time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)

	var delivered []string
	var appended []string
	var actions []string
	open := func(_ *cobra.Command, fn func(context.Context, *CommandDeps) error) error {
		return fn(t.Context(), &CommandDeps{
			Store: s,
			Deliver: func(_ context.Context, _ *Handoff, text string) error {
				delivered = append(delivered, text)
				return nil
			},
			PersonaAuthor: func(string) string { return "Aura" },
			ClosedMessage: func(string) string { return "closed" },
			AppendReply: func(_ context.Context, _ *Handoff, author, text string, _ time.Time) error {
				appended = append(appended, author+":"+text)
				return nil
			},
			RecordAction: func(_ context.Context, action string, _ *Handoff, _ string, _ time.Time) error {
				actions = append(actions, action)
				return nil
			},
			Now: func() time.Time { return opened.Add(time.Minute) },
		})
	}

	run := func(cmd *cobra.Command, args ...string) (string, error) {
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs(args)
		err := cmd.ExecuteContext(t.Context())
		return out.String(), err
	}

	out, err := run(Commands(open)[1], "list")
	if err != nil || strings.TrimSpace(out) != "no open handoffs" {
		t.Fatalf("empty list = %q, %v", out, err)
	}
	if _, err := run(Commands(open)[0], sessionID, "hi"); !fault.HasCode(err, ErrorCodeNotOpen) {
		t.Fatalf("reply before open = %v, want %s", err, ErrorCodeNotOpen)
	}
	if err := s.Open(t.Context(), &Handoff{
		SessionID: sessionID, Reason: "refund", PrincipalID: "discord:9", AgentID: "main",
		Account: "office", Channel: ChannelDiscord, ReplyChannelID: "4400", OpenedAt: opened,
	}); err != nil {
		t.Fatal(err)
	}
	out, err = run(Commands(open)[1], "list")
	if err != nil || !strings.Contains(out, sessionID+"\tdiscord:9\tmain\t") {
		t.Fatalf("list = %q, %v", out, err)
	}
	out, err = run(Commands(open)[0], sessionID, "Your", "refund")
	if err != nil || strings.TrimSpace(out) != "replied in "+sessionID {
		t.Fatalf("reply = %q, %v", out, err)
	}
	out, err = run(Commands(open)[1], "release", sessionID)
	if err != nil || strings.TrimSpace(out) != "released "+sessionID {
		t.Fatalf("release = %q, %v", out, err)
	}
	if len(delivered) != 2 || delivered[0] != "Your refund" || delivered[1] != "closed" {
		t.Fatalf("delivered = %+v", delivered)
	}
	if len(appended) != 1 || appended[0] != "Aura:Your refund" {
		t.Fatalf("appended = %+v", appended)
	}
	if len(actions) != 2 || actions[0] != ActionReply || actions[1] != ActionRelease {
		t.Fatalf("actions = %+v", actions)
	}
}
