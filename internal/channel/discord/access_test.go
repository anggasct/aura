package discord

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	auraagent "github.com/anggasct/aura/internal/agent"
	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/principal"
)

var accessTestTools = []string{"read_file", "write_file", "list_dir", "exec", "web_fetch", "web_search"}

func accessPersonas() []config.AgentDefinition {
	return []config.AgentDefinition{
		{ID: "owneronly", Description: "d", Instructions: "i"},
		{
			ID: "team", Name: "Team", Description: "d", Instructions: "i", Audience: config.AudienceTrusted,
			Tools: []string{"read_file", "write_file"}, PrincipalTools: []string{"read_file", "write_file"},
			Messages: map[string]string{config.MessageWaitingForOwner: "Sebentar, aku tanya pemilik dulu."},
		},
		{
			ID: "lobby", Description: "d", Instructions: "i", Audience: config.AudiencePublic,
			Tools: []string{"read_file"}, PublicTools: []string{"read_file"},
		},
	}
}

type accessObserver struct {
	mu     sync.Mutex
	denied map[string]int
	routed map[string]int
	grants []StandingGrantRequest
}

func (o *accessObserver) settings() AccessSettings {
	return AccessSettings{
		ApprovalTimeout:  time.Minute,
		StandingGrantTTL: 168 * time.Hour,
		Grants:           o,
		Denied: func(_ context.Context, reason string) {
			o.mu.Lock()
			defer o.mu.Unlock()
			o.denied[reason]++
		},
		ApprovalRouted: func(_ context.Context, result string) {
			o.mu.Lock()
			defer o.mu.Unlock()
			o.routed[result]++
		},
	}
}

func (o *accessObserver) RecordStandingGrant(_ context.Context, grant *StandingGrantRequest) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.grants = append(o.grants, *grant)
	return nil
}

func (o *accessObserver) count(table map[string]int, key string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return table[key]
}

func newAccessAdapter(t *testing.T, persona string, owners []string, personas []config.AgentDefinition) (*Adapter, *memorySink, *accessObserver) {
	t.Helper()
	t.Setenv("AURA_DISCORD_BOT_TOKEN", testToken)
	cfg := testAdapterConfig()
	cfg.Accounts[0].Persona = persona
	registry, err := auraagent.Build(personas, accessTestTools, []string{"primary"})
	if err != nil {
		t.Fatalf("agent registry: %v", err)
	}
	router, err := auraagent.NewRouter(registry, &cfg, nil)
	if err != nil {
		t.Fatalf("persona router: %v", err)
	}
	adapter, err := New(&cfg, &cfg.Accounts[0], &memoryResumeStore{}, newFakeEffectRunner(), &fakeMediaStore{}, &fakeSessionEnsurer{}, slog.New(slog.NewTextHandler(&logCapture{}, nil)))
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	adapter.SetPersonaRouter(router)
	adapter.SetPrincipalResolver(testOwners(t, owners...))
	observer := &accessObserver{denied: map[string]int{}, routed: map[string]int{}}
	adapter.SetAccess(observer.settings())
	adapter.selfID = "999"
	sink := newMemorySink()
	adapter.sink = sink
	return adapter, sink, observer
}

