package discord

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestRenderMentions(t *testing.T) {
	self := "999"
	cases := map[string]struct {
		content  string
		mentions []userPayload
		selfName string
		want     string
	}{
		"self from mentions": {
			content:  "apa kabar <@999>",
			mentions: []userPayload{{ID: "999", Username: "Aura"}},
			want:     "apa kabar @Aura",
		},
		"nick beats global name beats username": {
			content: "<@1> <@2> <@3>",
			mentions: []userPayload{
				{ID: "1", Username: "u1", GlobalName: "g1", Member: &memberPayload{Nick: "n1"}},
				{ID: "2", Username: "u2", GlobalName: "g2"},
				{ID: "3", Username: "u3"},
			},
			want: "@n1 @g2 @u3",
		},
		"nickname form renders like the plain form": {
			content:  "hi <@!1>",
			mentions: []userPayload{{ID: "1", Username: "budi"}},
			want:     "hi @budi",
		},
		"self falls back to the ready identity": {
			content:  "<@999>",
			selfName: "Aura",
			want:     "@Aura",
		},
		"mentions override the ready identity": {
			content:  "<@999>",
			mentions: []userPayload{{ID: "999", Username: "Aura", Member: &memberPayload{Nick: "Asisten"}}},
			selfName: "Aura",
			want:     "@Asisten",
		},
		"unresolved, role and channel markup stay": {
			content:  "<@42> <@&1> <#2> <:wave:3> <t:4>",
			mentions: []userPayload{{ID: "999", Username: "Aura"}},
			want:     "<@42> <@&1> <#2> <:wave:3> <t:4>",
		},
		"names are sanitized": {
			content: "<@1> <@2> <@3>",
			mentions: []userPayload{
				{ID: "1", Username: "multi\nline\tname"},
				{ID: "2", Username: "zero\u200bwidth\u0007bell"},
				{ID: "3", Username: strings.Repeat("x", 40)},
			},
			want: "@multi line name @zerowidthbell @" + strings.Repeat("x", 32),
		},
		"a name that sanitizes to empty keeps the markup": {
			content:  "<@1>",
			mentions: []userPayload{{ID: "1", Username: "\u200b\n"}},
			want:     "<@1>",
		},
		"no names leaves content untouched": {
			content: "plain <@1>",
			want:    "plain <@1>",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := renderMentions(tc.content, tc.mentions, self, tc.selfName); got != tc.want {
				t.Fatalf("renderMentions() = %q, want %q", got, tc.want)
			}
		})
	}
}

func envelopeText(t *testing.T, sink *memorySink, externalID string) string {
	t.Helper()
	sink.mu.Lock()
	defer sink.mu.Unlock()
	env := sink.envelopes[externalID]
	if env == nil || len(env.Parts) == 0 {
		t.Fatalf("no envelope text for %s", externalID)
		return ""
	}
	return env.Parts[0].Text
}

func createdThreadNames(stub *restStub) []string {
	stub.mu.Lock()
	defer stub.mu.Unlock()
	var names []string
	for _, call := range stub.calls {
		if call.method != http.MethodPost || !strings.HasSuffix(call.path, "/threads") {
			continue
		}
		var body struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(call.body, &body)
		names = append(names, body.Name)
	}
	return names
}

func TestMentionTextReachesTheEnvelopeAndTheThreadName(t *testing.T) {
	sink := newMemorySink()
	stub := threadCreateStub(t, "777101")
	adapter := newThreadAdapter(t, sink, &fakeSessionEnsurer{}, stub)
	msg := threadTrigger("9101", "apa kabar <@999>", nil)
	msg.Mentions = []userPayload{{ID: "999", Username: "Aura", Bot: true}}

	admitted, err := adapter.admit(t.Context(), msg)
	if err != nil || !admitted {
		t.Fatalf("admit() = %v, %v; want admission", admitted, err)
	}
	if got := envelopeText(t, sink, "message:9101"); got != "apa kabar @Aura" {
		t.Fatalf("envelope text = %q, want %q", got, "apa kabar @Aura")
	}
	if names := createdThreadNames(stub); len(names) != 1 || names[0] != "apa kabar @Aura" {
		t.Fatalf("thread names = %q, want [apa kabar @Aura]", names)
	}
}

func TestBareSelfMentionIsAdmittedAsTheBotName(t *testing.T) {
	sink := newMemorySink()
	stub := threadCreateStub(t, "777102")
	adapter := newThreadAdapter(t, sink, &fakeSessionEnsurer{}, stub)
	adapter.selfName = "Aura"

	admitted, err := adapter.admit(t.Context(), threadTrigger("9102", "<@999>", []string{"999"}))
	if err != nil || !admitted {
		t.Fatalf("admit() = %v, %v; want admission", admitted, err)
	}
	if got := envelopeText(t, sink, "message:9102"); got != "@Aura" {
		t.Fatalf("envelope text = %q, want @Aura", got)
	}
}

func TestMentionTextInThreadsAndDMs(t *testing.T) {
	sink := newMemorySink()
	stub := threadCreateStub(t, "777103")
	adapter := newThreadAdapter(t, sink, &fakeSessionEnsurer{}, stub)
	adapter.selfName = "Aura"

	if _, err := adapter.admit(t.Context(), threadTrigger("9103", "open <@999>", []string{"999"})); err != nil {
		t.Fatalf("trigger admit(): %v", err)
	}
	follow := threadFollowUp("9104", "777103", "tanya <@555> soal ini")
	follow.Mentions = []userPayload{{ID: "555", GlobalName: "Budi"}}
	if admitted, err := adapter.admit(t.Context(), follow); err != nil || !admitted {
		t.Fatalf("in-thread admit() = %v, %v; want admission", admitted, err)
	}
	if got := envelopeText(t, sink, "message:9104"); got != "tanya @Budi soal ini" {
		t.Fatalf("thread text = %q, want %q", got, "tanya @Budi soal ini")
	}

	dm := &messagePayload{ID: "9105", ChannelID: "dm-1", Author: userPayload{ID: "111"}, Content: "hai <@999>", Type: 0}
	if admitted, err := adapter.admit(t.Context(), dm); err != nil || !admitted {
		t.Fatalf("dm admit() = %v, %v; want admission", admitted, err)
	}
	if got := envelopeText(t, sink, "message:9105"); got != "hai @Aura" {
		t.Fatalf("dm text = %q, want %q", got, "hai @Aura")
	}
}

func TestReadyRecordsTheBotDisplayName(t *testing.T) {
	adapter := newThreadAdapter(t, newMemorySink(), &fakeSessionEnsurer{}, threadCreateStub(t, "777104"))
	data, err := json.Marshal(readyPayload{SessionID: "s-1", User: userPayload{ID: "999", Username: "aura", GlobalName: "Aura"}})
	if err != nil {
		t.Fatalf("marshal ready: %v", err)
	}
	if err := adapter.handleDispatch(t.Context(), &gatewayEnvelope{Type: eventReady, Data: data}, nil); err != nil {
		t.Fatalf("handleDispatch(READY): %v", err)
	}
	if adapter.selfID != "999" || adapter.selfName != "Aura" {
		t.Fatalf("self = %q %q, want 999 Aura", adapter.selfID, adapter.selfName)
	}
}
