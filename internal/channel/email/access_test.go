package email

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	netmail "net/mail"
	"strings"
	"sync"
	"testing"

	auraagent "github.com/anggasct/aura/internal/agent"
	"github.com/anggasct/aura/internal/channel/access"
	"github.com/anggasct/aura/internal/channel/approval"
	"github.com/anggasct/aura/internal/channel/media"
	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/principal"
)

type gateLog struct {
	mu    sync.Mutex
	calls []string
}

func (g *gateLog) note(name string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, name)
}

func (g *gateLog) all() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.calls...)
}

type loggedBudgets struct {
	log     *gateLog
	refusal string
	err     error
}

func (b loggedBudgets) Admit(context.Context, string, principal.Class) (string, error) {
	b.log.note("budget")
	return b.refusal, b.err
}

type loggedEvals struct {
	log       *gateLog
	evaluated bool
	err       error
}

func (e loggedEvals) Evaluated(context.Context, string) (bool, error) {
	e.log.note("eval")
	return e.evaluated, e.err
}

type loggedHandoffs struct {
	log   *gateLog
	state string
	err   error
}

func (h loggedHandoffs) Check(context.Context, string) (string, error) {
	h.log.note("handoff")
	return h.state, h.err
}

type recordingEraser struct {
	mu   sync.Mutex
	ids  []string
	fail error
}

func (e *recordingEraser) Forget(_ context.Context, principalID string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.ids = append(e.ids, principalID)
	return e.fail
}

func (e *recordingEraser) forgotten() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.ids...)
}

type storedMedia struct {
	meta    media.Meta
	content []byte
}

type recordingMedia struct {
	mu    sync.Mutex
	items []storedMedia
	fail  error
}

func (m *recordingMedia) Put(_ context.Context, content io.Reader, meta *media.Meta) (media.Receipt, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return media.Receipt{}, m.fail
	}
	data, err := io.ReadAll(content)
	if err != nil {
		return media.Receipt{}, err
	}
	m.items = append(m.items, storedMedia{meta: *meta, content: data})
	return media.Receipt{RefID: fmt.Sprintf("art-%d", len(m.items)), Digest: "sha256:test", SizeBytes: int64(len(data))}, nil
}

func (m *recordingMedia) stored() []storedMedia {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]storedMedia(nil), m.items...)
}

type recordingNotifier struct {
	mu    sync.Mutex
	asked []approval.Requester
	ids   []string
	why   []string
	fail  error
}

func (n *recordingNotifier) NotifyOwner(_ context.Context, requester *approval.Requester, sessionID, reason string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.asked = append(n.asked, *requester)
	n.ids = append(n.ids, sessionID)
	n.why = append(n.why, reason)
	return n.fail
}

func (h *harness) submissions() []Submission {
	h.t.Helper()
	mailer, ok := h.mailer.(*fakeMailer)
	if !ok {
		h.t.Fatalf("harness mailer is %T, want the recording mailer", h.mailer)
	}
	return mailer.sent()
}

func (h *harness) effectRows() int {
	h.t.Helper()
	var rows int
	if err := h.db.QueryRowContext(h.t.Context(), `SELECT COUNT(*) FROM effect_intent`).Scan(&rows); err != nil {
		h.t.Fatalf("count effects: %v", err)
	}
	return rows
}

func parseSubmission(t *testing.T, submission *Submission) *netmail.Message {
	t.Helper()
	parsed, err := netmail.ReadMessage(bytes.NewReader(submission.Message))
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	return parsed
}

