package discord

import (
	"net/http"
	"testing"

	"github.com/anggasct/aura/internal/config"
)

func TestGroupContextDecidesChannelSessions(t *testing.T) {
	stub := newRestStub(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})
	personas := accessPersonas()
	personas = append(personas, config.AgentDefinition{
		ID: "crew", Description: "d", Instructions: "i", Audience: config.AudienceTrusted, GroupContext: config.GroupContextShared,
	})
	cases := map[string]struct {
		persona string
		allow   []string
		authors map[string]string
	}{
		"public members are split per principal": {
			persona: "lobby",
			authors: map[string]string{
				"601": "lobby/discord:owner/channel:333/principal:discord:601",
				"602": "lobby/discord:owner/channel:333/principal:discord:602",
				"777": "lobby/discord:owner/channel:333",
			},
		},
		"trusted members are split by default": {
			persona: "team",
			allow:   []string{"112"},
			authors: map[string]string{
				"111": "team/discord:owner/channel:333/principal:discord:111",
				"112": "team/discord:owner/channel:333/principal:discord:112",
				"777": "team/discord:owner/channel:333",
			},
		},
		"shared trusted persona keeps one channel session": {
			persona: "crew",
			allow:   []string{"112"},
			authors: map[string]string{
				"111": "crew/discord:owner/channel:333",
				"112": "crew/discord:owner/channel:333",
				"777": "crew/discord:owner/channel:333",
			},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			adapter, sink, _ := newAccessAdapter(t, tc.persona, []string{"discord:777"}, personas)
			adapter.restBase = stub.srv.URL
			adapter.account.AllowedUserIDs = append(adapter.account.AllowedUserIDs, tc.allow...)
			index := 0
			for author, want := range tc.authors {
				index++
				id := "96" + string(rune('0'+index)) + author
				admitted, err := adapter.admit(t.Context(), textMessage(id, "333", "222", author, "<@999> hello", []string{"999"}))
				if err != nil || !admitted {
					t.Fatalf("admit(%s) = %v, %v; want admitted", author, admitted, err)
				}
				if got := routedEnvelope(t, sink, "message:"+id).ConversationID; got != want {
					t.Fatalf("author %s session = %q, want %q", author, got, want)
				}
			}
		})
	}
}

func TestThreadsStayOneSharedSessionForEveryMember(t *testing.T) {
	adapter, sink, _ := newAccessAdapter(t, "lobby", []string{"discord:777"}, accessPersonas())
	adapter.rememberThread("9700", "7701", threadRoute{parent: "333", persona: "lobby"})
	for i, author := range []string{"601", "602", "777"} {
		id := "970" + string(rune('1'+i))
		if admitted, err := adapter.admit(t.Context(), textMessage(id, "7701", "222", author, "follow up", nil)); err != nil || !admitted {
			t.Fatalf("admit(%s) = %v, %v", author, admitted, err)
		}
		if got := routedEnvelope(t, sink, "message:"+id).ConversationID; got != "lobby/discord:owner/thread:7701" {
			t.Fatalf("author %s thread session = %q, want the shared thread session", author, got)
		}
	}
}
