package email

import (
	"context"
	"crypto/sha256"
	"encoding/base32"
	"encoding/json"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/turn"
	runtimechannelhost "github.com/anggasct/aura/internal/platform/turn/channelhost"
	"github.com/anggasct/aura/internal/store/storetest"
)

func wantDigest(text string) string {
	sum := sha256.Sum256([]byte(text))
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum[:]))[:26]
}

func TestNew_RejectsMissingDependencies(t *testing.T) {
	cfg := &config.Email{}
	account := &config.EmailAccount{Name: testAccount}
	effects := newEffects(t, storetest.OpenMigrated(t))
	cases := map[string]func() (*Adapter, error){
		"nil config": func() (*Adapter, error) {
			return New(nil, account, newFakeMailbox(), &fakeMailer{}, newMemoryCursors(), effects, newMemorySessions(), nil)
		},
		"nil account": func() (*Adapter, error) {
			return New(cfg, nil, newFakeMailbox(), &fakeMailer{}, newMemoryCursors(), effects, newMemorySessions(), nil)
		},
		"unnamed": func() (*Adapter, error) {
			return New(cfg, &config.EmailAccount{}, newFakeMailbox(), &fakeMailer{}, newMemoryCursors(), effects, newMemorySessions(), nil)
		},
		"nil connector": func() (*Adapter, error) {
			return New(cfg, account, nil, &fakeMailer{}, newMemoryCursors(), effects, newMemorySessions(), nil)
		},
		"nil mailer": func() (*Adapter, error) {
			return New(cfg, account, newFakeMailbox(), nil, newMemoryCursors(), effects, newMemorySessions(), nil)
		},
		"nil cursors": func() (*Adapter, error) {
			return New(cfg, account, newFakeMailbox(), &fakeMailer{}, nil, effects, newMemorySessions(), nil)
		},
		"nil effects": func() (*Adapter, error) {
			return New(cfg, account, newFakeMailbox(), &fakeMailer{}, newMemoryCursors(), nil, newMemorySessions(), nil)
		},
		"nil sessions": func() (*Adapter, error) {
			return New(cfg, account, newFakeMailbox(), &fakeMailer{}, newMemoryCursors(), effects, nil, nil)
		},
	}
	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			adapter, err := build()
			if code, ok := CodeOf(err); !ok || code != ErrorCodeInvalidArgument || adapter != nil {
				t.Fatalf("New() = %v, %v; want invalid_argument and no adapter", adapter, err)
			}
		})
	}
}

func TestStart_RejectsNilSink(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	if code, ok := CodeOf(h.adapter.Start(t.Context(), nil)); !ok || code != ErrorCodeInvalidArgument {
		t.Fatalf("Start(nil) code = %q, want invalid_argument", code)
	}
}

func TestIngest_OwnerMessageStartsOneThreadTurn(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.start()
	h.deliver(mailSpec{subject: "Plan the week", body: "Please draft the agenda.", messageID: "m1@example.com"}.raw())

	envelopes := h.sink.all()
	if len(envelopes) != 1 {
		t.Fatalf("envelopes = %d, want 1 (drops %v)", len(envelopes), h.dropReasons())
	}
	env := envelopes[0]
	wantSession := testPersona + "/email:" + testAccount + "/thread:" + wantDigest("m1@example.com")
	if env.Source != "email:work" || env.AgentID != testPersona || env.ConversationID != wantSession {
		t.Errorf("envelope routing = %q %q %q, want email:work %s %s", env.Source, env.AgentID, env.ConversationID, testPersona, wantSession)
	}
	if env.PrincipalID != principal.OwnerID || env.PrincipalClass != string(principal.ClassOwner) {
		t.Errorf("principal = %q class %q, want the owner", env.PrincipalID, env.PrincipalClass)
	}
	if env.ExternalID != "email:work:m1@example.com" {
		t.Errorf("ExternalID = %q, want the source and Message-ID", env.ExternalID)
	}
	if len(env.Parts) != 1 || env.Parts[0].Text != "Subject: Plan the week\n\nPlease draft the agenda." {
		t.Errorf("parts = %+v", env.Parts)
	}
	var reply replyReference
	if err := json.Unmarshal(env.ReplyContext, &reply); err != nil || reply.MessageID != "m1@example.com" || reply.Sender != ownerAddress || reply.Subject != "Plan the week" {
		t.Errorf("reply context = %s (%v), want the Message-ID, authenticated sender and subject", env.ReplyContext, err)
	}
	if got := h.admittedClasses(); len(got) != 1 || got[0] != principal.ClassOwner {
		t.Errorf("admitted = %v, want one owner turn", got)
	}
	if got := h.cursors.get("email:work"); got.LastUID != 1 {
		t.Errorf("cursor = %+v, want last uid 1", got)
	}
	if owner, found, _ := h.sessions.SessionOwner(t.Context(), wantSession); !found || owner != principal.OwnerID {
		t.Errorf("session owner = %q found %v, want the owner", owner, found)
	}
	if len(h.mailbox.bodies) != 1 || len(h.mailbox.headers) != 1 {
		t.Errorf("fetches = %d headers %d bodies, want one of each", len(h.mailbox.headers), len(h.mailbox.bodies))
	}
}