func TestAccess_PublicSendersPassTheSharedGates(t *testing.T) {
	cases := []struct {
		name      string
		audience  string
		from      string
		budgets   func(*gateLog) access.BudgetGate
		evals     func(*gateLog) access.EvalGate
		handoffs  func(*gateLog) access.HandoffGate
		wantDrop  string
		wantClass principal.Class
	}{
		{name: "public persona admits a stranger", audience: config.AudiencePublic, from: strangerAddr, wantClass: principal.ClassPublic},
		{name: "trusted persona refuses a stranger", audience: config.AudienceTrusted, from: strangerAddr, wantDrop: "author_not_allowed"},
		{name: "owner persona refuses a stranger", audience: config.AudienceOwner, from: strangerAddr, wantDrop: "author_not_allowed"},
		{name: "public persona still admits the trusted friend", audience: config.AudiencePublic, from: friendAddress, wantClass: principal.ClassTrusted},
		{name: "public persona still admits the owner", audience: config.AudiencePublic, from: ownerAddress, wantClass: principal.ClassOwner},
		{
			name: "missing eval refuses a stranger", audience: config.AudiencePublic, from: strangerAddr, wantDrop: "persona_eval_missing",
			evals: func(g *gateLog) access.EvalGate { return loggedEvals{log: g} },
		},
		{
			name: "eval lookup failure refuses a stranger", audience: config.AudiencePublic, from: strangerAddr, wantDrop: "persona_eval_missing",
			evals: func(g *gateLog) access.EvalGate { return loggedEvals{log: g, evaluated: true, err: errBoom} },
		},
		{
			name: "missing eval never refuses the owner", audience: config.AudiencePublic, from: ownerAddress, wantClass: principal.ClassOwner,
			evals: func(g *gateLog) access.EvalGate { return loggedEvals{log: g} },
		},
		{
			name: "rate limit refuses a stranger", audience: config.AudiencePublic, from: strangerAddr, wantDrop: "rate_limited",
			budgets: func(g *gateLog) access.BudgetGate {
				return loggedBudgets{log: g, refusal: access.BudgetRefusalRateLimited}
			},
		},
		{
			name: "exhausted budget refuses a stranger", audience: config.AudiencePublic, from: strangerAddr, wantDrop: "budget_exhausted",
			budgets: func(g *gateLog) access.BudgetGate {
				return loggedBudgets{log: g, refusal: access.BudgetRefusalExhausted}
			},
		},
		{
			name: "budget lookup failure refuses a stranger", audience: config.AudiencePublic, from: strangerAddr, wantDrop: "budget_check_failed",
			budgets: func(g *gateLog) access.BudgetGate { return loggedBudgets{log: g, err: errBoom} },
		},
		{
			name: "exhausted budget never refuses the owner", audience: config.AudiencePublic, from: ownerAddress, wantClass: principal.ClassOwner,
			budgets: func(g *gateLog) access.BudgetGate {
				return loggedBudgets{log: g, refusal: access.BudgetRefusalExhausted}
			},
		},
		{
			name: "open handoff holds a stranger", audience: config.AudiencePublic, from: strangerAddr, wantDrop: "handoff_held",
			handoffs: func(g *gateLog) access.HandoffGate { return loggedHandoffs{log: g, state: access.HandoffStateOpen} },
		},
		{
			name: "open handoff holds a trusted sender", audience: config.AudienceTrusted, from: friendAddress, wantDrop: "handoff_held",
			handoffs: func(g *gateLog) access.HandoffGate { return loggedHandoffs{log: g, state: access.HandoffStateOpen} },
		},
		{
			name: "handoff lookup failure holds a stranger", audience: config.AudiencePublic, from: strangerAddr, wantDrop: "handoff_check_failed",
			handoffs: func(g *gateLog) access.HandoffGate { return loggedHandoffs{log: g, err: errBoom} },
		},
		{
			name: "expired handoff admits a stranger", audience: config.AudiencePublic, from: strangerAddr, wantClass: principal.ClassPublic,
			handoffs: func(g *gateLog) access.HandoffGate { return loggedHandoffs{log: g, state: access.HandoffStateExpired} },
		},
		{
			name: "open handoff never holds the owner", audience: config.AudiencePublic, from: ownerAddress, wantClass: principal.ClassOwner,
			handoffs: func(g *gateLog) access.HandoffGate { return loggedHandoffs{log: g, state: access.HandoffStateOpen} },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			log := &gateLog{}
			opts := harnessOptions{audience: tc.audience, evals: loggedEvals{log: log, evaluated: true}}
			if tc.evals != nil {
				opts.evals = tc.evals(log)
			}
			if tc.budgets != nil {
				opts.budgets = tc.budgets(log)
			}
			if tc.handoffs != nil {
				opts.handoffs = tc.handoffs(log)
			}
			h := newHarness(t, opts)
			h.start()
			h.deliver(mailSpec{from: tc.from, subject: "Question", messageID: "g1@example.com"}.raw())
			if tc.wantDrop != "" {
				h.wantTurns(0)
				h.wantDrops(tc.wantDrop)
			} else {
				h.wantTurns(1)
				h.wantDrops()
				if got := h.sink.all()[0].PrincipalClass; got != string(tc.wantClass) {
					t.Errorf("class = %q, want %q", got, tc.wantClass)
				}
			}
			if sent := h.submissions(); len(sent) != 0 {
				t.Errorf("mail sent = %d, want none: gates and replies are the runtime's business, a refusal never mails back", len(sent))
			}
			if rows := h.effectRows(); rows != 0 {
				t.Errorf("effect rows = %d, want none", rows)
			}
		})
	}
}

