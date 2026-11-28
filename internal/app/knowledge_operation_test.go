package app

import (
	"slices"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/modules/knowledge/knowledgetest"
	"github.com/anggasct/aura/internal/platform/command"
	"github.com/anggasct/aura/internal/platform/principal"
)

const chatAddURL = "https://docs.example.test/shipping"

func (r *chatRigE2E) sourceCount(t *testing.T) int {
	t.Helper()
	return countRows(t, r.DB, `SELECT count(*) FROM kb_source`)
}

func TestKnowledgeChatCommandsOnlyInDirectChats(t *testing.T) {
	for _, channel := range []string{principal.ChannelTelegram, principal.ChannelDiscord} {
		owner := chatSender{channel: channel, class: principal.ClassOwner, direct: true}
		t.Run(channel+" owner in a direct chat", func(t *testing.T) {
			rig := newChatRigE2E(t)

			added := rig.say(t, owner, "/kb add support "+chatAddURL)
			if len(added) == 0 || !strings.HasPrefix(added[0], "added: ") || rig.sourceCount(t) != 2 {
				t.Fatalf("add replied %q with %d sources, want an added source", added, rig.sourceCount(t))
			}
			listed := rig.say(t, owner, "/kb list")
			if len(listed) != 2 || !strings.HasPrefix(listed[0], "COLLECTION\tSOURCES\t") || !strings.HasPrefix(listed[1], "support\t2\t") {
				t.Fatalf("list replied %q, want the table with the collection", listed)
			}
			previewed := rig.say(t, owner, "/kb remove support "+chatAddURL)
			if len(previewed) == 0 || !strings.HasPrefix(previewed[0], "would remove ") || rig.sourceCount(t) != 2 {
				t.Fatalf("remove without --apply replied %q with %d sources, want a preview only", previewed, rig.sourceCount(t))
			}
			removed := rig.say(t, owner, "/kb remove support "+chatAddURL+" --apply")
			if len(removed) == 0 || !strings.HasPrefix(removed[0], "removed: ") || rig.sourceCount(t) != 1 {
				t.Fatalf("remove replied %q with %d sources, want the source removed", removed, rig.sourceCount(t))
			}
		})
		t.Run(channel+" refuses a group or guild channel", func(t *testing.T) {
			rig := newChatRigE2E(t)
			before := rig.sourceCount(t)
			fetches := rig.kb.Site.Fetches()

			for _, text := range []string{"/kb add support " + chatAddURL, "/kb remove support " + knowledgetest.PageURL, "/kb list", "/kb nonsense"} {
				got := rig.say(t, chatSender{channel: channel, class: principal.ClassOwner}, text)
				if !slices.Equal(got, []string{command.ReplyDirectOnly}) {
					t.Errorf("%q outside a direct chat replied %q, want only the scope refusal", text, got)
				}
			}
			if rig.sourceCount(t) != before || rig.kb.Site.Fetches() != fetches {
				t.Errorf("sources changed from %d to %d or the page was fetched", before, rig.sourceCount(t))
			}
		})
		t.Run(channel+" refuses other principals in a direct chat", func(t *testing.T) {
			rig := newChatRigE2E(t)
			before := rig.sourceCount(t)
			fetches := rig.kb.Site.Fetches()

			for _, class := range []principal.Class{principal.ClassTrusted, principal.ClassPublic} {
				for _, text := range []string{"/kb add support " + chatAddURL, "/kb remove support " + knowledgetest.PageURL, "/kb embed support", "/kb list"} {
					got := rig.say(t, chatSender{channel: channel, class: class, direct: true}, text)
					if !slices.Equal(got, []string{command.ReplyDenied}) {
						t.Errorf("%q from a %s principal replied %q, want only the denial", text, class, got)
					}
				}
			}
			if rig.sourceCount(t) != before || rig.kb.Site.Fetches() != fetches || rig.kb.Provider.Sent() != 0 {
				t.Errorf("a refused sender changed %d sources to %d, fetched or sent text", before, rig.sourceCount(t))
			}
		})
	}
}

func TestKnowledgeChatCommandsAreAnsweredOnceWhenAMessageIsRedelivered(t *testing.T) {
	rig := newChatRigE2E(t)
	owner := chatSender{channel: principal.ChannelTelegram, class: principal.ClassOwner, direct: true}
	rig.messages = 40
	fetches := rig.kb.Site.Fetches()

	first := rig.say(t, owner, "/kb add support "+chatAddURL)
	rig.messages = 40
	second := rig.say(t, owner, "/kb add support "+chatAddURL)

	if !slices.Equal(first, second) || rig.sourceCount(t) != 2 {
		t.Fatalf("first %q second %q sources %d, want the same journaled reply and one new source", first, second, rig.sourceCount(t))
	}
	if runs := rig.handlers.handled("kb.add"); runs != 1 {
		t.Errorf("the add handler ran %d times for one message, want once", runs)
	}
	if got := rig.kb.Site.Fetches() - fetches; got != 1 {
		t.Errorf("the page was fetched %d times for one message, want once", got)
	}
}