func TestIngest_TrustedSenderStartsATrustedTurn(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.start()
	h.deliver(mailSpec{from: friendAddress, subject: "Hi", messageID: "t1@example.com"}.raw())

	envelopes := h.sink.all()
	if len(envelopes) != 1 {
		t.Fatalf("envelopes = %d, want 1 (drops %v)", len(envelopes), h.dropReasons())
	}
	if envelopes[0].PrincipalID != "email:"+friendAddress || envelopes[0].PrincipalClass != string(principal.ClassTrusted) {
		t.Errorf("principal = %q class %q, want the trusted friend", envelopes[0].PrincipalID, envelopes[0].PrincipalClass)
	}
}

func TestIngest_RepeatMessageIDCreatesNoSecondTurn(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.start()
	first := mailSpec{subject: "Once", messageID: "dup@example.com"}.raw()
	h.deliver(first)
	h.deliver(first, mailSpec{subject: "Once again", messageID: "dup@example.com", body: "different body"}.raw())

	envelopes := h.sink.all()
	if len(envelopes) != 3 {
		t.Fatalf("envelopes offered to the runtime = %d, want 3", len(envelopes))
	}
	for _, env := range envelopes {
		if env.ExternalID != envelopes[0].ExternalID {
			t.Fatalf("ExternalID %q differs from %q for the same Message-ID", env.ExternalID, envelopes[0].ExternalID)
		}
	}
	if turns := h.sink.turns(); turns != 1 {
		t.Fatalf("distinct turns = %d, want 1", turns)
	}
}

func TestIngest_AuthenticationFailuresDropEveryClass(t *testing.T) {
	forgedTop := "Authentication-Results: mx.attacker.net; dmarc=pass header.from=example.com"
	cases := map[string]mailSpec{
		"no authentication results":                {noAuth: true},
		"dmarc fail":                               {auth: testAuthserv + "; dmarc=fail header.from=example.com"},
		"dmarc none":                               {auth: testAuthserv + "; dmarc=none header.from=example.com"},
		"dmarc pass for another domain":            {auth: testAuthserv + "; dmarc=pass header.from=attacker.net"},
		"forged header with another id only":       {noAuth: true, extra: []string{forgedTop}},
		"forged header above a failing trusted":    {auth: testAuthserv + "; dmarc=fail header.from=example.com", extra: []string{forgedTop}},
		"forged header below a failing trusted":    {auth: testAuthserv + "; dmarc=fail header.from=example.com", below: []string{"Authentication-Results: " + testAuthserv + "; dmarc=pass header.from=example.com"}},
		"forged header below a silent trusted":     {auth: testAuthserv + "; spf=pass smtp.mailfrom=example.com", below: []string{"Authentication-Results: " + testAuthserv + "; dmarc=pass header.from=example.com"}},
		"forged header below another domain pass":  {auth: testAuthserv + "; dmarc=pass header.from=attacker.net", below: []string{"Authentication-Results: " + testAuthserv + "; dmarc=pass header.from=example.com"}},
		"forged header with a lookalike id":        {noAuth: true, extra: []string{"Authentication-Results: " + testAuthserv + ".attacker.net; dmarc=pass header.from=example.com"}},
		"unreadable trusted header over a forgery": {auth: testAuthserv + "; dmarc=pass (", below: []string{"Authentication-Results: " + testAuthserv + "; dmarc=pass header.from=example.com"}},
	}
	for name, spec := range cases {
		for _, from := range []string{ownerAddress, friendAddress, strangerAddr} {
			t.Run(name+"/"+from, func(t *testing.T) {
				h := newHarness(t, harnessOptions{audience: config.AudiencePublic})
				h.start()
				spec.from = from
				spec.messageID = "forged@example.com"
				if from == strangerAddr {
					spec.auth = strings.ReplaceAll(spec.auth, "header.from=example.com", "header.from=example.net")
				}
				h.deliver(spec.raw())
				h.wantTurns(0)
				h.wantDrops("sender_unauthenticated")
				if len(h.mailbox.bodies) != 0 {
					t.Errorf("fetched %d bodies for unauthenticated mail", len(h.mailbox.bodies))
				}
			})
		}
	}
}