func TestAccess_PublicPrincipalIsTheSenderAddress(t *testing.T) {
	h := newHarness(t, harnessOptions{audience: config.AudiencePublic})
	h.start()
	h.deliver(mailSpec{from: strangerAddr, subject: "Hello", messageID: "p1@example.com"}.raw())
	h.wantTurns(1)
	env := h.sink.all()[0]
	if env.PrincipalID != "email:"+strangerAddr || env.PrincipalClass != string(principal.ClassPublic) {
		t.Fatalf("principal = %q class %q, want the public sender", env.PrincipalID, env.PrincipalClass)
	}
	if owner, _, _ := h.sessions.SessionOwner(t.Context(), env.ConversationID); owner != "email:"+strangerAddr {
		t.Errorf("session owner = %q, want the sender principal", owner)
	}
	if got := h.admittedClasses(); len(got) != 1 || got[0] != principal.ClassPublic {
		t.Errorf("admitted = %v, want one public turn", got)
	}
}

func TestAccess_GatesRunInDiscordDMOrder(t *testing.T) {
	cases := []struct {
		name     string
		audience string
		from     string
		want     []string
	}{
		{name: "public stranger runs eval, budget and handoff in order", audience: config.AudiencePublic, from: strangerAddr, want: []string{"eval", "budget", "handoff"}},
		{name: "trusted sender runs the same order", audience: config.AudienceTrusted, from: friendAddress, want: []string{"eval", "budget", "handoff"}},
		{name: "owner skips every gate", audience: config.AudienceTrusted, from: ownerAddress, want: nil},
		{name: "audience refusal runs no later gate", audience: config.AudienceOwner, from: strangerAddr, want: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			log := &gateLog{}
			h := newHarness(t, harnessOptions{
				audience: tc.audience,
				evals:    loggedEvals{log: log, evaluated: true},
				budgets:  loggedBudgets{log: log},
				handoffs: loggedHandoffs{log: log, state: access.HandoffStateNone},
			})
			h.start()
			h.deliver(mailSpec{from: tc.from, messageID: "o1@example.com"}.raw())
			if got := log.all(); strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("gate order = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAccess_UnauthenticatedMailNeverReachesGatesCommandsOrAttachments(t *testing.T) {
	bodies := map[string]string{
		"plain text":           "hello",
		"forgetme":             "/forgetme",
		"forgetme confirm":     "/forgetme confirm",
		"approval look-alikes": "approve yes accept",
	}
	for _, from := range []string{ownerAddress, friendAddress, strangerAddr} {
		for name, body := range bodies {
			t.Run(from+"/"+name, func(t *testing.T) {
				log := &gateLog{}
				eraser := &recordingEraser{}
				store := &recordingMedia{}
				route, err := approval.NewOwnerRoute(&ownerStub{})
				if err != nil {
					t.Fatalf("NewOwnerRoute: %v", err)
				}
				h := newHarness(t, harnessOptions{
					audience: config.AudiencePublic, route: route, erasure: eraser, media: store,
					evals: loggedEvals{log: log, evaluated: true}, budgets: loggedBudgets{log: log}, handoffs: loggedHandoffs{log: log},
				})
				h.start()
				spec := mailSpec{from: from, messageID: "u1@example.com", body: body, auth: testAuthserv + "; dmarc=fail header.from=" + domainOf(from)}
				h.deliver(mailWithAttachments(spec, body, testAttachment{filename: "a.png", mediaType: "image/png", content: []byte("png")}))
				h.wantTurns(0)
				h.wantDrops("sender_unauthenticated")
				if calls := log.all(); len(calls) != 0 {
					t.Errorf("gates consulted for unauthenticated mail: %v", calls)
				}
				if ids := eraser.forgotten(); len(ids) != 0 {
					t.Errorf("erasure ran for unauthenticated mail: %v", ids)
				}
				if items := store.stored(); len(items) != 0 {
					t.Errorf("attachments stored for unauthenticated mail: %d", len(items))
				}
				if sent := h.submissions(); len(sent) != 0 {
					t.Errorf("mail sent to an unauthenticated sender: %d", len(sent))
				}
				if route.HoldsApprovalTarget(wantSession("u1@example.com")) {
					t.Error("an approval target was registered for unauthenticated mail")
				}
			})
		}
	}
}

func wantSession(messageID string) string {
	return testPersona + "/email:" + testAccount + "/thread:" + wantDigest(messageID)
}

type ownerStub struct {
	mu    sync.Mutex
	calls int
}

func (o *ownerStub) DecideForOwner(context.Context, *approval.Prompt, *approval.Requester) (bool, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.calls++
	return true, nil
}

func (o *ownerStub) asked() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.calls
}

func TestAccess_AdmittedSessionsRegisterTheirRequesterForOwnerApproval(t *testing.T) {
	cases := []struct {
		name string
		from string
		want approval.Requester
	}{
		{name: "owner", from: ownerAddress, want: approval.Requester{Principal: principal.OwnerID, Class: principal.ClassOwner, Persona: testPersona}},
		{name: "trusted", from: friendAddress, want: approval.Requester{Principal: "email:" + friendAddress, Class: principal.ClassTrusted, Persona: testPersona}},
		{name: "public", from: strangerAddr, want: approval.Requester{Principal: "email:" + strangerAddr, Class: principal.ClassPublic, Persona: testPersona}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			owner := &ownerStub{}
			route, err := approval.NewOwnerRoute(owner)
			if err != nil {
				t.Fatalf("NewOwnerRoute: %v", err)
			}
			h := newHarness(t, harnessOptions{audience: config.AudiencePublic, route: route})
			h.start()
			h.deliver(mailSpec{from: tc.from, messageID: "r1@example.com"}.raw())
			h.wantTurns(1)
			session := h.sink.all()[0].ConversationID
			got, held := route.RequesterOf(session)
			if !held || got != tc.want {
				t.Fatalf("RequesterOf = %+v %v, want %+v", got, held, tc.want)
			}
			accepted, err := route.Decide(t.Context(), &approval.Prompt{ToolName: "write_file", SessionID: session, PrincipalID: tc.want.Principal})
			if err != nil || !accepted || owner.asked() != 1 {
				t.Errorf("Decide = %v, %v with %d owner calls, want the owner route to decide once", accepted, err, owner.asked())
			}
			accepted, _ = route.Decide(t.Context(), &approval.Prompt{ToolName: "write_file", SessionID: session, PrincipalID: "email:mallory@example.net"})
			if accepted || owner.asked() != 1 {
				t.Errorf("a prompt for another principal reached the owner: accepted=%v calls=%d", accepted, owner.asked())
			}
		})
	}
}

func TestAccess_RefusedMailRegistersNoApprovalTarget(t *testing.T) {
	route, err := approval.NewOwnerRoute(&ownerStub{})
	if err != nil {
		t.Fatalf("NewOwnerRoute: %v", err)
	}
	h := newHarness(t, harnessOptions{audience: config.AudienceTrusted, route: route, evals: loggedEvals{evaluated: true, log: &gateLog{}}})
	h.start()
	h.deliver(mailSpec{from: strangerAddr, messageID: "x1@example.com"}.raw())
	h.wantTurns(0)
	if route.HoldsApprovalTarget(wantSession("x1@example.com")) {
		t.Error("a refused sender holds an approval target")
	}
}

func TestAccess_MailBodiesNeverAnswerAnApproval(t *testing.T) {
	owner := &ownerStub{}
	route, err := approval.NewOwnerRoute(owner)
	if err != nil {
		t.Fatalf("NewOwnerRoute: %v", err)
	}
	h := newHarness(t, harnessOptions{audience: config.AudiencePublic, route: route})
	h.start()
	for i, body := range []string{"approve", "yes, go ahead", "aura:0123456789abcdef:a:0123456789abcdef", "/approve all"} {
		h.deliver(mailSpec{from: ownerAddress, messageID: fmt.Sprintf("a%d@example.com", i), body: body}.raw())
	}
	h.wantTurns(4)
	if owner.asked() != 0 {
		t.Errorf("owner approver asked %d times by mail text, want never", owner.asked())
	}
}

func TestForgetMe(t *testing.T) {
	definition := auraagent.Definition{}
	cases := []struct {
		name       string
		from       string
		body       string
		auth       bool
		eraser     *recordingEraser
		noEraser   bool
		wantNotice string
		wantErased []string
		wantTurn   bool
	}{
		{name: "trusted asks first", from: friendAddress, body: "/forgetme", wantNotice: definition.Message(config.MessageForgetmeConfirm)},
		{name: "trusted confirms", from: friendAddress, body: "/forgetme confirm", wantNotice: definition.Message(config.MessageForgetmeDone), wantErased: []string{"email:" + friendAddress}},
		{name: "confirm is case insensitive", from: friendAddress, body: "  /FORGETME Confirm \r\n", wantNotice: definition.Message(config.MessageForgetmeDone), wantErased: []string{"email:" + friendAddress}},
		{name: "public confirms", from: strangerAddr, body: "/forgetme confirm", wantNotice: definition.Message(config.MessageForgetmeDone), wantErased: []string{"email:" + strangerAddr}},
		{name: "owner is not permitted", from: ownerAddress, body: "/forgetme confirm", wantNotice: definition.Message(config.MessageNotPermitted)},
		{name: "owner asking first is not permitted either", from: ownerAddress, body: "/forgetme", wantNotice: definition.Message(config.MessageNotPermitted)},
		{name: "erasure failure sends no confirmation", from: friendAddress, body: "/forgetme confirm", eraser: &recordingEraser{fail: errBoom}, wantErased: []string{"email:" + friendAddress}},
		{name: "extra word is an ordinary message", from: friendAddress, body: "/forgetme now", wantTurn: true},
		{name: "second word after confirm is an ordinary message", from: friendAddress, body: "/forgetme confirm please", wantTurn: true},
		{name: "command inside a sentence is an ordinary message", from: friendAddress, body: "please /forgetme confirm", wantTurn: true},
		{name: "command followed by more lines is an ordinary message", from: friendAddress, body: "/forgetme confirm\n\nthanks", wantTurn: true},
		{name: "another command is an ordinary message", from: friendAddress, body: "/status", wantTurn: true},
		{name: "without an eraser the command is ordinary text", from: friendAddress, body: "/forgetme confirm", noEraser: true, wantTurn: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eraser := tc.eraser
			if eraser == nil {
				eraser = &recordingEraser{}
			}
			opts := harnessOptions{audience: config.AudiencePublic}
			if !tc.noEraser {
				opts.erasure = eraser
			}
			h := newHarness(t, opts)
			h.start()
			h.deliver(mailSpec{from: tc.from, subject: "Cleanup", messageID: "f1@example.com", body: tc.body}.raw())
			if got := eraser.forgotten(); strings.Join(got, ",") != strings.Join(tc.wantErased, ",") {
				t.Errorf("erased = %v, want %v", got, tc.wantErased)
			}
			if tc.wantTurn {
				h.wantTurns(1)
			} else {
				h.wantTurns(0)
				h.wantDrops()
			}
			sent := h.submissions()
			if tc.wantNotice == "" {
				if len(sent) != 0 {
					t.Fatalf("mail sent = %d, want none", len(sent))
				}
				return
			}
			if len(sent) != 1 {
				t.Fatalf("mail sent = %d, want one notice", len(sent))
			}
			parsed := parseSubmission(t, &sent[0])
			if sent[0].To != tc.from || parsed.Header.Get("To") != tc.from || parsed.Header.Get("Subject") != "Re: Cleanup" {
				t.Errorf("notice envelope To=%q header To=%q Subject=%q, want a reply to the sender", sent[0].To, parsed.Header.Get("To"), parsed.Header.Get("Subject"))
			}
			if got := strings.TrimSpace(decodedBody(t, parsed)); got != tc.wantNotice {
				t.Errorf("notice = %q, want %q", got, tc.wantNotice)
			}
			if rows := h.effectRows(); rows != 0 {
				t.Errorf("effect rows = %d, want none: a notice must leave no journal row holding the address", rows)
			}
		})
	}
}