func TestAudienceMatrix(t *testing.T) {
	stub := newRestStub(t, func(call int, w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})
	cases := []struct {
		persona string
		author  string
		admit   bool
		class   principal.Class
		denied  int
	}{
		{"owneronly", "777", true, principal.ClassOwner, 0},
		{"owneronly", "111", false, "", 1},
		{"owneronly", "666", false, "", 0},
		{"team", "777", true, principal.ClassOwner, 0},
		{"team", "111", true, principal.ClassTrusted, 0},
		{"team", "666", false, "", 0},
		{"lobby", "777", true, principal.ClassOwner, 0},
		{"lobby", "111", true, principal.ClassTrusted, 0},
		{"lobby", "666", true, principal.ClassPublic, 0},
	}
	for _, tc := range cases {
		t.Run(tc.persona+"/"+tc.author, func(t *testing.T) {
			adapter, sink, observer := newAccessAdapter(t, tc.persona, []string{"discord:777"}, accessPersonas())
			adapter.restBase = stub.srv.URL
			admitted, err := adapter.admit(t.Context(), textMessage("9001", "444", "", tc.author, "hello", nil))
			if err != nil || admitted != tc.admit {
				t.Fatalf("admit() = %v, %v; want %v", admitted, err, tc.admit)
			}
			if got := observer.count(observer.denied, DeniedReasonAudience); got != tc.denied {
				t.Errorf("audience denials = %d, want %d", got, tc.denied)
			}
			if !tc.admit {
				if sink.calls["message:9001"] != 0 {
					t.Fatal("dropped message reached the ingress sink")
				}
				return
			}
			env := routedEnvelope(t, sink, "message:9001")
			if env.PrincipalClass != string(tc.class) || env.AgentID != tc.persona {
				t.Fatalf("envelope class/persona = %q/%q, want %q/%q", env.PrincipalClass, env.AgentID, tc.class, tc.persona)
			}
			if tc.class == principal.ClassPublic && env.PrincipalID != "discord:666" {
				t.Fatalf("public principal = %q, want discord:666", env.PrincipalID)
			}
		})
	}
}

func TestPublicGuildMessagesNeedAMentionOutsideBotThreads(t *testing.T) {
	stub := newRestStub(t, func(call int, w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})
	adapter, sink, _ := newAccessAdapter(t, "lobby", []string{"discord:777"}, accessPersonas())
	adapter.restBase = stub.srv.URL
	if admitted, err := adapter.admit(t.Context(), textMessage("9101", "333", "222", "666", "hello", nil)); err != nil || admitted {
		t.Fatalf("unmentioned public guild message admit() = %v, %v; want dropped", admitted, err)
	}
	stub.mu.Lock()
	probes := len(stub.calls)
	stub.mu.Unlock()
	if probes != 0 {
		t.Fatalf("rest calls for an unmentioned public message = %d, want none", probes)
	}
	if admitted, err := adapter.admit(t.Context(), textMessage("9102", "333", "222", "666", "<@999> hello", []string{"999"})); err != nil || !admitted {
		t.Fatalf("mentioned public guild message admit() = %v, %v; want admitted", admitted, err)
	}
	if env := routedEnvelope(t, sink, "message:9102"); env.PrincipalClass != string(principal.ClassPublic) {
		t.Fatalf("mentioned message class = %q, want public", env.PrincipalClass)
	}
	adapter.rememberThread("9100", "777000", threadRoute{parent: "333", persona: "lobby"})
	if admitted, err := adapter.admit(t.Context(), textMessage("9103", "777000", "222", "666", "follow up", nil)); err != nil || !admitted {
		t.Fatalf("public follow-up in a bot thread admit() = %v, %v; want admitted without a mention", admitted, err)
	}
}

func TestOwnerOnlyPersonaDropsTrustedThreadFollowUps(t *testing.T) {
	adapter, sink, observer := newAccessAdapter(t, "owneronly", []string{"discord:777"}, accessPersonas())
	adapter.rememberThread("9200", "777001", threadRoute{parent: "333", persona: "owneronly"})
	if admitted, err := adapter.admit(t.Context(), textMessage("9201", "777001", "222", "111", "follow up", nil)); err != nil || admitted {
		t.Fatalf("trusted follow-up admit() = %v, %v; want dropped", admitted, err)
	}
	if sink.calls["message:9201"] != 0 || observer.count(observer.denied, DeniedReasonAudience) != 1 {
		t.Fatalf("dropped follow-up reached the sink or was not counted")
	}
}

func newOwnerRoutedAdapter(t *testing.T, stub *approvalStub, owners []string, class principal.Class) (*Adapter, *accessObserver) {
	t.Helper()
	adapter, _, observer := newAccessAdapter(t, "team", owners, accessPersonas())
	adapter.restBase = stub.srv.URL
	adapter.rememberTarget("dm:111", &approvalBinding{principal: "discord:111", class: class, persona: "team", actor: "111", channelID: "333"})
	return adapter, observer
}