func TestIngest_AuthenticatedMailFromTheSameShapesIsAdmitted(t *testing.T) {
	cases := map[string]mailSpec{
		"forged header above the trusted pass": {extra: []string{"Authentication-Results: mx.attacker.net; dmarc=fail header.from=example.com"}},
		"trusted header with a comment":        {auth: testAuthserv + " (Postfix); dmarc=pass header.from=example.com"},
		"subdomain sender with its own pass":   {from: "owner@mail.example.com", auth: testAuthserv + "; dmarc=pass header.from=mail.example.com"},
	}
	for name, spec := range cases {
		t.Run(name, func(t *testing.T) {
			owners := []string{"email:" + ownerAddress, "email:owner@mail.example.com"}
			h := newHarness(t, harnessOptions{owners: owners})
			h.start()
			spec.messageID = "ok@example.com"
			h.deliver(spec.raw())
			h.wantTurns(1)
		})
	}
}

func TestIngest_IgnoredMailDropsBeforeAuthentication(t *testing.T) {
	cases := map[string]mailSpec{
		"auto-replied":           {extra: []string{"Auto-Submitted: auto-replied"}},
		"auto-generated":         {extra: []string{"Auto-Submitted: auto-generated"}},
		"auto-notified":          {extra: []string{"Auto-Submitted: auto-notified"}},
		"list id":                {extra: []string{"List-Id: <team.example.com>"}},
		"precedence bulk":        {extra: []string{"Precedence: bulk"}},
		"precedence list":        {extra: []string{"Precedence: list"}},
		"precedence junk":        {extra: []string{"Precedence: junk"}},
		"the account's own":      {from: testAddress},
		"own mail in other case": {rawFrom: []string{"From: Aura <" + strings.ToUpper(testAddress) + ">"}},
	}
	for name, spec := range cases {
		for _, noAuth := range []bool{false, true} {
			t.Run(name, func(t *testing.T) {
				h := newHarness(t, harnessOptions{})
				h.start()
				spec.noAuth = noAuth
				spec.messageID = "auto@example.com"
				h.deliver(spec.raw())
				h.wantTurns(0)
				h.wantDrops("automated_mail")
				if len(h.mailbox.bodies) != 0 {
					t.Errorf("fetched %d bodies for ignored mail", len(h.mailbox.bodies))
				}
			})
		}
	}
}

func TestIngest_AutoSubmittedNoIsOrdinaryMail(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.start()
	h.deliver(mailSpec{extra: []string{"Auto-Submitted: no"}, messageID: "human@example.com"}.raw())
	h.wantTurns(1)
}

func TestIngest_InvalidSendersDropWithTheirReason(t *testing.T) {
	cases := map[string]mailSpec{
		"two addresses in one header": {rawFrom: []string{"From: owner@example.com, friend@example.com"}},
		"two from headers":            {rawFrom: []string{"From: owner@example.com", "From: friend@example.com"}},
		"unparsable from":             {rawFrom: []string{"From: not an address"}},
		"group from":                  {rawFrom: []string{"From: Team: owner@example.com;"}},
		"missing from":                {rawFrom: []string{"X-Note: none"}},
		"no message id":               {noID: true},
		"two message ids":             {extraID: "second@example.com"},
	}
	for name, spec := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, harnessOptions{})
			h.start()
			if !spec.noID {
				spec.messageID = "invalid@example.com"
			}
			h.deliver(spec.raw())
			h.wantTurns(0)
			h.wantDrops("sender_invalid")
		})
	}
}