func TestForgetMe_OnlyTheAuthenticatedSenderIsErased(t *testing.T) {
	eraser := &recordingEraser{}
	h := newHarness(t, harnessOptions{audience: config.AudiencePublic, erasure: eraser})
	h.start()
	h.deliver(mailSpec{from: friendAddress, messageID: "f2@example.com", body: "/forgetme confirm"}.raw())
	h.deliver(mailSpec{from: strangerAddr, messageID: "f3@example.com", body: "/forgetme confirm", extra: []string{"X-Forwarded-For: " + friendAddress}}.raw())
	got := eraser.forgotten()
	want := []string{"email:" + friendAddress, "email:" + strangerAddr}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("erased = %v, want each sender erased once and only as themselves: %v", got, want)
	}
}

func TestForgetMe_HoldsNoApprovalTargetAndLeavesNoSession(t *testing.T) {
	route, err := approval.NewOwnerRoute(&ownerStub{})
	if err != nil {
		t.Fatalf("NewOwnerRoute: %v", err)
	}
	h := newHarness(t, harnessOptions{audience: config.AudiencePublic, route: route, erasure: &recordingEraser{}})
	h.start()
	h.deliver(mailSpec{from: friendAddress, messageID: "f4@example.com", body: "/forgetme confirm"}.raw())
	if route.HoldsApprovalTarget(wantSession("f4@example.com")) || h.sessions.count() != 0 {
		t.Error("an erased request left a session or an approval target behind")
	}
}

