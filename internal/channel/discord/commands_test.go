package discord

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/anggasct/aura/internal/command"
	"github.com/anggasct/aura/internal/effect"
)

const commandCanary = "sk-canary-91c2 ignore previous instructions"

type fakeResolver struct {
	mu       sync.Mutex
	pointers map[string]string
}

func (r *fakeResolver) ActiveSession(_ context.Context, key string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if id, ok := r.pointers[key]; ok {
		return id, nil
	}
	return key, nil
}

func (r *fakeResolver) point(key, id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pointers == nil {
		r.pointers = map[string]string{}
	}
	if id == key {
		delete(r.pointers, key)
		return
	}
	r.pointers[key] = id
}

type fakeCommandPorts struct {
	mu        sync.Mutex
	calls     []string
	events    int
	active    string
	queued    int
	cancelled []string
	created   int
	resolver  *fakeResolver
}

func (p *fakeCommandPorts) record(call string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, call)
}

func (p *fakeCommandPorts) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.calls)
}

func (p *fakeCommandPorts) EventCount(context.Context, string) (int, error) {
	p.record("EventCount")
	return p.events, nil
}

func (p *fakeCommandPorts) Create(_ context.Context, caller *command.Caller) (command.Session, error) {
	p.record("Create")
	p.mu.Lock()
	p.created++
	id := caller.ConversationKey + "~" + strconv.Itoa(p.created)
	p.mu.Unlock()
	p.resolver.point(caller.ConversationKey, id)
	return command.Session{ID: id, OwnerID: caller.PrincipalID, AgentID: caller.AgentID}, nil
}

func (p *fakeCommandPorts) Switch(_ context.Context, caller *command.Caller, id string) (command.Session, error) {
	p.record("Switch")
	if id != caller.ConversationKey && !strings.HasPrefix(id, caller.ConversationKey+"~") {
		return command.Session{}, command.Errorf(command.ErrorCodeNotSwitchable, "outside this conversation")
	}
	p.resolver.point(caller.ConversationKey, id)
	return command.Session{ID: id, OwnerID: caller.PrincipalID, AgentID: caller.AgentID}, nil
}

func (p *fakeCommandPorts) CancelActive(_ context.Context, sessionID string) (turnID string, cancelled bool, err error) {
	p.record("CancelActive")
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.active == "" {
		return "", false, nil
	}
	p.cancelled = append(p.cancelled, sessionID)
	return p.active, true, nil
}

func (p *fakeCommandPorts) Activity(string) (activeTurnID string, queued int) {
	p.record("Activity")
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.active, p.queued
}

type commandObservation struct {
	name, class, result string
}

type commandRecorderSpy struct {
	mu   sync.Mutex
	seen []commandObservation
}

func (r *commandRecorderSpy) Start(ctx context.Context, name, _, class string) (spanCtx context.Context, finish func(string)) {
	return ctx, func(result string) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.seen = append(r.seen, commandObservation{name: name, class: class, result: result})
	}
}

func (r *commandRecorderSpy) observed() []commandObservation {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.seen)
}

type commandFixture struct {
	adapter  *Adapter
	sink     *memorySink
	stub     *restStub
	sessions *fakeSessionEnsurer
	effects  *fakeEffectRunner
	ports    *fakeCommandPorts
	resolver *fakeResolver
	recorder *commandRecorderSpy
	logs     *logCapture
}

func newCommandFixture(t *testing.T) *commandFixture {
	t.Helper()
	resolver := &fakeResolver{}
	f := &commandFixture{
		sink:     newMemorySink(),
		stub:     threadCreateStub(t, "777"),
		sessions: &fakeSessionEnsurer{},
		ports:    &fakeCommandPorts{events: 4, resolver: resolver},
		resolver: resolver,
		recorder: &commandRecorderSpy{},
		logs:     &logCapture{},
	}
	f.adapter = f.newAdapter(t)
	effects, ok := f.adapter.effects.(*fakeEffectRunner)
	if !ok {
		t.Fatalf("effects = %T", f.adapter.effects)
	}
	f.effects = effects
	return f
}