func TestIngest_DropReasons(t *testing.T) {
	cases := map[string]struct {
		opts   harnessOptions
		mutate func(h *harness)
		spec   mailSpec
		want   string
	}{
		"authenticated stranger": {
			spec: mailSpec{from: strangerAddr, auth: testAuthserv + "; dmarc=pass header.from=example.net"},
			want: "author_not_allowed",
		},
		"trusted sender to an owner persona": {
			opts: harnessOptions{audience: config.AudienceOwner},
			spec: mailSpec{from: friendAddress},
			want: "audience",
		},
		"no persona routed": {
			mutate: func(h *harness) { h.adapter.SetPersonaRouter(nil) },
			spec:   mailSpec{},
			want:   "persona_unrouted",
		},
		"persona never evaluated": {
			opts: harnessOptions{evals: staticEvals{}},
			spec: mailSpec{from: friendAddress},
			want: "persona_eval_missing",
		},
		"persona eval lookup fails": {
			opts: harnessOptions{evals: staticEvals{err: errBoom}},
			spec: mailSpec{from: friendAddress},
			want: "persona_eval_missing",
		},
		"rate limited": {
			opts: harnessOptions{budgets: failingBudgets{refusal: config.MessageRateLimited}},
			spec: mailSpec{from: friendAddress},
			want: "rate_limited",
		},
		"budget exhausted": {
			opts: harnessOptions{budgets: failingBudgets{refusal: config.MessageBudgetExhausted}},
			spec: mailSpec{from: friendAddress},
			want: "budget_exhausted",
		},
		"budget lookup fails": {
			opts: harnessOptions{budgets: failingBudgets{err: errBoom}},
			spec: mailSpec{from: friendAddress},
			want: "budget_check_failed",
		},
		"empty message": {
			spec: mailSpec{body: " "},
			want: "empty_content",
		},
		"unreadable header": {
			spec: mailSpec{rawFrom: []string{"garbage without a colon"}},
			want: "message_malformed",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, tc.opts)
			if tc.mutate != nil {
				tc.mutate(h)
			}
			h.start()
			tc.spec.messageID = "reason@example.com"
			h.deliver(tc.spec.raw())
			h.wantTurns(0)
			h.wantDrops(tc.want)
		})
	}
}

func TestIngest_OwnerBypassesBudgetsAndEvals(t *testing.T) {
	h := newHarness(t, harnessOptions{
		audience: config.AudienceTrusted,
		budgets:  failingBudgets{refusal: config.MessageRateLimited},
		evals:    staticEvals{},
	})
	h.start()
	h.deliver(mailSpec{messageID: "owner@example.com"}.raw())
	h.wantTurns(1)
}

func TestIngest_BudgetsAndEvalsPassForTrustedSenders(t *testing.T) {
	h := newHarness(t, harnessOptions{budgets: failingBudgets{}, evals: staticEvals{evaluated: true}})
	h.start()
	h.deliver(mailSpec{from: friendAddress}.raw())
	h.wantTurns(1)
}

func TestIngest_SizeBoundsDropBeforeFetching(t *testing.T) {
	t.Run("oversized message", func(t *testing.T) {
		h := newHarness(t, harnessOptions{email: func(c *config.Email) { c.MaxAttachmentBytes = 1 }})
		h.start()
		uid := h.mailbox.add(mailSpec{messageID: "big@example.com"}.raw())
		h.mailbox.setSize(uid, 1+messageOverhead+1)
		h.wakeCycle()
		h.wantTurns(0)
		h.wantDrops("message_too_large")
		if len(h.mailbox.headers) != 0 || len(h.mailbox.bodies) != 0 {
			t.Errorf("fetched %d headers and %d bodies for an oversized message", len(h.mailbox.headers), len(h.mailbox.bodies))
		}
	})
	t.Run("message at the bound", func(t *testing.T) {
		h := newHarness(t, harnessOptions{email: func(c *config.Email) { c.MaxAttachmentBytes = 1 }})
		h.start()
		uid := h.mailbox.add(mailSpec{messageID: "edge@example.com"}.raw())
		h.mailbox.setSize(uid, 1+messageOverhead)
		h.wakeCycle()
		h.wantTurns(1)
	})
	t.Run("oversized header", func(t *testing.T) {
		h := newHarness(t, harnessOptions{})
		h.start()
		h.deliver(mailSpec{messageID: "wide@example.com", extra: []string{"X-Pad: " + strings.Repeat("a", maxHeaderBytes+10)}}.raw())
		h.wantTurns(0)
		h.wantDrops("message_too_large")
		if len(h.mailbox.bodies) != 0 {
			t.Errorf("fetched %d bodies for an oversized header", len(h.mailbox.bodies))
		}
	})
	t.Run("body past the bound", func(t *testing.T) {
		h := newHarness(t, harnessOptions{email: func(c *config.Email) { c.MaxAttachmentBytes = 1 }})
		h.start()
		raw := mailSpec{messageID: "grew@example.com", body: strings.Repeat("a", messageOverhead+64)}.raw()
		uid := h.mailbox.add(raw)
		h.mailbox.setSize(uid, 100)
		h.wakeCycle()
		h.wantTurns(0)
		h.wantDrops("message_too_large")
	})
}