type testAttachment struct {
	filename  string
	mediaType string
	content   []byte
	plain     bool
}

func mailWithAttachments(spec mailSpec, text string, attachments ...testAttachment) []byte {
	const boundary = "AURA-TEST-BOUNDARY"
	head, _, _ := strings.Cut(string(spec.raw()), "MIME-Version: 1.0\r\n")
	var out strings.Builder
	out.WriteString(head)
	out.WriteString("MIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=" + boundary + "\r\n\r\n")
	out.WriteString("--" + boundary + "\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n" + text + "\r\n")
	for _, item := range attachments {
		out.WriteString("--" + boundary + "\r\n")
		out.WriteString("Content-Type: " + item.mediaType + "\r\n")
		out.WriteString("Content-Disposition: attachment; filename=\"" + item.filename + "\"\r\n")
		if item.plain {
			out.WriteString("Content-Transfer-Encoding: 8bit\r\n\r\n")
			out.Write(item.content)
			out.WriteString("\r\n")
			continue
		}
		out.WriteString("Content-Transfer-Encoding: base64\r\n\r\n")
		encoded := base64.StdEncoding.EncodeToString(item.content)
		for len(encoded) > 76 {
			out.WriteString(encoded[:76] + "\r\n")
			encoded = encoded[76:]
		}
		out.WriteString(encoded + "\r\n")
	}
	out.WriteString("--" + boundary + "--\r\n")
	return []byte(out.String())
}