func trustedPrompt(expiresAt time.Time) *ApprovalPrompt {
	prompt := testPrompt("dm:111", expiresAt)
	prompt.PrincipalID = "discord:111"
	prompt.ToolName = "write_file"
	return prompt
}

func (s *approvalStub) postsTo(channel string) []cardPost {
	s.mu.Lock()
	defer s.mu.Unlock()
	var matched []cardPost
	for _, post := range s.posts {
		if post.channel == channel {
			matched = append(matched, post)
		}
	}
	return matched
}

func (s *approvalStub) waitPostTo(t *testing.T, channel string, count int) []cardPost {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if posts := s.postsTo(channel); len(posts) >= count {
			return posts
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d posts to channel %s", count, channel)
	return nil
}

func cardButtons(t *testing.T, post cardPost) (labels, ids []string) {
	t.Helper()
	components, _ := post.body["components"].([]any)
	if len(components) != 1 {
		t.Fatalf("card components = %v, want one action row", post.body["components"])
	}
	row, _ := components[0].(map[string]any)
	buttons, _ := row["components"].([]any)
	for _, raw := range buttons {
		button, _ := raw.(map[string]any)
		label, _ := button["label"].(string)
		id, _ := button["custom_id"].(string)
		labels = append(labels, label)
		ids = append(ids, id)
	}
	return labels, ids
}

func TestTrustedApprovalIsRoutedToTheOwnerDM(t *testing.T) {
	stub := newApprovalStub(t)
	adapter, observer := newOwnerRoutedAdapter(t, stub, []string{"discord:777"}, principal.ClassTrusted)
	result := decideAsync(t.Context(), adapter, trustedPrompt(time.Now().Add(time.Minute)))
	card := stub.waitPostTo(t, "555", 1)[0]
	content, _ := card.body["content"].(string)
	if !strings.Contains(content, "persona: Team (team)") || !strings.Contains(content, "principal: discord:111") || !strings.Contains(content, "write_file") {
		t.Fatalf("owner card content = %q", content)
	}
	labels, ids := cardButtons(t, card)
	if strings.Join(labels, "|") != "Approve|Allow 7 days|Reject" {
		t.Fatalf("owner card buttons = %v", labels)
	}
	notices := stub.waitPostTo(t, "333", 1)
	if got, _ := notices[0].body["content"].(string); got != "Sebentar, aku tanya pemilik dulu." {
		t.Fatalf("requester notice = %q, want the persona waiting_for_owner message", got)
	}
	if len(stub.postsTo("333")) != 1 || len(stub.postsTo("555")) != 1 {
		t.Fatalf("posts = requester %d owner %d, want the card only in the owner DM", len(stub.postsTo("333")), len(stub.postsTo("555")))
	}
	adapter.resolveComponentClick(t.Context(), clickPayload("ix-requester-dm", "111", "", "555", ids[0]))
	adapter.resolveComponentClick(t.Context(), clickPayload("ix-requester-channel", "111", "", "333", ids[0]))
	select {
	case outcome := <-result:
		t.Fatalf("requester click decided the approval: %+v", outcome)
	case <-time.After(100 * time.Millisecond):
	}
	adapter.resolveComponentClick(t.Context(), clickPayload("ix-owner", "777", "", "555", ids[0]))
	if outcome := awaitDecide(t, result, 5*time.Second); outcome.err != nil || !outcome.accepted {
		t.Fatalf("owner approve = %v, %v; want accepted", outcome.accepted, outcome.err)
	}
	if observer.count(observer.routed, ApprovalRoutedApproved) != 1 || len(observer.grants) != 0 {
		t.Fatalf("routed = %v grants = %v, want one approval and no standing grant", observer.routed, observer.grants)
	}
}

func TestOwnerAllowSevenDaysRecordsAStandingGrant(t *testing.T) {
	stub := newApprovalStub(t)
	adapter, observer := newOwnerRoutedAdapter(t, stub, []string{"discord:777"}, principal.ClassTrusted)
	result := decideAsync(t.Context(), adapter, trustedPrompt(time.Now().Add(time.Minute)))
	_, ids := cardButtons(t, stub.waitPostTo(t, "555", 1)[0])
	adapter.resolveComponentClick(t.Context(), clickPayload("ix-standing", "777", "", "555", ids[1]))
	if outcome := awaitDecide(t, result, 5*time.Second); outcome.err != nil || !outcome.accepted {
		t.Fatalf("standing click = %v, %v; want accepted", outcome.accepted, outcome.err)
	}
	observer.mu.Lock()
	defer observer.mu.Unlock()
	want := StandingGrantRequest{PrincipalID: "discord:111", AgentID: "team", ToolName: "write_file", OwnerID: "discord:777", TTL: 168 * time.Hour}
	if len(observer.grants) != 1 || observer.grants[0] != want {
		t.Fatalf("standing grants = %+v, want %+v", observer.grants, want)
	}
	if observer.routed[ApprovalRoutedStanding] != 1 {
		t.Fatalf("routed = %v, want one standing result", observer.routed)
	}
}

func TestOwnerRejectionTellsTheRequester(t *testing.T) {
	stub := newApprovalStub(t)
	adapter, observer := newOwnerRoutedAdapter(t, stub, []string{"discord:777"}, principal.ClassTrusted)
	result := decideAsync(t.Context(), adapter, trustedPrompt(time.Now().Add(time.Minute)))
	_, ids := cardButtons(t, stub.waitPostTo(t, "555", 1)[0])
	adapter.resolveComponentClick(t.Context(), clickPayload("ix-reject", "777", "", "555", ids[2]))
	if outcome := awaitDecide(t, result, 5*time.Second); outcome.err != nil || outcome.accepted {
		t.Fatalf("reject = %v, %v; want declined", outcome.accepted, outcome.err)
	}
	notices := stub.waitPostTo(t, "333", 2)
	if got, _ := notices[1].body["content"].(string); got != config.PersonaMessageDefaults()[config.MessageApprovalDenied] {
		t.Fatalf("rejection notice = %q", got)
	}
	if observer.count(observer.routed, ApprovalRoutedRejected) != 1 {
		t.Fatalf("routed = %v, want one rejection", observer.routed)
	}
}

func TestOwnerApprovalTimeoutDeniesAndTellsTheRequester(t *testing.T) {
	stub := newApprovalStub(t)
	personas := accessPersonas()
	personas[1].ApprovalTimeout = config.Duration(80 * time.Millisecond)
	adapter, _, observer := newAccessAdapter(t, "team", []string{"discord:777"}, personas)
	adapter.restBase = stub.srv.URL
	adapter.rememberTarget("dm:111", &approvalBinding{principal: "discord:111", class: principal.ClassTrusted, persona: "team", actor: "111", channelID: "333"})
	accepted, err := adapter.Decide(t.Context(), trustedPrompt(time.Now().Add(time.Hour)))
	if code, ok := CodeOf(err); accepted || !ok || code != ErrorCodeApprovalTimeout {
		t.Fatalf("Decide() = %v, %v; want approval_timeout", accepted, err)
	}
	notices := stub.waitPostTo(t, "333", 2)
	if got, _ := notices[1].body["content"].(string); got != config.PersonaMessageDefaults()[config.MessageApprovalTimeout] {
		t.Fatalf("timeout notice = %q", got)
	}
	if _, _, patches := stub.counts(); patches != 1 {
		t.Fatalf("card disable patches = %d, want 1", patches)
	}
	if observer.count(observer.routed, ApprovalRoutedTimeout) != 1 {
		t.Fatalf("routed = %v, want one timeout", observer.routed)
	}
}

func TestOwnerApprovalUnavailableDenies(t *testing.T) {
	cases := map[string]struct {
		owners []string
		dmCode int
		dms    int
	}{
		"no owner discord identity": {owners: nil, dmCode: http.StatusOK, dms: 0},
		"owner dm cannot be opened": {owners: []string{"discord:777"}, dmCode: http.StatusForbidden, dms: 1},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			stub := newApprovalStub(t)
			stub.dmCode = tc.dmCode
			adapter, observer := newOwnerRoutedAdapter(t, stub, tc.owners, principal.ClassTrusted)
			accepted, err := adapter.Decide(t.Context(), trustedPrompt(time.Now().Add(time.Minute)))
			if code, ok := CodeOf(err); accepted || !ok || code != ErrorCodeApprovalUnavailable {
				t.Fatalf("Decide() = %v, %v; want approval_unavailable", accepted, err)
			}
			if posts, _, _ := stub.counts(); posts != 0 {
				t.Fatalf("posts = %d, want no card and no notice", posts)
			}
			stub.mu.Lock()
			dms := stub.dmCalls
			stub.mu.Unlock()
			if dms != tc.dms {
				t.Fatalf("dm opens = %d, want %d", dms, tc.dms)
			}
			if observer.count(observer.routed, ApprovalRoutedUnavailable) != 1 {
				t.Fatalf("routed = %v, want one unavailable", observer.routed)
			}
		})
	}
}