func TestIngest_LongBodyIsBoundedToTheTurnInputLimit(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.start()
	h.deliver(mailSpec{messageID: "long@example.com", body: strings.Repeat("line of text\r\n", maxTurnInputBytes)}.raw())
	envelopes := h.sink.all()
	if len(envelopes) != 1 {
		t.Fatalf("envelopes = %d, want 1", len(envelopes))
	}
	text := envelopes[0].Parts[0].Text
	if len(text) > maxTurnInputBytes+len(truncationNotice) || !strings.HasSuffix(text, truncationNotice) {
		t.Errorf("turn input is %d bytes without the truncation notice", len(text))
	}
}

func TestIngest_VanishedMessageIsSkipped(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.start()
	h.mailbox.addGhost(mailSpec{messageID: "ghost@example.com"}.raw())
	h.deliver(mailSpec{messageID: "real@example.com"}.raw())
	h.wantTurns(1)
	h.wantDrops("message_unavailable")
	if got := h.cursors.get("email:work"); got.LastUID != 2 {
		t.Errorf("cursor = %+v, want last uid 2", got)
	}
}

func TestThreadSessions_SeparatePrincipalsNeverShareASession(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.start()
	root := "root@example.com"
	base := testPersona + "/email:" + testAccount + "/thread:" + wantDigest(root)
	friendKeyed := base + "/principal:" + wantDigest("email:"+friendAddress)
	ownerKeyed := func(rootID string) string {
		return testPersona + "/email:" + testAccount + "/thread:" + wantDigest(rootID) + "/principal:" + wantDigest(principal.OwnerID)
	}
	h.deliver(mailSpec{messageID: root}.raw())
	h.deliver(mailSpec{messageID: "r1@example.com", references: "<" + root + ">"}.raw())
	h.deliver(mailSpec{messageID: "r2@example.com", inReplyTo: "<" + root + ">"}.raw())
	h.deliver(mailSpec{from: friendAddress, messageID: "f1@example.com", references: "<" + root + "> <r1@example.com>"}.raw())
	h.deliver(mailSpec{from: friendAddress, messageID: "f2@example.com", inReplyTo: "<f1@example.com>", references: "<" + root + ">"}.raw())
	h.deliver(mailSpec{from: friendAddress, messageID: "own@example.com"}.raw())
	h.deliver(mailSpec{messageID: "o2@example.com", references: "<own@example.com>"}.raw())
	h.deliver(mailSpec{from: strangerAddr, auth: testAuthserv + "; dmarc=pass header.from=example.net", messageID: "s1@example.com", references: "<" + root + ">"}.raw())

	want := []string{base, base, base, friendKeyed, friendKeyed, testPersona + "/email:" + testAccount + "/thread:" + wantDigest("own@example.com"), ownerKeyed("own@example.com")}
	envelopes := h.sink.all()
	if len(envelopes) != len(want) {
		t.Fatalf("envelopes = %d, want %d (drops %v)", len(envelopes), len(want), h.dropReasons())
	}
	for i, env := range envelopes {
		if env.ConversationID != want[i] {
			t.Errorf("envelope %d session = %q, want %q", i, env.ConversationID, want[i])
		}
	}
	if owner, _, _ := h.sessions.SessionOwner(t.Context(), base); owner != principal.OwnerID {
		t.Errorf("thread session owner = %q, want the owner", owner)
	}
	if owner, _, _ := h.sessions.SessionOwner(t.Context(), friendKeyed); owner != "email:"+friendAddress {
		t.Errorf("keyed session owner = %q, want the friend", owner)
	}
	h.wantDrops("author_not_allowed")
	if h.sessions.count() != 4 {
		t.Errorf("sessions = %d, want 4 (the stranger created none)", h.sessions.count())
	}
}