func TestAttachments_AreBoundedAndLandAsArtifacts(t *testing.T) {
	const limit = 1024
	png := func(n int) []byte { return bytes.Repeat([]byte{0x89}, n) }
	cases := []struct {
		name        string
		attachments []testAttachment
		text        string
		store       *recordingMedia
		wantDrop    string
		wantStored  []string
		wantInput   string
	}{
		{name: "one image under the limit", attachments: []testAttachment{{filename: "photo.png", mediaType: "image/png", content: png(300)}}, text: "see attached", wantStored: []string{"photo.png"}, wantInput: "Subject: Docs\n\nsee attached"},
		{name: "an image exactly at the limit", attachments: []testAttachment{{filename: "edge.png", mediaType: "image/png", content: png(limit)}}, text: "see attached", wantStored: []string{"edge.png"}, wantInput: "Subject: Docs\n\nsee attached"},
		{name: "an image one byte over the limit", attachments: []testAttachment{{filename: "big.png", mediaType: "image/png", content: png(limit + 1)}}, text: "see attached", wantDrop: "attachment_failed"},
		{name: "a second attachment over the limit drops the whole message", attachments: []testAttachment{{filename: "ok.png", mediaType: "image/png", content: png(10)}, {filename: "big.pdf", mediaType: "application/pdf", content: png(limit * 2)}}, text: "see attached", wantDrop: "attachment_failed"},
		{name: "raw 8bit content over the limit", attachments: []testAttachment{{filename: "raw.png", mediaType: "image/png", content: png(limit * 3), plain: true}}, text: "see attached", wantDrop: "attachment_failed"},
		{name: "a pdf and an image both land", attachments: []testAttachment{{filename: "a.pdf", mediaType: "application/pdf", content: []byte("%PDF-1.4")}, {filename: "b.jpg", mediaType: "image/jpeg", content: png(20)}}, text: "two files", wantStored: []string{"a.pdf", "b.jpg"}, wantInput: "Subject: Docs\n\ntwo files"},
		{name: "an ineligible type is ignored", attachments: []testAttachment{{filename: "run.exe", mediaType: "application/x-msdownload", content: png(30)}}, text: "see attached", wantInput: "Subject: Docs\n\nsee attached"},
		{name: "an attachment alone still makes a turn", attachments: []testAttachment{{filename: "photo.png", mediaType: "image/png", content: png(30)}}, text: " ", wantStored: []string{"photo.png"}, wantInput: "Subject: Docs"},
		{name: "path segments never survive in the filename", attachments: []testAttachment{{filename: "../../etc/passwd.png", mediaType: "image/png", content: png(30)}}, text: "x", wantStored: []string{"passwd.png"}, wantInput: "Subject: Docs\n\nx"},
		{name: "nested directories are stripped", attachments: []testAttachment{{filename: "a/b/c.png", mediaType: "image/png", content: png(30)}}, text: "x", wantStored: []string{"c.png"}, wantInput: "Subject: Docs\n\nx"},
		{name: "storage failure drops the message", attachments: []testAttachment{{filename: "photo.png", mediaType: "image/png", content: png(30)}}, text: "x", store: &recordingMedia{fail: errBoom}, wantDrop: "attachment_failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := tc.store
			if store == nil {
				store = &recordingMedia{}
			}
			h := newHarness(t, harnessOptions{audience: config.AudiencePublic, media: store, email: func(cfg *config.Email) { cfg.MaxAttachmentBytes = limit }})
			h.start()
			h.deliver(mailWithAttachments(mailSpec{from: friendAddress, subject: "Docs", messageID: "att@example.com"}, tc.text, tc.attachments...))
			if tc.wantDrop != "" {
				h.wantTurns(0)
				h.wantDrops(tc.wantDrop)
				if tc.store == nil && len(store.stored()) != 0 {
					t.Errorf("stored %d artifacts for a message that was dropped", len(store.stored()))
				}
				return
			}
			h.wantTurns(1)
			env := h.sink.all()[0]
			var names []string
			for _, item := range store.stored() {
				names = append(names, item.meta.Filename)
				if item.meta.SessionID != env.ConversationID {
					t.Errorf("artifact session = %q, want the turn session %q", item.meta.SessionID, env.ConversationID)
				}
				if strings.ContainsAny(item.meta.Filename, "/\\") {
					t.Errorf("artifact filename %q carries a path separator", item.meta.Filename)
				}
			}
			if strings.Join(names, ",") != strings.Join(tc.wantStored, ",") {
				t.Errorf("stored = %v, want %v", names, tc.wantStored)
			}
			wantInput := tc.wantInput
			if len(env.Parts) != 1 || env.Parts[0].Text != wantInput {
				t.Errorf("turn input = %+v, want %q", env.Parts, wantInput)
			}
			var reply replyReference
			if err := json.Unmarshal(env.ReplyContext, &reply); err != nil || len(reply.Artifacts) != len(tc.wantStored) {
				t.Errorf("reply context artifacts = %d (%v), want %d", len(reply.Artifacts), err, len(tc.wantStored))
			}
		})
	}
}