func TestPublicApprovalCardOffersNoStandingGrant(t *testing.T) {
	stub := newApprovalStub(t)
	adapter, _ := newOwnerRoutedAdapter(t, stub, []string{"discord:777"}, principal.ClassPublic)
	result := decideAsync(t.Context(), adapter, trustedPrompt(time.Now().Add(time.Minute)))
	labels, ids := cardButtons(t, stub.waitPostTo(t, "555", 1)[0])
	if strings.Join(labels, "|") != "Approve|Reject" {
		t.Fatalf("public card buttons = %v, want no standing grant", labels)
	}
	forged := strings.Replace(ids[0], ":"+approvalActionOk+":", ":"+approvalActionStanding+":", 1)
	adapter.resolveComponentClick(t.Context(), clickPayload("ix-forged", "777", "", "555", forged))
	adapter.resolveComponentClick(t.Context(), clickPayload("ix-reject", "777", "", "555", ids[1]))
	if outcome := awaitDecide(t, result, 5*time.Second); outcome.err != nil || outcome.accepted {
		t.Fatalf("Decide() = %v, %v; want the forged standing click ignored and the rejection applied", outcome.accepted, outcome.err)
	}
}

func TestOwnerDMChannelIsOpenedOnce(t *testing.T) {
	stub := newApprovalStub(t)
	adapter, _ := newOwnerRoutedAdapter(t, stub, []string{"discord:777"}, principal.ClassTrusted)
	for range 2 {
		result := decideAsync(t.Context(), adapter, trustedPrompt(time.Now().Add(time.Minute)))
		posts := len(stub.postsTo("555"))
		_, ids := cardButtons(t, stub.waitPostTo(t, "555", posts+1)[posts])
		adapter.resolveComponentClick(t.Context(), clickPayload("ix-approve-"+ids[0], "777", "", "555", ids[0]))
		awaitDecide(t, result, 5*time.Second)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if stub.dmCalls != 1 {
		t.Fatalf("dm opens = %d, want the channel cached after the first", stub.dmCalls)
	}
}

func TestStandingGrantLabelFollowsTheTTL(t *testing.T) {
	for ttl, want := range map[time.Duration]string{
		168 * time.Hour:  "Allow 7 days",
		24 * time.Hour:   "Allow 1 day",
		36 * time.Hour:   "Allow 36 hours",
		90 * time.Minute: "Allow 1h30m0s",
	} {
		if got := standingGrantLabel(ttl); got != want {
			t.Errorf("standingGrantLabel(%v) = %q, want %q", ttl, got, want)
		}
	}
}