func (f *commandFixture) newAdapter(t *testing.T) *Adapter {
	t.Helper()
	adapter := newThreadAdapter(t, f.sink, f.sessions, f.stub)
	adapter.account.AllowedUserIDs = append(adapter.account.AllowedUserIDs, "112")
	adapter.SetPrincipalResolver(testOwners(t, "discord:111"))
	adapter.SetSessionResolver(f.resolver)
	adapter.logger = slog.New(slog.NewTextHandler(f.logs, nil))
	registry, err := command.New(CommandBinding())
	if err != nil {
		t.Fatalf("command.New: %v", err)
	}
	dispatcher, err := command.NewDispatcher(registry, command.Ports{Sessions: f.ports, Turns: f.ports}, f.recorder)
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	adapter.SetCommands(dispatcher)
	return adapter
}

func (f *commandFixture) admit(t *testing.T, msg *messagePayload) {
	t.Helper()
	admitted, err := f.adapter.admit(t.Context(), msg)
	if err != nil {
		t.Fatalf("admit(%q): %v", msg.Content, err)
	}
	if !admitted {
		t.Fatalf("admit(%q) = false", msg.Content)
	}
}

type sentReply struct {
	channel string
	content string
	replyTo string
}

func (f *commandFixture) replies(t *testing.T) []sentReply {
	t.Helper()
	var out []sentReply
	for _, call := range f.stub.messageSends(0) {
		var body struct {
			Content   string `json:"content"`
			Reference *struct {
				MessageID string `json:"message_id"`
			} `json:"message_reference"`
		}
		if err := json.Unmarshal(call.body, &body); err != nil {
			t.Fatalf("reply body: %v", err)
		}
		reply := sentReply{channel: strings.TrimSuffix(strings.TrimPrefix(call.path, "/channels/"), "/messages"), content: body.Content}
		if body.Reference != nil {
			reply.replyTo = body.Reference.MessageID
		}
		out = append(out, reply)
	}
	return out
}

func (f *commandFixture) threadCreates() int {
	f.stub.mu.Lock()
	defer f.stub.mu.Unlock()
	n := 0
	for _, call := range f.stub.calls {
		if call.method == http.MethodPost && strings.HasSuffix(call.path, "/threads") {
			n++
		}
	}
	return n
}

func dmMessage(id, author, content string) *messagePayload {
	return textMessage(id, "555", "", author, content, nil)
}

func TestDiscordCommandsInDMReplyWithoutATurn(t *testing.T) {
	f := newCommandFixture(t)
	f.ports.active, f.ports.queued = "turn-9", 2
	f.admit(t, dmMessage("8001", "111", "/status"))
	f.admit(t, dmMessage("8002", "111", "/session"))
	f.admit(t, dmMessage("8003", "111", "/cancel"))
	key := testSessionKey("dm:111")
	want := []sentReply{
		{channel: "555", replyTo: "8001", content: "session " + key + "\npersona main\nevents 4\nturn running turn-9 queued 2"},
		{channel: "555", replyTo: "8002", content: "session " + key},
		{channel: "555", replyTo: "8003", content: "cancelled turn turn-9"},
	}
	if got := f.replies(t); !slices.Equal(got, want) {
		t.Fatalf("replies =\n%+v\nwant\n%+v", got, want)
	}
	if calls, turns := f.sink.stats("message:8001"); calls != 0 || turns != 0 {
		t.Errorf("command reached the ingress sink: %d %d", calls, turns)
	}
	if !slices.Equal(f.ports.cancelled, []string{key}) {
		t.Errorf("cancelled sessions = %v, want %s", f.ports.cancelled, key)
	}
	if _, ok := ensuredOwner(f.sessions, key); !ok {
		t.Errorf("conversation session %s was not ensured", key)
	}
	for _, req := range f.effects.requests {
		if req.Provider == ProviderCommand && (req.SessionID != key || req.EventKind != effect.EventKindChannelRequested || !strings.HasPrefix(req.IdempotencyKey, "discord:owner:message:")) {
			t.Errorf("command intent = %+v", req)
		}
	}
}

