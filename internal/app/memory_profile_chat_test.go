package app

import (
	"slices"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/modules/profile/profiletest"
	"github.com/anggasct/aura/internal/platform/command"
	"github.com/anggasct/aura/internal/platform/principal"
)

func (r *chatRigE2E) memoryDocuments(t *testing.T) int {
	t.Helper()
	var count int
	if err := r.DB.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM memory_document`).Scan(&count); err != nil {
		t.Fatalf("count the memory documents: %v", err)
	}
	return count
}

func (r *chatRigE2E) factState(t *testing.T, id string) string {
	t.Helper()
	var state string
	if err := r.DB.QueryRowContext(t.Context(), `SELECT status || ':' || origin || ':' || owner_verified FROM profile_fact WHERE id = ?`, id).Scan(&state); err != nil {
		t.Fatalf("read the fact %s: %v", id, err)
	}
	return state
}

func TestMemoryAndProfileChatCommandsOnlyInDirectChatsForTheOwner(t *testing.T) {
	candidate := profiletest.CandidateFactID()
	for _, channel := range []string{principal.ChannelTelegram, principal.ChannelDiscord} {
		owner := chatSender{channel: channel, class: principal.ClassOwner, direct: true}
		t.Run(channel+" owner in a direct chat", func(t *testing.T) {
			rig := newChatRigE2E(t)

			inspected := rig.say(t, owner, "/memory inspect --session session-1")
			if len(inspected) != 2 || !strings.Contains(inspected[0], "mem-a1") || !strings.Contains(inspected[1], "mem-a2") {
				t.Fatalf("inspect replied %q, want the documents of the session", inspected)
			}
			previewed := rig.say(t, owner, "/memory forget --id mem-a1")
			if len(previewed) < 2 || previewed[0] != "dry-run: 1 would be deleted" || !strings.Contains(strings.Join(previewed, "\n"), "mem-a1") {
				t.Fatalf("forget replied %q, want a dry-run that names the document", previewed)
			}
			before := rig.memoryDocuments(t)
			applied := rig.say(t, owner, "/memory forget --id mem-a1 --apply")
			if !slices.Equal(applied, []string{"deleted: 1"}) {
				t.Fatalf("forget --apply replied %q", applied)
			}
			if rig.memoryDocuments(t) != before-1 {
				t.Errorf("documents = %d, want only the previewed document gone from %d", rig.memoryDocuments(t), before)
			}
			set := rig.say(t, owner, "/profile set --category tool --key shell --value zsh")
			if len(set) != 1 || !strings.HasPrefix(set[0], "set: pf-") {
				t.Fatalf("set replied %q", set)
			}
			if state := rig.factState(t, strings.TrimPrefix(set[0], "set: ")); state != "active:owner:1" {
				t.Errorf("the chat-set fact is %q, want it active, owner-origin and verified", state)
			}
			accepted := rig.say(t, owner, "/profile accept "+candidate)
			if !slices.Equal(accepted, []string{"accepted: " + candidate}) || rig.factState(t, candidate) != "active:agent:1" {
				t.Fatalf("accept replied %q with the fact %q, want it verified", accepted, rig.factState(t, candidate))
			}
			shown := rig.say(t, owner, "/profile delete "+candidate)
			if len(shown) < 2 || shown[0] != "dry-run: would delete" {
				t.Fatalf("delete replied %q, want a dry-run", shown)
			}
			if state := rig.factState(t, candidate); state != "active:agent:1" {
				t.Errorf("the dry-run changed the fact to %q", state)
			}
		})
		t.Run(channel+" refuses a group or guild channel", func(t *testing.T) {
			rig := newChatRigE2E(t)
			documents, facts := rig.memoryDocuments(t), rig.factState(t, candidate)

			for _, text := range []string{"/memory inspect", "/memory forget --id mem-a1 --apply", "/memory purge --apply", "/profile list", "/profile accept " + candidate, "/profile set --category tool --key shell --value zsh", "/profile nonsense"} {
				got := rig.say(t, chatSender{channel: channel, class: principal.ClassOwner}, text)
				if !slices.Equal(got, []string{command.ReplyDirectOnly}) {
					t.Errorf("%q outside a direct chat replied %q, want only the scope refusal", text, got)
				}
			}
			if rig.memoryDocuments(t) != documents || rig.factState(t, candidate) != facts {
				t.Error("a refused command changed the memory or the profile")
			}
		})
		t.Run(channel+" refuses other principals in a direct chat", func(t *testing.T) {
			rig := newChatRigE2E(t)
			documents, facts := rig.memoryDocuments(t), rig.factState(t, candidate)

			for _, class := range []principal.Class{principal.ClassTrusted, principal.ClassPublic} {
				for _, text := range []string{"/memory inspect", "/memory forget --id mem-a1 --apply", "/profile list", "/profile accept " + candidate} {
					got := rig.say(t, chatSender{channel: channel, class: class, direct: true}, text)
					if !slices.Equal(got, []string{command.ReplyDenied}) {
						t.Errorf("%q from a %s principal replied %q, want only the denial", text, class, got)
					}
				}
			}
			if rig.memoryDocuments(t) != documents || rig.factState(t, candidate) != facts {
				t.Error("a refused command changed the memory or the profile")
			}
		})
	}
}

func TestMemoryAndProfileChatCommandsAreAnsweredOnceWhenAMessageIsRedelivered(t *testing.T) {
	candidate := profiletest.CandidateFactID()
	cases := []struct {
		name    string
		command string
		handler string
		want    string
	}{
		{name: "an accept", command: "/profile accept " + candidate, handler: "profile.accept", want: "accepted: " + candidate},
		{name: "a forget that deletes", command: "/memory forget --id mem-a1 --apply", handler: "memory.forget", want: "deleted: 1"},
		{name: "a purge that deletes", command: "/memory purge --apply", handler: "memory.purge", want: "purged: 1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newChatRigE2E(t)
			owner := chatSender{channel: principal.ChannelTelegram, class: principal.ClassOwner, direct: true}
			rig.messages = 40

			first := rig.say(t, owner, tc.command)
			rig.messages = 40
			second := rig.say(t, owner, tc.command)

			if !slices.Equal(first, second) || !slices.Equal(first, []string{tc.want}) {
				t.Fatalf("first %q second %q, want the same journaled reply %q", first, second, tc.want)
			}
			if runs := rig.handlers.handled(tc.handler); runs != 1 {
				t.Errorf("the %s handler ran %d times for one message, want once", tc.handler, runs)
			}
		})
	}
}