func TestAttachments_AttachmentOnlyMailGetsAPlaceholderTurn(t *testing.T) {
	store := &recordingMedia{}
	h := newHarness(t, harnessOptions{media: store})
	h.start()
	spec := mailSpec{from: friendAddress, messageID: "att2@example.com"}
	spec.subject = ""
	h.deliver(mailWithAttachments(spec, " ", testAttachment{filename: "photo.png", mediaType: "image/png", content: []byte("png")}))
	h.wantTurns(1)
	if got := h.sink.all()[0].Parts[0].Text; got != attachmentOnlyText {
		t.Errorf("turn input = %q, want %q", got, attachmentOnlyText)
	}
}

func TestAttachments_OversizedMessagesAreRefusedBeforeAnyDownload(t *testing.T) {
	store := &recordingMedia{}
	h := newHarness(t, harnessOptions{media: store, email: func(cfg *config.Email) { cfg.MaxAttachmentBytes = 2048 }})
	h.start()
	uid := h.mailbox.add(mailWithAttachments(mailSpec{from: friendAddress, messageID: "huge@example.com"}, "x", testAttachment{filename: "a.png", mediaType: "image/png", content: []byte("png")}))
	h.mailbox.setSize(uid, 2048+messageOverhead+1)
	h.wakeCycle()
	h.wantTurns(0)
	h.wantDrops("message_too_large")
	if len(h.mailbox.bodies) != 0 || len(store.stored()) != 0 {
		t.Errorf("fetched %d bodies and stored %d artifacts for an oversized message, want none", len(h.mailbox.bodies), len(store.stored()))
	}
}

func TestAttachments_TooManyDropTheMessage(t *testing.T) {
	store := &recordingMedia{}
	h := newHarness(t, harnessOptions{media: store})
	h.start()
	items := make([]testAttachment, 0, maxAttachmentsPerMessage+1)
	for i := range maxAttachmentsPerMessage + 1 {
		items = append(items, testAttachment{filename: fmt.Sprintf("p%d.png", i), mediaType: "image/png", content: []byte("png")})
	}
	h.deliver(mailWithAttachments(mailSpec{from: friendAddress, messageID: "many@example.com"}, "x", items...))
	h.wantTurns(0)
	h.wantDrops("attachment_failed")
	if len(store.stored()) != 0 {
		t.Errorf("stored %d artifacts for a rejected message", len(store.stored()))
	}
}