func TestThreadSessions_FirstReferenceWinsOverLaterOnes(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.start()
	h.deliver(mailSpec{messageID: "late@example.com", references: "<first@example.com> <second@example.com>", inReplyTo: "<second@example.com>"}.raw())
	envelopes := h.sink.all()
	if len(envelopes) != 1 {
		t.Fatalf("envelopes = %d, want 1", len(envelopes))
	}
	if want := testPersona + "/email:" + testAccount + "/thread:" + wantDigest("first@example.com"); envelopes[0].ConversationID != want {
		t.Errorf("session = %q, want %q", envelopes[0].ConversationID, want)
	}
}

func TestCursor_FirstConnectionIngestsNoExistingMail(t *testing.T) {
	mailbox := newFakeMailbox()
	mailbox.add(mailSpec{messageID: "old1@example.com"}.raw())
	mailbox.add(mailSpec{messageID: "old2@example.com"}.raw())
	h := newHarness(t, harnessOptions{mailbox: mailbox})
	h.start()
	h.wantTurns(0)
	if got := h.cursors.get("email:work"); got != (Cursor{UIDValidity: 1, LastUID: 2}) {
		t.Fatalf("cursor = %+v, want validity 1 uid 2", got)
	}
	h.deliver(mailSpec{messageID: "new@example.com"}.raw())
	h.wantTurns(1)
}

func TestCursor_SurvivesRestart(t *testing.T) {
	mailbox, cursors, sessions := newFakeMailbox(), newMemoryCursors(), newMemorySessions()
	first := newHarness(t, harnessOptions{mailbox: mailbox, cursors: cursors, sessions: sessions})
	first.start()
	first.deliver(mailSpec{messageID: "a@example.com"}.raw(), mailSpec{messageID: "b@example.com"}.raw())
	first.wantTurns(2)
	first.stop()

	mailbox.add(mailSpec{messageID: "while-down@example.com"}.raw())
	second := newHarness(t, harnessOptions{mailbox: mailbox, cursors: cursors, sessions: sessions})
	second.start()
	second.wantTurns(1)
	if id := second.sink.all()[0].ExternalID; id != "email:work:while-down@example.com" {
		t.Errorf("restart ingested %q, want only the mail that arrived while down", id)
	}
	second.deliver(mailSpec{messageID: "c@example.com"}.raw())
	second.wantTurns(2)
	if got := cursors.get("email:work"); got.LastUID != 4 {
		t.Errorf("cursor = %+v, want last uid 4", got)
	}
}

func TestCursor_ChangedValidityIngestsNoOldMailAndReportsHealth(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.start()
	h.deliver(mailSpec{messageID: "before@example.com"}.raw())
	h.wantTurns(1)
	if findings := h.adapter.Check(t.Context()); len(findings) != 0 {
		t.Fatalf("findings before the change = %+v", findings)
	}

	h.mailbox.reset(2)
	h.mailbox.add(mailSpec{messageID: "renumbered1@example.com"}.raw())
	h.mailbox.add(mailSpec{messageID: "renumbered2@example.com"}.raw())
	h.wakeCycle()
	h.wantTurns(1)
	if got := h.cursors.get("email:work"); got != (Cursor{UIDValidity: 2, LastUID: 2}) {
		t.Fatalf("cursor = %+v, want validity 2 uid 2", got)
	}
	findings := h.adapter.Check(t.Context())
	if len(findings) != 1 || findings[0].Code != "email_uidvalidity_changed" || findings[0].Component != "email" {
		t.Fatalf("findings = %+v, want the uidvalidity finding", findings)
	}
	if strings.Contains(findings[0].Detail, ownerAddress) {
		t.Error("finding detail carries an address")
	}

	h.deliver(mailSpec{messageID: "after@example.com"}.raw())
	h.wantTurns(2)
	if id := h.sink.all()[1].ExternalID; id != "email:work:after@example.com" {
		t.Errorf("second turn = %q, want the mail after the change", id)
	}
}