func TestDiscordCommandRecognitionInGuildChannels(t *testing.T) {
	tests := []struct {
		name      string
		content   string
		command   bool
		wantReply string
	}{
		{"mention then help", "<@999> /help", true, "commands:"},
		{"nickname mention and case", "<@!999>   /HELP", true, "commands:"},
		{"session command in parent", "<@999> /status", true, "Use /status inside an Aura thread or a DM."},
		{"terminal-only in parent", "<@999> /exit", true, "/exit only works in the terminal."},
		{"mention mid-text", "hi <@999> /status", false, ""},
		{"unknown slash name", "<@999> /bogus", false, ""},
		{"new in parent", "<@999> /new", true, "Use /new inside an Aura thread or a DM."},
		{"unbound portable name", "<@999> /skill pdf", false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newCommandFixture(t)
			f.admit(t, threadTrigger("9100", tt.content, []string{"999"}))
			calls, _ := f.sink.stats("message:9100")
			if tt.command {
				replies := f.replies(t)
				if len(replies) != 1 || !strings.HasPrefix(replies[0].content, tt.wantReply) || replies[0].channel != "333" || replies[0].replyTo != "9100" {
					t.Fatalf("replies = %+v, want %q in 333", replies, tt.wantReply)
				}
				if calls != 0 || f.threadCreates() != 0 {
					t.Errorf("local command created a turn (%d) or thread (%d)", calls, f.threadCreates())
				}
				return
			}
			if calls != 1 || f.threadCreates() != 1 {
				t.Errorf("ordinary input: turns = %d threads = %d, want 1 and 1", calls, f.threadCreates())
			}
		})
	}
}

func TestDiscordCommandsInBotThreadUseTheThreadSession(t *testing.T) {
	f := newCommandFixture(t)
	f.admit(t, threadTrigger("9200", "<@999> start a thread", []string{"999"}))
	f.admit(t, threadFollowUp("9201", "777", "/session"))
	f.admit(t, threadFollowUp("9202", "777", "<@999> /status"))
	replies := f.replies(t)
	var commandReplies []sentReply
	for _, reply := range replies {
		if reply.replyTo == "9201" || reply.replyTo == "9202" {
			commandReplies = append(commandReplies, reply)
		}
	}
	key := testSessionKey("thread:777")
	if len(commandReplies) != 2 || commandReplies[0].content != "session "+key || !strings.HasPrefix(commandReplies[1].content, "session "+key+"\npersona main") {
		t.Fatalf("thread replies = %+v", commandReplies)
	}
	if commandReplies[0].channel != "777" {
		t.Errorf("thread reply channel = %s, want 777", commandReplies[0].channel)
	}
	if calls, _ := f.sink.stats("message:9201"); calls != 0 {
		t.Errorf("thread command reached the ingress sink")
	}
	if f.threadCreates() != 1 {
		t.Errorf("threads created = %d, want only the trigger thread", f.threadCreates())
	}
}

func TestDiscordTerminalOnlyNamesAreUnavailable(t *testing.T) {
	f := newCommandFixture(t)
	for i, name := range []string{"exit", "quit", "clear"} {
		f.admit(t, dmMessage("830"+string(rune('0'+i)), "112", "/"+name+" now"))
	}
	replies := f.replies(t)
	for i, name := range []string{"exit", "quit", "clear"} {
		if replies[i].content != "/"+name+" only works in the terminal." {
			t.Errorf("reply %d = %q", i, replies[i].content)
		}
	}
	if f.ports.callCount() != 0 {
		t.Errorf("terminal-only names reached ports: %v", f.ports.calls)
	}
	for _, seen := range f.recorder.observed() {
		if seen.name != command.NameTerminalOnly || seen.result != command.ResultUnavailable {
			t.Errorf("observed = %+v", seen)
		}
	}
}

func TestDiscordTrustedPrincipalIsDeniedOwnerCommands(t *testing.T) {
	f := newCommandFixture(t)
	f.ports.active = "turn-1"
	for i, text := range []string{"/status", "/session", "/session " + testSessionKey("dm:112"), "/cancel"} {
		f.admit(t, dmMessage("840"+string(rune('0'+i)), "112", text))
	}
	for _, reply := range f.replies(t) {
		if reply.content != command.ReplyDenied {
			t.Errorf("reply = %q, want denial", reply.content)
		}
	}
	if f.ports.callCount() != 0 {
		t.Errorf("denied commands reached ports: %v", f.ports.calls)
	}
	seen := f.recorder.observed()
	if len(seen) != 4 {
		t.Fatalf("observed = %+v", seen)
	}
	for _, obs := range seen {
		if obs.class != "trusted" || obs.result != command.ResultDenied {
			t.Errorf("observed = %+v", obs)
		}
	}
	if !strings.Contains(f.logs.String(), "level=WARN") || !strings.Contains(f.logs.String(), "result=denied") {
		t.Errorf("logs = %s, want a warn line per denial", f.logs.String())
	}
}

