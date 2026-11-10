package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"log/slog"
	netmail "net/mail"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/anggasct/aura/internal/channels/email"
	"github.com/anggasct/aura/internal/config"
	toolsbuiltin "github.com/anggasct/aura/internal/modules/builtin"
	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/capability/capabilitytest"
	"github.com/anggasct/aura/internal/platform/principal"
	runtimeadk "github.com/anggasct/aura/internal/platform/turn/adk"
	runtimechannelhost "github.com/anggasct/aura/internal/platform/turn/channelhost"
	runtimeengine "github.com/anggasct/aura/internal/platform/turn/engine"
	"github.com/anggasct/aura/internal/store"
)

const (
	accessOwner    = "owner@example.com"
	accessFriend   = "friend@example.com"
	accessStranger = "stranger@example.com"
	accessThird    = "third@example.com"
	accessAccount  = "aura@example.com"
	callMarker     = "CALL "
)

func writeEmailAccessConfig(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, "config.yaml")
	content := `version: 1
storage:
  path: ` + root + `
tools:
  workspace: ` + root + `
owner:
  identities:
    - email:` + accessOwner + `
channels:
  email:
    enabled: true
    accounts:
      - name: work
        persona: desk
        address: ` + accessAccount + `
        authserv_id: mx.example.com
        allowed_senders:
          - ` + accessFriend + `
        imap:
          host: imap.example.com
          username: ` + accessAccount + `
          password_ref: env://AURA_EMAIL_TEST_PASSWORD
        smtp:
          host: smtp.example.com
          username: ` + accessAccount + `
          password_ref: env://AURA_EMAIL_TEST_SMTP_PASSWORD
access:
  budgets:
    trusted:
      max_tokens_per_day: 100000
      max_turns_per_minute: 50
    public:
      max_tokens_per_day: 100000
      max_turns_per_minute: 50
agents:
  definitions:
    - id: desk
      description: mail desk
      instructions: "You answer mail."
      audience: public
      tools: [read_file, web_search, email_send, handoff_to_owner]
      principal_tools: [read_file, email_send]
      public_tools: [web_search, handoff_to_owner]
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	seedConfiguredStorage(t, path)
	return path
}

type markerModel struct {
	mu        sync.Mutex
	requests  []*adkmodel.LLMRequest
	responses []string
	calls     int
}

func (m *markerModel) Name() string { return "email-access" }

func (m *markerModel) GenerateContent(_ context.Context, req *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	m.mu.Lock()
	m.requests = append(m.requests, req)
	m.mu.Unlock()
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		usage := &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: 4, CandidatesTokenCount: 1, TotalTokenCount: 5}
		reply := func(part *genai.Part) *adkmodel.LLMResponse {
			return &adkmodel.LLMResponse{Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{part}}, TurnComplete: true, FinishReason: genai.FinishReasonStop, UsageMetadata: usage}
		}
		if len(req.Contents) == 0 {
			yield(reply(&genai.Part{Text: "ok"}), nil)
			return
		}
		last := req.Contents[len(req.Contents)-1]
		var text strings.Builder
		for _, part := range last.Parts {
			if part.FunctionResponse != nil {
				encoded, _ := json.Marshal(part.FunctionResponse.Response)
				m.mu.Lock()
				m.responses = append(m.responses, string(encoded))
				m.mu.Unlock()
				yield(reply(&genai.Part{Text: "done"}), nil)
				return
			}
			text.WriteString(part.Text)
		}
		for line := range strings.SplitSeq(text.String(), "\n") {
			rest, found := strings.CutPrefix(strings.TrimSpace(line), callMarker)
			if !found {
				continue
			}
			name, raw, _ := strings.Cut(rest, " ")
			args := map[string]any{}
			if err := json.Unmarshal([]byte(raw), &args); err != nil {
				yield(nil, fmt.Errorf("bad scripted call: %w", err))
				return
			}
			m.mu.Lock()
			m.calls++
			id := fmt.Sprintf("call-%d", m.calls)
			m.mu.Unlock()
			yield(reply(&genai.Part{FunctionCall: &genai.FunctionCall{ID: id, Name: name, Args: args}}), nil)
			return
		}
		yield(reply(&genai.Part{Text: "ok"}), nil)
	}
}

func (m *markerModel) requestCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.requests)
}

func (m *markerModel) toolResponses() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.responses)
}

func (m *markerModel) firstRequest() *adkmodel.LLMRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.requests) == 0 {
		return nil
	}
	return m.requests[0]
}

type emailAccessOptions struct {
	approvers func(log *askLog) []approvalAdapter
}

type emailAccessHarness struct {
	t           *testing.T
	configPath  string
	cfg         *config.Config
	db          *sql.DB
	mail        *fakeMail
	submissions *fakeSubmissions
	model       *markerModel
	asked       *askLog
	routed      *routedResults
	adapter     *email.Adapter
	artifacts   string
	awaited     int
	counter     int
}

func newEmailAccessHarness(t *testing.T, options emailAccessOptions) *emailAccessHarness {
	t.Helper()
	path := writeEmailAccessConfig(t)
	cfg := mustLoadConfig(t, path)
	db := openAccessDB(t, path)
	registry, err := buildAgentRegistry(cfg)
	if err != nil {
		t.Fatalf("buildAgentRegistry: %v", err)
	}
	passEvals(t, db, registry, "desk")
	h := &emailAccessHarness{
		t: t, configPath: path, cfg: cfg, db: db, mail: newFakeMail(), submissions: newFakeSubmissions(),
		model: &markerModel{}, asked: &askLog{}, routed: &routedResults{},
	}
	_, h.artifacts, _, err = storagePaths(cfg)
	if err != nil {
		t.Fatalf("storagePaths: %v", err)
	}
	var approvers []approvalAdapter
	if options.approvers != nil {
		approvers = options.approvers(h.asked)
	}
	route, err := newOwnerRoute(approvers, h.routed.observe)
	if err != nil {
		t.Fatalf("newOwnerRoute: %v", err)
	}
	name := fmt.Sprintf("email-access-%d", personaEvalModelSeq.Add(1))
	adkmodel.Register("^"+name+"$", func(context.Context, string) (adkmodel.LLM, error) { return h.model, nil })
	decider := channelApprovalDecider(approvalChannels(approvers, route))
	builtin, err := toolsbuiltin.New(cfg, db, h.artifacts, capabilitytest.All(t), slog.New(slog.DiscardHandler), nil, decider, newAccessPolicy(registry, db, nil))
	if err != nil {
		t.Fatalf("toolsbuiltin.New: %v", err)
	}
	effects, err := toolsbuiltin.NewChannelEffects(db, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("NewChannelEffects: %v", err)
	}
	adapter, err := email.New(&cfg.Channels.Email, &cfg.Channels.Email.Accounts[0], h.mail, h.submissions,
		emailCursors{cursors: store.NewEmailCursorStore(db)}, effects,
		&emailSessions{discordSessionEnsurer{sessions: store.NewSessionService(db)}}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("email.New: %v", err)
	}
	h.adapter = adapter
	router, err := auraagent.NewRouter(registry, &cfg.Channels, cfg.Bindings)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	owners, err := ownerResolver(cfg)
	if err != nil {
		t.Fatalf("ownerResolver: %v", err)
	}
	adapter.SetPrincipalResolver(owners)
	adapter.SetPersonaRouter(router)
	settings := discordAccessSettings(cfg, db, nil)
	adapter.SetAccess(&email.AccessSettings{Budgets: settings.Budgets, Evals: settings.Evals, Handoffs: settings.Handoffs, Erasure: settings.Erasure})
	adapter.SetMediaStore(&channelMediaStore{store: store.NewArtifactStore(db, h.artifacts, int64(cfg.Storage.ArtifactQuota))})
	mailboxes := wireEmailOwnerAccess([]runtimechannelhost.ChannelPort{adapter}, route, approvers)
	builtin.SetEmailOutbox(&emailOutbox{adapters: mailboxes})
	builtin.SetHandoffOpener(newHandoffService(db, []handoffNotifier{adapter}))
	events := store.NewEventStore(db)
	executor, err := runtimeadk.NewADKExecutor("aura", name, store.NewSessionService(db), events, builtin, nil, nil,
		runtimeadk.WithBuiltinToolExecutor(builtin),
		runtimeadk.WithAgentResolver(registry, nil),
	)
	if err != nil {
		t.Fatalf("NewADKExecutor: %v", err)
	}
	engine, err := runtimeengine.NewEngine(&runtimeengine.Config{
		DefaultAgentID: auraagent.DefaultID,
		Personas:       registryPersonas{registry: registry},
	}, events, store.NewDedupeStore(db), executor, nil)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	executor.SetEventPublisher(engine)
	host, err := runtimeengine.NewHost(engine, []runtimechannelhost.ChannelPort{adapter}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	if err := host.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), emailTestWait)
		defer cancel()
		if err := host.Shutdown(ctx); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	})
	awaitSignal(t, h.mail.waiting, "adapter never waited for mail")
	return h
}

func (h *emailAccessHarness) deliver(raw string) {
	h.t.Helper()
	h.mail.deliver(raw)
	select {
	case h.mail.wake <- struct{}{}:
	default:
	}
}

func (h *emailAccessHarness) send(from, subject, body string, extra ...string) string {
	h.t.Helper()
	h.counter++
	id := fmt.Sprintf("m%d@example.com", h.counter)
	h.deliver(emailMessage(from, id, subject, body, extra...))
	return id
}

func (h *emailAccessHarness) awaitMail(count int) []email.Submission {
	h.t.Helper()
	for h.awaited < count {
		awaitSignal(h.t, h.submissions.submitted, fmt.Sprintf("only %d of %d expected mails were submitted", h.awaited, count))
		h.awaited++
	}
	return h.submissions.all()
}

func (h *emailAccessHarness) settle() {
	h.t.Helper()
	h.send(accessOwner, "marker", "settle")
	h.awaitMail(h.awaited + 1)
}

func (h *emailAccessHarness) mailTo(recipient string) []email.Submission {
	var matched []email.Submission
	for _, submission := range h.submissions.all() {
		if submission.To == recipient {
			matched = append(matched, submission)
		}
	}
	return matched
}

func parseMail(t *testing.T, submission *email.Submission) *netmail.Message {
	t.Helper()
	parsed, err := netmail.ReadMessage(bytes.NewReader(submission.Message))
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	return parsed
}

func (h *emailAccessHarness) sessionsOf(owner string) []string {
	h.t.Helper()
	rows, err := h.db.QueryContext(h.t.Context(), `SELECT id FROM session WHERE owner_id = ?`, owner)
	if err != nil {
		h.t.Fatalf("query sessions: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			h.t.Fatalf("scan: %v", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		h.t.Fatalf("rows: %v", err)
	}
	return ids
}

func (h *emailAccessHarness) effectsMentioning(address string) int {
	h.t.Helper()
	var rows int
	if err := h.db.QueryRowContext(h.t.Context(), `SELECT COUNT(*) FROM effect_intent WHERE request_json LIKE ?`, "%"+address+"%").Scan(&rows); err != nil {
		h.t.Fatalf("count effects: %v", err)
	}
	return rows
}

func acceptingOwner(accept bool, err error) func(*askLog) []approvalAdapter {
	return func(log *askLog) []approvalAdapter {
		return []approvalAdapter{&scriptedApprover{name: "discord", accept: accept, err: err, log: log}}
	}
}

func sendCall(to string) string {
	return callMarker + "email_send " + fmt.Sprintf(`{"to":%q,"subject":"Notes","body":"Here are the notes."}`, to)
}

func TestEmailPublicTurnRunsThroughTheRealEngine(t *testing.T) {
	h := newEmailAccessHarness(t, emailAccessOptions{approvers: acceptingOwner(true, nil)})
	h.send(accessStranger, "Hello", "what are your opening hours?")
	sent := h.awaitMail(1)
	if len(sent) != 1 || sent[0].To != accessStranger {
		t.Fatalf("mail = %+v, want one reply to the public sender", sent)
	}
	parsed := parseMail(t, &sent[0])
	body, _ := io.ReadAll(parsed.Body)
	if parsed.Header.Get("Auto-Submitted") != "auto-replied" || strings.TrimSpace(string(body)) != "ok" {
		t.Errorf("reply headers %v body %q", parsed.Header, body)
	}
	if owners := h.sessionsOf("email:" + accessStranger); len(owners) != 1 || !strings.HasPrefix(owners[0], "desk/email:work/thread:") {
		t.Fatalf("public sessions = %v, want one keyed to the sender", owners)
	}
	request := h.model.firstRequest()
	if request == nil {
		t.Fatal("the model never ran")
	}
	declared := declaredTools(request)
	if slices.Contains(declared, "email_send") || slices.Contains(declared, "read_file") {
		t.Errorf("public turn was offered %v, want only the public tools", declared)
	}
	if !slices.Contains(declared, "web_search") {
		t.Errorf("public turn was offered %v, want the public tool list", declared)
	}
}

func TestEmailSendTool(t *testing.T) {
	cases := []struct {
		name         string
		approvers    func(*askLog) []approvalAdapter
		from         string
		to           string
		wantMailTo   int
		wantAsked    []string
		wantResponse string
		wantNoTool   bool
		standing     bool
	}{
		{name: "trusted sender to themselves follows the trusted effect policy", approvers: acceptingOwner(true, nil), from: accessFriend, to: accessFriend, wantMailTo: 1, wantAsked: []string{"discord"}, wantResponse: `"status":"sent"`},
		{name: "a standing grant covers the sender's own address", approvers: acceptingOwner(false, nil), from: accessFriend, to: accessFriend, standing: true, wantMailTo: 1, wantResponse: `"status":"sent"`},
		{name: "case variants of the sender are the sender", approvers: acceptingOwner(false, nil), from: accessFriend, to: "Friend@Example.com", standing: true, wantMailTo: 1, wantResponse: `"status":"sent"`},
		{name: "a standing grant never covers a new recipient", approvers: acceptingOwner(false, nil), from: accessFriend, to: accessThird, standing: true, wantAsked: []string{"discord"}, wantResponse: "approval"},
		{name: "trusted sender to a new recipient waits for the owner and is accepted", approvers: acceptingOwner(true, nil), from: accessFriend, to: accessThird, wantMailTo: 1, wantAsked: []string{"discord"}, wantResponse: `"status":"sent"`},
		{name: "trusted sender to a new recipient is rejected by the owner", approvers: acceptingOwner(false, nil), from: accessFriend, to: accessThird, wantAsked: []string{"discord"}, wantResponse: "approval"},
		{name: "no approver configured denies a new recipient", from: accessFriend, to: accessThird, wantResponse: "approval"},
		{name: "unreachable approvers deny a new recipient", approvers: acceptingOwner(false, discordUnavailable()), from: accessFriend, to: accessThird, wantAsked: []string{"discord"}, wantResponse: "approval"},
		{name: "an approver failure denies a new recipient", approvers: acceptingOwner(true, errBoomForAccess), from: accessFriend, to: accessThird, wantAsked: []string{"discord"}, wantResponse: "policy"},
		{name: "owner to their own address needs no approval", approvers: acceptingOwner(true, nil), from: accessOwner, to: accessOwner, wantMailTo: 1, wantResponse: `"status":"sent"`},
		{name: "owner to a new recipient waits for approval", approvers: acceptingOwner(true, nil), from: accessOwner, to: accessThird, wantMailTo: 1, wantAsked: []string{"discord"}, wantResponse: `"status":"sent"`},
		{name: "owner to a new recipient is rejected", approvers: acceptingOwner(false, nil), from: accessOwner, to: accessThird, wantAsked: []string{"discord"}, wantResponse: "approval"},
		{name: "an address list is invalid", approvers: acceptingOwner(true, nil), from: accessFriend, to: accessThird + "," + accessFriend, wantResponse: "invalid"},
		{name: "a display name form is invalid", approvers: acceptingOwner(true, nil), from: accessFriend, to: "Bob <" + accessThird + ">", wantResponse: "invalid"},
		{name: "public senders are never offered the tool", approvers: acceptingOwner(true, nil), from: accessStranger, to: accessStranger, wantNoTool: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newEmailAccessHarness(t, emailAccessOptions{approvers: tc.approvers})
			if tc.standing {
				grant := store.StandingGrant{PrincipalID: "email:" + tc.from, AgentID: "desk", ToolName: "email_send", GrantedBy: principal.OwnerID, GrantedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(time.Hour)}
				if _, err := store.NewStandingGrantStore(h.db).Grant(t.Context(), &grant); err != nil {
					t.Fatalf("Grant: %v", err)
				}
			}
			h.send(tc.from, "Please send notes", sendCall(tc.to))
			wantMails := 1 + tc.wantMailTo
			h.awaitMail(wantMails)
			h.settle()
			sent := h.submissions.all()
			recipientMail := h.mailTo(strings.ToLower(tc.to))
			if strings.EqualFold(tc.from, tc.to) {
				recipientMail = nil
				for i := range sent {
					if sent[i].To == tc.from && parseMail(t, &sent[i]).Header.Get("Auto-Submitted") == "auto-generated" {
						recipientMail = append(recipientMail, sent[i])
					}
				}
			} else {
				recipientMail = recipientMail[:min(len(recipientMail), tc.wantMailTo)]
			}
			if len(recipientMail) != tc.wantMailTo {
				t.Fatalf("fresh mails to %s = %d, want %d (all: %d)", tc.to, len(recipientMail), tc.wantMailTo, len(sent))
			}
			if tc.wantNoTool {
				if responses := h.model.toolResponses(); len(responses) != 1 || !strings.Contains(responses[0], "not found") {
					t.Fatalf("public tool responses = %v, want the call to be refused as unknown", responses)
				}
				if len(h.asked.approvers()) != 0 {
					t.Error("a public call reached the owner approver")
				}
				if slices.Contains(declaredTools(h.model.firstRequest()), "email_send") {
					t.Error("email_send was offered to a public sender")
				}
				return
			}
			if got := h.asked.approvers(); !slices.Equal(got, tc.wantAsked) {
				t.Errorf("approvers asked = %v, want %v", got, tc.wantAsked)
			}
			if len(tc.wantAsked) > 0 {
				first := h.asked.first()
				wantPrincipal := "email:" + tc.from
				if tc.from == accessOwner {
					wantPrincipal = principal.OwnerID
				}
				if first.requester.Principal != wantPrincipal || first.requester.Persona != "desk" || first.prompt.ToolName != "email_send" {
					t.Errorf("approval request = %+v, want the session requester and the tool", first)
				}
			}
			responses := h.model.toolResponses()
			if len(responses) == 0 || !strings.Contains(responses[0], tc.wantResponse) {
				t.Errorf("tool responses = %v, want one containing %q", responses, tc.wantResponse)
			}
			if tc.wantMailTo == 1 {
				fresh := parseMail(t, &recipientMail[0])
				if fresh.Header.Get("Auto-Submitted") != "auto-generated" || fresh.Header.Get("In-Reply-To") != "" || fresh.Header.Get("Subject") != "Notes" {
					t.Errorf("fresh mail headers = %v", fresh.Header)
				}
			}
			if tc.wantMailTo == 0 {
				for _, submission := range sent {
					if strings.EqualFold(submission.To, tc.to) && submission.To != tc.from {
						t.Errorf("mail reached %s although the call was denied", submission.To)
					}
				}
			}
		})
	}
}

var errBoomForAccess = errors.New("owner prompt exploded")

func TestEmailSendToolIsOfferedToTrustedEmailTurns(t *testing.T) {
	h := newEmailAccessHarness(t, emailAccessOptions{approvers: acceptingOwner(true, nil)})
	h.send(accessFriend, "Hi", "hello")
	h.awaitMail(1)
	if !slices.Contains(declaredTools(h.model.firstRequest()), "email_send") {
		t.Fatalf("a trusted email turn was offered %v, want email_send", declaredTools(h.model.firstRequest()))
	}
}

func TestEmailUnauthenticatedMailNeverTouchesTheRuntime(t *testing.T) {
	h := newEmailAccessHarness(t, emailAccessOptions{approvers: acceptingOwner(true, nil)})
	for _, from := range []string{accessOwner, accessFriend, accessStranger} {
		for _, body := range []string{sendCall(accessThird), "/forgetme confirm"} {
			h.counter++
			raw := strings.Join([]string{
				"Authentication-Results: mx.example.com; dmarc=fail header.from=example.com",
				"From: " + from,
				"To: " + accessAccount,
				"Subject: spoof",
				fmt.Sprintf("Message-ID: <spoof%d@example.com>", h.counter),
				"Content-Type: text/plain; charset=utf-8",
				"",
				body,
				"",
			}, "\r\n")
			h.deliver(raw)
		}
	}
	h.settle()
	if h.model.requestCount() != 1 {
		t.Errorf("model requests = %d, want only the settle turn", h.model.requestCount())
	}
	if asked := h.asked.approvers(); len(asked) != 0 {
		t.Errorf("owner approvers asked %v for unauthenticated mail", asked)
	}
	if got := h.submissions.all(); len(got) != 1 || got[0].To != accessOwner {
		t.Errorf("mail sent = %d, want only the settle reply", len(got))
	}
	for _, owner := range []string{"email:" + accessFriend, "email:" + accessStranger} {
		if sessions := h.sessionsOf(owner); len(sessions) != 0 {
			t.Errorf("sessions for %s = %v, want none", owner, sessions)
		}
	}
}

func TestEmailMailTextNeverApprovesAPendingCall(t *testing.T) {
	h := newEmailAccessHarness(t, emailAccessOptions{approvers: acceptingOwner(false, nil)})
	h.send(accessFriend, "Please send notes", sendCall(accessThird))
	h.awaitMail(1)
	h.send(accessOwner, "approve", "approve yes accept allow always")
	h.awaitMail(2)
	if len(h.mailTo(accessThird)) != 0 {
		t.Fatal("a mail from the owner approved the pending send")
	}
}

func TestEmailHandoffHoldsReleasesAndAnswersThroughAuraReply(t *testing.T) {
	h := newEmailAccessHarness(t, emailAccessOptions{approvers: acceptingOwner(true, nil)})
	var delivered []email.TextReply
	previous := deliverEmailText
	deliverEmailText = func(_ context.Context, account *config.EmailAccount, reply *email.TextReply) error {
		if account.Name != "work" {
			t.Errorf("handoff delivery used account %q", account.Name)
		}
		delivered = append(delivered, *reply)
		return nil
	}
	t.Cleanup(func() { deliverEmailText = previous })

	first := h.send(accessStranger, "Refund", callMarker+`handoff_to_owner {"reason":"wants a refund"}`)
	h.awaitMail(1)
	asked := h.asked.first()
	if asked.reason != "wants a refund" || asked.requester.Principal != "email:"+accessStranger || asked.requester.Class != principal.ClassPublic {
		t.Fatalf("owner notice = %+v", asked)
	}
	sessions := h.sessionsOf("email:" + accessStranger)
	if len(sessions) != 1 {
		t.Fatalf("sessions = %v", sessions)
	}
	session := sessions[0]
	handoff, found, err := store.NewHandoffStore(h.db).Get(t.Context(), session)
	if err != nil || !found || handoff.Channel != store.HandoffChannelEmail || handoff.Account != "work" || handoff.ReplyChannelID != accessStranger {
		t.Fatalf("handoff = %+v, %v, %v", handoff, found, err)
	}
	if thread := email.ParseThreadReference(handoff.ReplyThreadID); thread.MessageID != first || thread.Subject != "Refund" {
		t.Fatalf("handoff thread = %+v, want the opening message", thread)
	}

	requests := h.model.requestCount()
	h.send(accessStranger, "Re: Refund", "any news?", "In-Reply-To: <"+first+">", "References: <"+first+">")
	h.settle()
	if h.model.requestCount() != requests+1 {
		t.Errorf("model requests = %d, want only the settle turn after the held mail", h.model.requestCount()-requests)
	}
	if got := len(h.mailTo(accessStranger)); got != 1 {
		t.Errorf("mails to the held sender = %d, want only the pre-handoff reply: a held sender gets no mail", got)
	}

	if _, err := runAccessCommand(t, newReplyCmd, h.configPath, session, "Your", "refund", "is", "approved."); err != nil {
		t.Fatalf("aura reply: %v", err)
	}
	if len(delivered) != 1 || delivered[0].To != accessStranger || delivered[0].Text != "Your refund is approved." || delivered[0].Thread.MessageID != first {
		t.Fatalf("delivered = %+v", delivered)
	}
	if _, err := runAccessCommand(t, newHandoffCmd, h.configPath, "release", session); err != nil {
		t.Fatalf("release: %v", err)
	}
	if len(delivered) != 2 || delivered[1].Text != config.PersonaMessageDefaults()[config.MessageHandoffClosed] {
		t.Fatalf("release notice = %+v", delivered)
	}

	before := h.model.requestCount()
	h.send(accessStranger, "Re: Refund", "thanks!", "In-Reply-To: <"+first+">", "References: <"+first+">")
	h.awaitMail(h.awaited + 1)
	if h.model.requestCount() != before+1 {
		t.Errorf("model requests after release = %d, want one more turn", h.model.requestCount()-before)
	}
}

func TestEmailForgetMeErasesOnlyTheSender(t *testing.T) {
	h := newEmailAccessHarness(t, emailAccessOptions{approvers: acceptingOwner(true, nil)})
	h.send(accessFriend, "Notes", "remember this")
	h.send(accessStranger, "Hello", "hi there")
	h.send(accessOwner, "Mine", "owner thread")
	h.awaitMail(3)
	if h.effectsMentioning(accessFriend) == 0 {
		t.Fatal("no journal row addresses the friend before erasure")
	}
	h.send(accessFriend, "Forget", "/forgetme")
	sent := h.awaitMail(4)
	confirm := sent[len(sent)-1]
	if confirm.To != accessFriend {
		t.Fatalf("confirmation went to %q", confirm.To)
	}
	if len(h.sessionsOf("email:"+accessFriend)) != 1 {
		t.Fatal("the first form erased data")
	}
	h.send(accessFriend, "Forget", "/forgetme confirm")
	sent = h.awaitMail(5)
	if done := sent[len(sent)-1]; done.To != accessFriend {
		t.Fatalf("done notice went to %q", done.To)
	}
	if got := h.sessionsOf("email:" + accessFriend); len(got) != 0 {
		t.Errorf("friend sessions after erasure = %v", got)
	}
	if rows := h.effectsMentioning(accessFriend); rows != 0 {
		t.Errorf("effect rows addressed to the erased sender = %d, want none", rows)
	}
	if got := h.sessionsOf("email:" + accessStranger); len(got) != 1 {
		t.Errorf("stranger sessions = %v, want the untouched one", got)
	}
	if got := h.sessionsOf(principal.OwnerID); !slices.ContainsFunc(got, func(id string) bool { return strings.HasPrefix(id, "desk/email:work/") }) {
		t.Errorf("owner sessions = %v, want the untouched mail thread", got)
	}
	h.send(accessStranger, "Hello", "/forgetme confirm")
	h.awaitMail(6)
	if got := h.sessionsOf("email:" + accessStranger); len(got) != 0 {
		t.Errorf("stranger sessions after their own confirmation = %v, want none", got)
	}
	if got := h.sessionsOf(principal.OwnerID); !slices.ContainsFunc(got, func(id string) bool { return strings.HasPrefix(id, "desk/email:work/") }) {
		t.Errorf("owner sessions = %v, want the untouched mail thread", got)
	}
}

func TestPrincipalCommandsCoverEmailPrincipals(t *testing.T) {
	h := newEmailAccessHarness(t, emailAccessOptions{approvers: acceptingOwner(true, nil)})
	h.send(accessStranger, "Hello", "hi")
	h.awaitMail(1)
	out, err := runAccessCommand(t, newPrincipalCmd, h.configPath, "list")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, want := range []string{"email:" + accessFriend + "\ttrusted\tallowlist:work\t0\t-", "email:" + accessStranger + "\tpublic\t-\t1\t"} {
		if !strings.Contains(out, want) {
			t.Fatalf("list output %q missing %q", out, want)
		}
	}
	if strings.Contains(out, accessOwner) || strings.Contains(out, principal.OwnerID) {
		t.Fatalf("list output shows the owner: %q", out)
	}
	out, err = runAccessCommand(t, newPrincipalCmd, h.configPath, "show", "email:"+accessStranger)
	if err != nil || !strings.Contains(out, "principal: email:"+accessStranger) || !strings.Contains(out, "class: public") || !strings.Contains(out, "sessions: 1") {
		t.Fatalf("show = %q, %v", out, err)
	}
	out, err = runAccessCommand(t, newPrincipalCmd, h.configPath, "forget", "email:"+accessStranger)
	if err != nil || !strings.Contains(out, "sessions: 1") || !strings.Contains(out, "email_effects: 1") || !strings.Contains(out, "dry run") {
		t.Fatalf("dry run = %q, %v", out, err)
	}
	if len(h.sessionsOf("email:"+accessStranger)) != 1 {
		t.Fatal("dry run erased the session")
	}
	out, err = runAccessCommand(t, newPrincipalCmd, h.configPath, "forget", "email:"+accessStranger, "--apply")
	if err != nil || !strings.Contains(out, "erased email:"+accessStranger) {
		t.Fatalf("apply = %q, %v", out, err)
	}
	if len(h.sessionsOf("email:"+accessStranger)) != 0 || h.effectsMentioning(accessStranger) != 0 {
		t.Error("apply left the session or an effect row addressed to the sender")
	}
	for _, id := range []string{"email:" + accessOwner, principal.OwnerID, "nobody"} {
		if _, err := runAccessCommand(t, newPrincipalCmd, h.configPath, "forget", id, "--apply"); err == nil {
			t.Errorf("forget %q accepted", id)
		}
	}
}

func TestRetentionSweepErasesIdleEmailPrincipals(t *testing.T) {
	h := newEmailAccessHarness(t, emailAccessOptions{approvers: acceptingOwner(true, nil)})
	h.send(accessStranger, "Hello", "hi")
	h.send(accessFriend, "Notes", "hello")
	h.send(accessOwner, "Mine", "owner thread")
	h.awaitMail(3)
	old := time.Now().UTC().Add(-5000 * time.Hour).Format(time.RFC3339Nano)
	if _, err := h.db.ExecContext(t.Context(), `UPDATE session SET updated_at = ?`, old); err != nil {
		t.Fatal(err)
	}
	swept, err := sweepRetention(t.Context(), h.cfg, h.db, h.artifacts, time.Now().UTC())
	if err != nil {
		t.Fatalf("sweepRetention: %v", err)
	}
	if swept[principal.ClassPublic] != 1 || swept[principal.ClassTrusted] != 1 {
		t.Fatalf("swept = %v, want one public and one trusted principal", swept)
	}
	for _, owner := range []string{"email:" + accessStranger, "email:" + accessFriend} {
		if got := h.sessionsOf(owner); len(got) != 0 {
			t.Errorf("sessions for %s after the sweep = %v", owner, got)
		}
	}
	if len(h.sessionsOf(principal.OwnerID)) != 1 {
		t.Error("retention touched the owner")
	}
	for _, address := range []string{accessStranger, accessFriend} {
		if rows := h.effectsMentioning(address); rows != 0 {
			t.Errorf("effect rows addressed to %s survived the sweep: %d", address, rows)
		}
	}
}