func TestCursor_DoesNotAdvancePastARetryableFailure(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.start()
	h.sink.err = &turn.Error{Code: turn.ErrorCodeRuntimeOverloaded}
	h.mailbox.add(mailSpec{messageID: "pending@example.com"}.raw())
	h.mailbox.wake <- struct{}{}
	h.awaitReconnect()
	if got := h.cursors.get("email:work"); got.LastUID != 0 {
		t.Fatalf("cursor advanced to %+v past a failed intake", got)
	}
	if h.adapter.Health(t.Context()).Status != runtimechannelhost.ChannelDown {
		t.Error("health is not down after a failed intake")
	}
	findings := h.adapter.Check(t.Context())
	if len(findings) != 1 || findings[0].Code != "email_intake_failed" {
		t.Fatalf("findings = %+v, want the intake finding", findings)
	}

	h.sink.mu.Lock()
	h.sink.err = nil
	h.sink.mu.Unlock()
	h.awaitWaiting()
	h.wantTurns(1)
	if got := h.cursors.get("email:work"); got.LastUID != 1 {
		t.Errorf("cursor = %+v, want last uid 1 after the retry", got)
	}
	if h.adapter.Health(t.Context()).Status != runtimechannelhost.ChannelHealthy {
		t.Error("health did not recover")
	}
}

func TestCursor_RuntimeRejectionDropsAndMovesOn(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.start()
	h.sink.err = &turn.Error{Code: turn.ErrorCodeInvalidArgument}
	h.deliver(mailSpec{messageID: "refused@example.com"}.raw())
	h.wantDrops("intake_rejected")
	if got := h.cursors.get("email:work"); got.LastUID != 1 {
		t.Errorf("cursor = %+v, want last uid 1", got)
	}
}

func TestCursor_WindowsLargeBacklogs(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.adapter.window = 2
	h.start()
	var raws [][]byte
	for _, id := range []string{"w1", "w2", "w3", "w4", "w5"} {
		raws = append(raws, mailSpec{messageID: id + "@example.com"}.raw())
	}
	h.deliver(raws...)
	envelopes := h.sink.all()
	if len(envelopes) != 5 {
		t.Fatalf("envelopes = %d, want 5", len(envelopes))
	}
	for i, env := range envelopes {
		if want := "email:work:w" + string(rune('1'+i)) + "@example.com"; env.ExternalID != want {
			t.Errorf("envelope %d = %q, want %q", i, env.ExternalID, want)
		}
	}
}

func TestCursor_GapsInTheUIDSpaceAreSkipped(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.adapter.window = 2
	h.start()
	h.mailbox.add(mailSpec{messageID: "g1@example.com"}.raw())
	removed := h.mailbox.add(mailSpec{messageID: "g2@example.com"}.raw())
	h.mailbox.add(mailSpec{messageID: "g3@example.com"}.raw())
	h.mailbox.remove(removed)
	h.wakeCycle()
	h.wantTurns(2)
	if got := h.cursors.get("email:work"); got.LastUID != 3 {
		t.Errorf("cursor = %+v, want last uid 3", got)
	}
}

func TestHealth_ReportsMailboxFaultsAndRecovers(t *testing.T) {
	cases := map[string]struct {
		err  error
		code string
	}{
		"password unavailable": {err: Errorf(ErrorCodeCredentialsUnavailable, "x"), code: "email_password_unavailable"},
		"login rejected":       {err: Errorf(ErrorCodeLoginRejected, "x"), code: "email_login_rejected"},
		"unreachable":          {err: Errorf(ErrorCodeConnectionFailed, "x"), code: "email_unreachable"},
		"protocol":             {err: Errorf(ErrorCodeProtocolInvalid, "x"), code: "email_protocol_invalid"},
		"untyped":              {err: errBoom, code: "email_unreachable"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			mailbox := newFakeMailbox()
			mailbox.connect = []error{tc.err}
			h := newHarness(t, harnessOptions{mailbox: mailbox})
			h.start()
			if got := h.adapter.Health(t.Context()); got.Status != runtimechannelhost.ChannelHealthy {
				t.Fatalf("health after recovery = %+v", got)
			}
			if mailbox.connects != 2 {
				t.Fatalf("connects = %d, want a retry after one failure", mailbox.connects)
			}
		})
	}
}