func TestDiscordHelpGoldensPerClass(t *testing.T) {
	f := newCommandFixture(t)
	f.admit(t, dmMessage("8501", "111", "/help"))
	f.admit(t, dmMessage("8502", "112", "/help"))
	replies := f.replies(t)
	owner := "commands:\n" +
		"  /help          show this help\n" +
		"  /status        show the current session\n" +
		"  /session [id]  show or switch to a session\n" +
		"  /new           start a new session\n" +
		"  /cancel        cancel the active turn"
	trusted := "commands:\n  /help  show this help"
	if len(replies) != 2 || replies[0].content != owner || replies[1].content != trusted {
		t.Fatalf("help replies = %+v", replies)
	}
}

func TestDiscordCommandRedeliveryRepliesOnce(t *testing.T) {
	f := newCommandFixture(t)
	f.ports.active = "turn-1"
	msg := dmMessage("8601", "111", "/cancel")
	f.admit(t, msg)
	f.admit(t, msg)
	if replies := f.replies(t); len(replies) != 1 || replies[0].content != "cancelled turn turn-1" {
		t.Fatalf("replies = %+v, want one", replies)
	}
	if len(f.ports.cancelled) != 1 {
		t.Errorf("cancel executed %d times, want 1", len(f.ports.cancelled))
	}
	seen := f.recorder.observed()
	if len(seen) != 2 || seen[0].result != command.ResultOK || seen[1].result != command.ResultReplayed || seen[1].name != "cancel" {
		t.Errorf("observed = %+v, want ok then replayed", seen)
	}
}

func TestDiscordCommandPrivacy(t *testing.T) {
	f := newCommandFixture(t)
	for i, text := range []string{"/status " + commandCanary, "/help " + commandCanary, "/exit " + commandCanary} {
		f.admit(t, dmMessage("870"+string(rune('0'+i)), "111", text))
		f.admit(t, dmMessage("871"+string(rune('0'+i)), "112", text))
	}
	if strings.Contains(f.logs.String(), "canary") {
		t.Errorf("logs carry command arguments: %s", f.logs.String())
	}
	for _, req := range f.effects.requests {
		if req.Provider == ProviderCommand && strings.Contains(string(req.Request), "canary") {
			t.Errorf("command intent carries arguments: %s", req.Request)
		}
	}
	for _, obs := range f.recorder.observed() {
		if strings.Contains(obs.name+obs.class+obs.result, "canary") {
			t.Errorf("observation carries arguments: %+v", obs)
		}
	}
	f.admit(t, dmMessage("8720", "111", "/session "+testSessionKey("dm:111")))
	var target string
	for _, req := range f.effects.requests {
		if req.Provider == ProviderCommand && req.Operation == "session" {
			var body commandRequest
			_ = json.Unmarshal(req.Request, &body)
			target = body.Target
		}
	}
	if target != testSessionKey("dm:111") {
		t.Errorf("session intent target = %q, want the session id", target)
	}
}

func TestDiscordWithoutCommandsKeepsSlashTextAsInput(t *testing.T) {
	sink := newMemorySink()
	adapter := newThreadAdapter(t, sink, &fakeSessionEnsurer{}, threadCreateStub(t, "777"))
	if admitted, err := adapter.admit(t.Context(), dmMessage("8801", "111", "/status")); err != nil || !admitted {
		t.Fatalf("admit = %v %v", admitted, err)
	}
	if calls, _ := sink.stats("message:8801"); calls != 1 {
		t.Errorf("turns = %d, want /status to stay model input without a dispatcher", calls)
	}
}

func TestStripLeadingMentions(t *testing.T) {
	tests := map[string]string{
		"<@999> /help":           "/help",
		"<@!999><@999>  /help x": "/help x",
		"  \n<@999>\t/help":      "/help",
		"<@123> /help":           "<@123> /help",
		"/help <@999>":           "/help <@999>",
	}
	for in, want := range tests {
		if got := stripLeadingMentions(in, "999"); got != want {
			t.Errorf("stripLeadingMentions(%q) = %q, want %q", in, got, want)
		}
	}
	if got := stripLeadingMentions(" <@999> /help", ""); got != "<@999> /help" {
		t.Errorf("without self id = %q", got)
	}
}