func TestSessionRoute(t *testing.T) {
	h := newHarness(t, harnessOptions{audience: config.AudiencePublic})
	h.start()
	h.deliver(mailSpec{from: strangerAddr, messageID: "s1@example.com"}.raw())
	h.wantTurns(1)
	session := h.sink.all()[0].ConversationID
	cases := []struct {
		name    string
		session string
		want    Route
		found   bool
	}{
		{name: "an admitted session resolves its account and sender", session: session, want: Route{Account: testAccount, Sender: strangerAddr}, found: true},
		{name: "an unknown session resolves nothing", session: "main/discord:owner/dm:1"},
		{name: "an empty session resolves nothing"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, found := h.adapter.SessionRoute(tc.session)
			if found != tc.found || got != tc.want {
				t.Errorf("SessionRoute = %+v %v, want %+v %v", got, found, tc.want, tc.found)
			}
		})
	}
	var absent *Adapter
	if _, found := absent.SessionRoute(session); found {
		t.Error("a nil adapter resolved a route")
	}
}

func TestNotifyHandoff(t *testing.T) {
	cases := []struct {
		name     string
		from     string
		notifier func() *recordingNotifier
		session  string
		wantErr  func(error) bool
		wantTo   string
	}{
		{name: "public sender notifies the owner route", from: strangerAddr, notifier: func() *recordingNotifier { return &recordingNotifier{} }, wantTo: strangerAddr},
		{name: "trusted sender notifies the owner route", from: friendAddress, notifier: func() *recordingNotifier { return &recordingNotifier{} }, wantTo: friendAddress},
		{name: "unknown session has no target", from: strangerAddr, notifier: func() *recordingNotifier { return &recordingNotifier{} }, session: "main/email:work/thread:none", wantErr: func(err error) bool { return errors.Is(err, access.ErrNoHandoffTarget) }},
		{name: "unreachable owner fails the handoff", from: strangerAddr, notifier: func() *recordingNotifier { return &recordingNotifier{fail: errBoom} }, wantErr: func(err error) bool { return errors.Is(err, errBoom) }},
		{name: "owner sessions never hand off", from: ownerAddress, notifier: func() *recordingNotifier { return &recordingNotifier{} }, wantErr: func(err error) bool {
			code, ok := CodeOf(err)
			return ok && code == ErrorCodeHandoffUnavailable
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			notifier := tc.notifier()
			h := newHarness(t, harnessOptions{audience: config.AudiencePublic, notifier: notifier})
			h.start()
			h.deliver(mailSpec{from: tc.from, subject: "Refund please", messageID: "h1@example.com"}.raw())
			h.wantTurns(1)
			session := h.sink.all()[0].ConversationID
			if tc.session != "" {
				session = tc.session
			}
			target, err := h.adapter.NotifyHandoff(t.Context(), session, "wants a refund")
			if tc.wantErr != nil {
				if err == nil || !tc.wantErr(err) {
					t.Fatalf("NotifyHandoff err = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("NotifyHandoff: %v", err)
			}
			if target.Channel != principal.ChannelEmail || target.Account != testAccount || target.ChannelID != tc.wantTo {
				t.Errorf("target = %+v, want the email account and the authenticated sender", target)
			}
			thread := ParseThreadReference(target.ThreadID)
			if thread.MessageID != "h1@example.com" || thread.Subject != "Refund please" {
				t.Errorf("thread = %+v, want the last inbound message", thread)
			}
			if len(notifier.asked) != 1 || notifier.ids[0] != session || notifier.why[0] != "wants a refund" || notifier.asked[0].Principal != "email:"+tc.wantTo {
				t.Errorf("owner notice = %+v %v %v", notifier.asked, notifier.ids, notifier.why)
			}
		})
	}
}

func TestNotifyHandoff_WithoutAnOwnerRouteFailsClosed(t *testing.T) {
	h := newHarness(t, harnessOptions{audience: config.AudiencePublic})
	h.start()
	h.deliver(mailSpec{from: strangerAddr, messageID: "h2@example.com"}.raw())
	h.wantTurns(1)
	if _, err := h.adapter.NotifyHandoff(t.Context(), h.sink.all()[0].ConversationID, "reason"); err == nil {
		t.Fatal("NotifyHandoff succeeded without an owner route")
	}
}

func TestParseThreadReference(t *testing.T) {
	cases := map[string]ThreadReference{
		`{"message_id":"a@b","subject":"Hi"}`: {MessageID: "a@b", Subject: "Hi"},
		``:                                    {},
		`not json`:                            {},
		`[]`:                                  {},
	}
	for raw, want := range cases {
		if got := ParseThreadReference(raw); got != want {
			t.Errorf("ParseThreadReference(%q) = %+v, want %+v", raw, got, want)
		}
	}
}