func TestHealth_FindingsNameTheFaultWithoutDetails(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.adapter.markFault(faultLoginRejected)
	findings := h.adapter.Check(t.Context())
	if len(findings) != 1 || findings[0].Code != "email_login_rejected" || findings[0].Remediation != RemediationReviewMailbox {
		t.Fatalf("findings = %+v", findings)
	}
	health := h.adapter.Health(t.Context())
	if health.Status != runtimechannelhost.ChannelDown || strings.Contains(health.Detail, testAddress) {
		t.Errorf("health = %+v", health)
	}
	h.adapter.markLive()
	if got := h.adapter.Health(t.Context()); got.Status != runtimechannelhost.ChannelHealthy {
		t.Errorf("health after markLive = %+v", got)
	}
	if findings := h.adapter.Check(t.Context()); len(findings) != 0 {
		t.Errorf("findings after recovery = %+v", findings)
	}
}

func TestRun_BacksOffExponentiallyAndWaitsThePollInterval(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		mailbox := newFakeMailbox()
		mailbox.connect = []error{errBoom, errBoom, errBoom, errBoom}
		h := newHarness(t, harnessOptions{mailbox: mailbox})
		h.adapter.retryBase = time.Second
		h.adapter.retryCap = 5 * time.Second
		var stamps []time.Time
		connector := &stampedConnector{inner: mailbox, stamps: &stamps}
		h.adapter.connector = connector
		start := time.Now()
		h.start()
		want := []time.Duration{0, time.Second, 3 * time.Second, 7 * time.Second, 12 * time.Second}
		if len(stamps) != len(want) {
			t.Fatalf("connect attempts = %d, want %d", len(stamps), len(want))
		}
		for i, stamp := range stamps {
			if got := stamp.Sub(start); got != want[i] {
				t.Errorf("attempt %d at %v, want %v", i, got, want[i])
			}
		}
		if len(mailbox.waits) != 1 || mailbox.waits[0] != time.Minute {
			t.Errorf("waits = %v, want the account poll interval", mailbox.waits)
		}
		h.stop()
	})
}

type stampedConnector struct {
	inner  Connector
	stamps *[]time.Time
}

func (c *stampedConnector) Connect(ctx context.Context) (Session, error) {
	*c.stamps = append(*c.stamps, time.Now())
	return c.inner.Connect(ctx)
}

func TestChannelName_IsTheSource(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	if h.adapter.ChannelName() != "email:work" || h.adapter.Account() != "work" {
		t.Errorf("identity = %q %q", h.adapter.ChannelName(), h.adapter.Account())
	}
	var none *Adapter
	if none.Account() != "" {
		t.Error("nil adapter has an account")
	}
}

func TestLogs_NeverCarryAddressesSubjectsBodiesOrIDs(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.start()
	const subject, body, id = "TOPSECRETSUBJECT", "TOPSECRETBODY", "secretid"
	h.deliver(
		mailSpec{subject: subject, body: body, messageID: id + "1@example.com"}.raw(),
		mailSpec{from: friendAddress, subject: subject, body: body, messageID: id + "2@example.com", noAuth: true}.raw(),
		mailSpec{from: strangerAddr, subject: subject, body: body, messageID: id + "3@example.com", auth: testAuthserv + "; dmarc=pass header.from=example.net"}.raw(),
		mailSpec{subject: subject, body: body, messageID: id + "4@example.com", extra: []string{"List-Id: <x.example.com>"}}.raw(),
		mailSpec{subject: subject, body: body, messageID: id + "5@example.com", rawFrom: []string{"From: " + ownerAddress + ", " + friendAddress}}.raw(),
	)
	logs := h.logs.String()
	for _, secret := range []string{ownerAddress, friendAddress, strangerAddr, testAddress, subject, body, id, "example.com"} {
		if strings.Contains(logs, secret) {
			t.Errorf("logs contain %q", secret)
		}
	}
	for _, reason := range []string{"sender_unauthenticated", "author_not_allowed", "automated_mail", "sender_invalid"} {
		if !strings.Contains(logs, "reason="+reason) {
			t.Errorf("logs do not record the %s reason", reason)
		}
	}
}