func envelopeSession(t *testing.T, sink *memorySink, externalID string) string {
	t.Helper()
	sink.mu.Lock()
	defer sink.mu.Unlock()
	env := sink.envelopes[externalID]
	if env == nil {
		t.Fatalf("no envelope for %s", externalID)
	}
	return env.ConversationID
}

func TestDiscordNewStartsAFreshSessionInADM(t *testing.T) {
	f := newCommandFixture(t)
	key := testSessionKey("dm:111")
	f.admit(t, dmMessage("8901", "111", "hello before"))
	f.admit(t, dmMessage("8902", "111", "/new"))
	f.admit(t, dmMessage("8903", "111", "hello after"))
	f.admit(t, dmMessage("8904", "111", "/session"))
	if got := envelopeSession(t, f.sink, "message:8901"); got != key {
		t.Errorf("turn before /new ran in %s, want %s", got, key)
	}
	if got := envelopeSession(t, f.sink, "message:8903"); got != key+"~1" {
		t.Errorf("turn after /new ran in %s, want %s~1", got, key)
	}
	var contents []string
	for _, reply := range f.replies(t) {
		contents = append(contents, reply.content)
	}
	if !slices.Equal(contents, []string{"new session " + key + "~1", "session " + key + "~1"}) {
		t.Errorf("replies = %q", contents)
	}
	if owner, ok := ensuredOwner(f.sessions, key); !ok || owner != "local-owner" {
		t.Errorf("base session %s = %q %v, want it kept for the conversation", key, owner, ok)
	}
}

func TestDiscordNewInABotThreadSurvivesARestart(t *testing.T) {
	f := newCommandFixture(t)
	key := testSessionKey("thread:777")
	f.admit(t, threadTrigger("9300", "<@999> open a thread", []string{"999"}))
	f.admit(t, threadFollowUp("9301", "777", "/new"))
	f.admit(t, threadFollowUp("9302", "777", "in the new session"))
	if got := envelopeSession(t, f.sink, "message:9302"); got != key+"~1" {
		t.Fatalf("thread turn after /new ran in %s, want %s~1", got, key)
	}
	f.adapter = f.newAdapter(t)
	f.admit(t, threadFollowUp("9303", "777", "after restart"))
	if got := envelopeSession(t, f.sink, "message:9303"); got != key+"~1" {
		t.Fatalf("thread turn after restart ran in %s, want %s~1 (probing still finds the base key)", got, key)
	}
	if _, ok := ensuredOwner(f.sessions, key); !ok {
		t.Errorf("base thread session %s missing", key)
	}
}

func TestDiscordNewRedeliveryCreatesOneSession(t *testing.T) {
	f := newCommandFixture(t)
	msg := dmMessage("8950", "111", "/new")
	f.admit(t, msg)
	f.admit(t, msg)
	if f.ports.created != 1 {
		t.Errorf("sessions created = %d, want 1", f.ports.created)
	}
	if replies := f.replies(t); len(replies) != 1 {
		t.Errorf("replies = %+v, want one", replies)
	}
	seen := f.recorder.observed()
	if len(seen) != 2 || seen[1] != (commandObservation{name: "new", class: "owner", result: command.ResultReplayed}) {
		t.Errorf("observed = %+v", seen)
	}
}

func TestDiscordSessionSwitchStaysInsideTheConversation(t *testing.T) {
	f := newCommandFixture(t)
	key := testSessionKey("dm:111")
	f.admit(t, dmMessage("8960", "111", "/new"))
	f.admit(t, dmMessage("8961", "111", "/new"))
	f.admit(t, dmMessage("8962", "111", "/session "+key+"~1"))
	f.admit(t, dmMessage("8963", "111", "/session "+testSessionKey("dm:112")))
	f.admit(t, dmMessage("8964", "111", "/session "+key))
	f.admit(t, dmMessage("8965", "111", "back on the base"))
	var contents []string
	for _, reply := range f.replies(t) {
		contents = append(contents, reply.content)
	}
	want := []string{
		"new session " + key + "~1",
		"new session " + key + "~2",
		"switched to session " + key + "~1",
		command.ReplyNotSwitchable,
		"switched to session " + key,
	}
	if !slices.Equal(contents, want) {
		t.Fatalf("replies =\n%q\nwant\n%q", contents, want)
	}
	if got := envelopeSession(t, f.sink, "message:8965"); got != key {
		t.Errorf("turn after switching back ran in %s, want %s", got, key)
	}
}
