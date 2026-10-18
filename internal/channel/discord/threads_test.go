package discord

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/memory"
	runtimechannelhost "github.com/anggasct/aura/internal/runtime/channelhost"
	runtimeingress "github.com/anggasct/aura/internal/runtime/ingress"
)

func newThreadAdapter(t *testing.T, sink *memorySink, sessions *fakeSessionEnsurer, stub *restStub) *Adapter {
	t.Helper()
	t.Setenv("AURA_DISCORD_BOT_TOKEN", testToken)
	cfg := testAdapterConfig()
	doubles := &testDoubles{effects: newFakeEffectRunner(), media: &fakeMediaStore{}, sessions: sessions}
	adapter, err := New(&cfg, &memoryResumeStore{}, doubles.effects, doubles.media, doubles.sessions, nil)
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	adapter.rateCap = 30 * time.Millisecond
	adapter.restBase = stub.srv.URL
	adapter.selfID = "999"
	adapter.sink = sink
	return adapter
}

func deliveryForEnvelope(t *testing.T, effectID string, env *runtimeingress.IngressEnvelope, text string) *runtimechannelhost.DeliveryRequest {
	t.Helper()
	if env == nil {
		t.Fatal("nil envelope for delivery")
	}
	return &runtimechannelhost.DeliveryRequest{
		EffectID:       effectID,
		Channel:        "discord",
		ConversationID: env.ConversationID,
		ReplyContext:   env.ReplyContext,
		Parts:          []runtimechannelhost.OutputPart{{Text: text}},
		IdempotencyKey: "key-" + effectID,
	}
}

func threadTrigger(id, content string, mentions []string) *messagePayload {
	users := make([]userPayload, 0, len(mentions))
	for _, mention := range mentions {
		users = append(users, userPayload{ID: mention})
	}
	return &messagePayload{
		ID:        id,
		ChannelID: "333",
		GuildID:   "222",
		Author:    userPayload{ID: "111"},
		Content:   content,
		Mentions:  users,
		Type:      0,
	}
}

func threadFollowUp(id, threadID, content string) *messagePayload {
	return &messagePayload{
		ID:        id,
		ChannelID: threadID,
		GuildID:   "222",
		Author:    userPayload{ID: "111"},
		Content:   content,
		Type:      0,
	}
}

func threadCreateStub(t *testing.T, threadID string) *restStub {
	t.Helper()
	var seq atomic.Int64
	return newRestStub(t, func(call int, w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/threads") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"` + threadID + `","parent_id":"333","owner_id":"999","type":11}`))
			return
		}
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/messages") {
			n := seq.Add(1)
			writeMessage(t, w, "m-thread-"+strconv.FormatInt(n, 10))
			return
		}
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"` + threadID + `","parent_id":"333","owner_id":"999","type":11}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
}

func envelopeThreadID(t *testing.T, sink *memorySink, externalID string) (string, replyReference) {
	t.Helper()
	sink.mu.Lock()
	env := sink.envelopes[externalID]
	sink.mu.Unlock()
	if env == nil {
		t.Fatalf("no envelope for %s", externalID)
		return "", replyReference{}
	}
	var ref replyReference
	if err := json.Unmarshal(env.ReplyContext, &ref); err != nil {
		t.Fatalf("reply context: %v", err)
	}
	return env.ConversationID, ref
}

func TestThreads_MentionCreatesThreadAndDeliversInside(t *testing.T) {
	sink := newMemorySink()
	sessions := &fakeSessionEnsurer{}
	stub := threadCreateStub(t, "777001")
	adapter := newThreadAdapter(t, sink, sessions, stub)

	admitted, err := adapter.admit(t.Context(), threadTrigger("9001", "hello aura", []string{"999"}))
	if err != nil {
		t.Fatalf("admit(): %v", err)
	}
	if !admitted {
		t.Fatal("admit() = false, want thread admission")
	}
	conv, ref := envelopeThreadID(t, sink, "message:9001")
	if conv != "thread:777001" {
		t.Fatalf("conversation = %q, want thread:777001", conv)
	}
	if ref.ThreadID != "777001" || ref.ChannelID != "333" || ref.MessageID != "9001" {
		t.Fatalf("reply reference = %+v, want parent 333 thread 777001 message 9001", ref)
	}
	if owner, ok := ensuredOwner(sessions, "thread:777001"); !ok || owner != "111" {
		t.Fatalf("thread session owner = %q, ok = %v; want 111", owner, ok)
	}
	creates := 0
	stub.mu.Lock()
	for _, call := range stub.calls {
		if call.method != http.MethodPost || !strings.HasSuffix(call.path, "/threads") {
			continue
		}
		creates++
		var body map[string]any
		_ = json.Unmarshal(call.body, &body)
		if body["name"] == "" {
			t.Error("thread create carries no name")
		}
		if duration, _ := body["auto_archive_duration"].(float64); int(duration) != defaultThreadAutoArchiveMinutes {
			t.Errorf("auto_archive_duration = %v, want %d", duration, defaultThreadAutoArchiveMinutes)
		}
	}
	stub.mu.Unlock()
	if creates != 1 {
		t.Fatalf("thread creates = %d, want 1", creates)
	}
	sink.mu.Lock()
	env := sink.envelopes["message:9001"]
	sink.mu.Unlock()
	receipt, err := adapter.Deliver(t.Context(), deliveryForEnvelope(t, "turn-9001", env, "reply inside thread"))
	if err != nil {
		t.Fatalf("Deliver(): %v", err)
	}
	if receipt.ExternalID == "" {
		t.Fatal("empty delivery receipt")
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	sends := 0
	for _, call := range stub.calls {
		if call.method == http.MethodPost && call.path == "/channels/777001/messages" {
			sends++
			var payload map[string]any
			_ = json.Unmarshal(call.body, &payload)
			if _, ok := payload["message_reference"]; ok {
				t.Error("thread send carries a reply reference")
			}
		}
		if call.method == http.MethodPost && call.path == "/channels/333/messages" {
			t.Error("fallback channel send on healthy thread")
		}
	}
	if sends == 0 {
		t.Fatal("no in-thread send captured")
	}
}

func TestThreads_InThreadNeedsNoMention(t *testing.T) {
	sink := newMemorySink()
	sessions := &fakeSessionEnsurer{}
	stub := threadCreateStub(t, "777002")
	adapter := newThreadAdapter(t, sink, sessions, stub)

	if _, err := adapter.admit(t.Context(), threadTrigger("9011", "open a thread", []string{"999"})); err != nil {
		t.Fatalf("trigger admit(): %v", err)
	}
	before := stub.postCount()
	admitted, err := adapter.admit(t.Context(), threadFollowUp("9012", "777002", "follow up without mention"))
	if err != nil {
		t.Fatalf("in-thread admit(): %v", err)
	}
	if !admitted {
		t.Fatal("in-thread admit() = false, want admission without mention")
	}
	conv, ref := envelopeThreadID(t, sink, "message:9012")
	if conv != "thread:777002" {
		t.Fatalf("conversation = %q, want thread:777002", conv)
	}
	if ref.ThreadID != "777002" {
		t.Fatalf("thread id = %q, want 777002", ref.ThreadID)
	}
	stub.mu.Lock()
	creates := 0
	for _, call := range stub.calls {
		if call.method == http.MethodPost && strings.HasSuffix(call.path, "/threads") {
			creates++
		}
	}
	stub.mu.Unlock()
	if creates != 1 {
		t.Fatalf("thread creates = %d, want 1 (follow-up creates none)", creates)
	}
	if got := stub.postCount(); got != before {
		t.Fatalf("message posts changed across in-thread intake: %d vs %d", got, before)
	}
}

func TestThreads_DuplicateTriggerCreatesOneThread(t *testing.T) {
	sink := newMemorySink()
	sessions := &fakeSessionEnsurer{}
	stub := threadCreateStub(t, "777003")
	adapter := newThreadAdapter(t, sink, sessions, stub)

	msg := func() *messagePayload { return threadTrigger("9021", "duplicate trigger", []string{"999"}) }
	for range 2 {
		admitted, err := adapter.admit(t.Context(), msg())
		if err != nil {
			t.Fatalf("admit(): %v", err)
		}
		if !admitted {
			t.Fatal("admit() = false, want duplicate to reach the sink")
		}
	}
	stub.mu.Lock()
	creates := 0
	for _, call := range stub.calls {
		if call.method == http.MethodPost && strings.HasSuffix(call.path, "/threads") {
			creates++
		}
	}
	stub.mu.Unlock()
	if creates != 1 {
		t.Fatalf("thread creates = %d, want 1", creates)
	}
	if calls, turns := sink.stats("message:9021"); calls != 2 || turns != 1 {
		t.Fatalf("calls = %d, turns = %d; want 2 calls and 1 turn", calls, turns)
	}
}

func TestThreads_CreateFailureFallsBackToChannel(t *testing.T) {
	sink := newMemorySink()
	sessions := &fakeSessionEnsurer{}
	stub := newRestStub(t, func(call int, w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/threads") {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		writeMessage(t, w, "m-parent-1")
	})
	adapter := newThreadAdapter(t, sink, sessions, stub)

	admitted, err := adapter.admit(t.Context(), threadTrigger("9031", "no permission trigger", []string{"999"}))
	if err != nil {
		t.Fatalf("admit(): %v", err)
	}
	if !admitted {
		t.Fatal("admit() = false, want channel fallback admission")
	}
	conv, ref := envelopeThreadID(t, sink, "message:9031")
	if conv != "channel:333" {
		t.Fatalf("conversation = %q, want channel:333 fallback", conv)
	}
	if ref.ThreadID != "" {
		t.Fatalf("thread id = %q, want empty on fallback", ref.ThreadID)
	}
	sink.mu.Lock()
	env := sink.envelopes["message:9031"]
	sink.mu.Unlock()
	if _, err := adapter.Deliver(t.Context(), deliveryForEnvelope(t, "turn-9031", env, "parent reply")); err != nil {
		t.Fatalf("Deliver(): %v", err)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	found := false
	for _, call := range stub.calls {
		if call.method == http.MethodPost && call.path == "/channels/333/messages" {
			found = true
		}
	}
	if !found {
		t.Fatal("no parent channel send on create failure")
	}
}

func TestThreads_ArchivedThreadSendFallsBackToParent(t *testing.T) {
	sink := newMemorySink()
	sessions := &fakeSessionEnsurer{}
	stub := newRestStub(t, func(call int, w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/threads") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"777004","parent_id":"333","owner_id":"999","type":11}`))
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/channels/777004/messages" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		writeMessage(t, w, "m-fallback-1")
	})
	adapter := newThreadAdapter(t, sink, sessions, stub)

	if _, err := adapter.admit(t.Context(), threadTrigger("9041", "archived thread trigger", []string{"999"})); err != nil {
		t.Fatalf("admit(): %v", err)
	}
	sink.mu.Lock()
	env := sink.envelopes["message:9041"]
	sink.mu.Unlock()
	if env == nil {
		t.Fatal("no envelope for trigger")
	}
	receipt, err := adapter.Deliver(t.Context(), deliveryForEnvelope(t, "turn-9041", env, "fallback body"))
	if err != nil {
		t.Fatalf("Deliver(): %v, want fallback success", err)
	}
	if receipt.ExternalID != "m-fallback-1" {
		t.Fatalf("receipt = %+v, want parent fallback message", receipt)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	threadAttempts := 0
	parentSends := 0
	for _, call := range stub.calls {
		if call.method == http.MethodPost && call.path == "/channels/777004/messages" {
			threadAttempts++
		}
		if call.method == http.MethodPost && call.path == "/channels/333/messages" {
			parentSends++
		}
	}
	if threadAttempts == 0 {
		t.Fatal("no thread send attempt captured")
	}
	if parentSends == 0 {
		t.Fatal("no parent fallback send captured")
	}
}

func TestThreads_TwoThreadsScopeMemorySessions(t *testing.T) {
	sink := newMemorySink()
	sessions := &fakeSessionEnsurer{}
	var seq atomic.Int64
	stub := newRestStub(t, func(call int, w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/threads") {
			n := seq.Add(1)
			id := "77701" + string(rune('0'+n))
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"` + id + `","parent_id":"333","owner_id":"999","type":11}`))
			return
		}
		writeMessage(t, w, "m")
	})
	adapter := newThreadAdapter(t, sink, sessions, stub)

	if _, err := adapter.admit(t.Context(), threadTrigger("9051", "first topic", []string{"999"})); err != nil {
		t.Fatalf("first admit(): %v", err)
	}
	if _, err := adapter.admit(t.Context(), threadTrigger("9052", "second topic", []string{"999"})); err != nil {
		t.Fatalf("second admit(): %v", err)
	}
	firstConv, firstRef := envelopeThreadID(t, sink, "message:9051")
	secondConv, secondRef := envelopeThreadID(t, sink, "message:9052")
	if firstConv == secondConv {
		t.Fatalf("thread sessions collide: %q", firstConv)
	}
	if !strings.HasPrefix(firstConv, "thread:") || !strings.HasPrefix(secondConv, "thread:") {
		t.Fatalf("conversations = %q, %q; want thread: sessions", firstConv, secondConv)
	}
	if firstRef.ChannelID != "333" || secondRef.ChannelID != "333" {
		t.Fatalf("parents = %q, %q; want shared parent 333", firstRef.ChannelID, secondRef.ChannelID)
	}
	if firstRef.ThreadID == "" || secondRef.ThreadID == "" || firstRef.ThreadID == secondRef.ThreadID {
		t.Fatalf("thread ids = %q, %q; want distinct", firstRef.ThreadID, secondRef.ThreadID)
	}
}

type fakeThreadDocStore struct {
	mu   sync.Mutex
	docs []memory.StoredDocument
}

func (s *fakeThreadDocStore) UpsertDocument(_ context.Context, doc *memory.StoredDocument) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.docs = append(s.docs, *doc)
	return true, nil
}

func (s *fakeThreadDocStore) ProjectionWatermark(_ context.Context, sessionID string) (mark int64, found bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.docs {
		d := s.docs[i]
		if d.SessionID == sessionID {
			if !found || d.ToSequence > mark {
				mark = d.ToSequence
				found = true
			}
		}
	}
	return mark, found, nil
}

func (s *fakeThreadDocStore) Search(_ context.Context, query *memory.StoredQuery) (hits []memory.StoredHit, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.docs {
		d := s.docs[i]
		if d.OwnerID != query.OwnerID {
			continue
		}
		if query.SessionID != "" && d.SessionID != query.SessionID {
			continue
		}
		matched := true
		for _, term := range strings.Fields(query.Terms) {
			if !strings.Contains(strings.ToLower(d.Content), strings.ToLower(term)) {
				matched = false
				break
			}
		}
		if matched {
			hits = append(hits, memory.StoredHit{StoredDocument: d})
		}
	}
	return hits, nil
}

func (s *fakeThreadDocStore) DeleteSessionDocuments(_ context.Context, sessionID string) error {
	return nil
}

func (s *fakeThreadDocStore) DeleteExpiredSummaries(_ context.Context, now time.Time, limit int) (int, error) {
	return 0, nil
}

func (s *fakeThreadDocStore) DeleteSessionSummaries(_ context.Context, ownerID, sessionID string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.docs[:0]
	deleted := 0
	for i := range s.docs {
		d := s.docs[i]
		if d.OwnerID == ownerID && d.SessionID == sessionID && d.Kind == memory.KindSummary {
			deleted++
			continue
		}
		kept = append(kept, d)
	}
	s.docs = kept
	return deleted, nil
}

func (s *fakeThreadDocStore) DeleteOtherSummaries(_ context.Context, ownerID, sessionID, keepID string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.docs[:0]
	deleted := 0
	for i := range s.docs {
		d := s.docs[i]
		if d.OwnerID == ownerID && d.SessionID == sessionID && d.Kind == memory.KindSummary && d.ID != keepID {
			deleted++
			continue
		}
		kept = append(kept, d)
	}
	s.docs = kept
	return deleted, nil
}

func (s *fakeThreadDocStore) ListSessionDocumentsByKind(_ context.Context, ownerID, sessionID, kind string, limit int) ([]memory.StoredDocument, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []memory.StoredDocument
	for i := range s.docs {
		d := s.docs[i]
		if d.OwnerID != ownerID || d.SessionID != sessionID || d.Kind != kind {
			continue
		}
		out = append(out, d)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (s *fakeThreadDocStore) GetDocument(_ context.Context, id string) (memory.StoredDocument, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.docs {
		if s.docs[i].ID == id {
			return s.docs[i], true, nil
		}
	}
	return memory.StoredDocument{}, false, nil
}

func (s *fakeThreadDocStore) ListSessionDocuments(_ context.Context, ownerID, sessionID string, limit int) ([]memory.StoredDocument, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []memory.StoredDocument
	for i := range s.docs {
		d := s.docs[i]
		if d.OwnerID != ownerID || d.SessionID != sessionID {
			continue
		}
		out = append(out, d)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (s *fakeThreadDocStore) ListOwnerDocuments(_ context.Context, ownerID string, limit int) ([]memory.StoredDocument, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []memory.StoredDocument
	for i := range s.docs {
		d := s.docs[i]
		if d.OwnerID != ownerID {
			continue
		}
		out = append(out, d)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (s *fakeThreadDocStore) CountSessionDocuments(_ context.Context, sessionID string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for i := range s.docs {
		if s.docs[i].SessionID == sessionID {
			n++
		}
	}
	return n, nil
}

func (s *fakeThreadDocStore) DeleteDocumentsByIDsOwned(_ context.Context, ownerID string, ids []string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	wanted := make(map[string]bool, len(ids))
	for _, id := range ids {
		wanted[id] = true
	}
	kept := s.docs[:0]
	deleted := 0
	for i := range s.docs {
		d := s.docs[i]
		if d.OwnerID == ownerID && wanted[d.ID] {
			deleted++
			continue
		}
		kept = append(kept, d)
	}
	s.docs = kept
	return deleted, nil
}

func TestThreads_TwoThreadsCrossThreadRecallIsolation(t *testing.T) {
	sink := newMemorySink()
	sessions := &fakeSessionEnsurer{}
	var seq atomic.Int64
	stub := newRestStub(t, func(call int, w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/threads") {
			n := seq.Add(1)
			id := "77701" + string(rune('0'+n))
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"` + id + `","parent_id":"333","owner_id":"999","type":11}`))
			return
		}
		writeMessage(t, w, "m")
	})
	adapter := newThreadAdapter(t, sink, sessions, stub)

	if _, err := adapter.admit(t.Context(), threadTrigger("9101", "alpha topic", []string{"999"})); err != nil {
		t.Fatalf("first admit(): %v", err)
	}
	if _, err := adapter.admit(t.Context(), threadTrigger("9102", "beta topic", []string{"999"})); err != nil {
		t.Fatalf("second admit(): %v", err)
	}
	firstConv, _ := envelopeThreadID(t, sink, "message:9101")
	secondConv, _ := envelopeThreadID(t, sink, "message:9102")
	if firstConv == secondConv {
		t.Fatalf("thread sessions collide: %q", firstConv)
	}

	store := &fakeThreadDocStore{}
	svc, err := memory.NewService(store, nil, &memory.Config{MaxDocuments: 2})
	if err != nil {
		t.Fatalf("NewService(): %v", err)
	}

	firstDocs := []memory.StoredDocument{
		{ID: "doc-a1", OwnerID: "111", SessionID: firstConv, Kind: "event.text", FromSequence: 1, ToSequence: 1, Content: "alpha project secret", CreatedAt: time.Now().UTC()},
		{ID: "doc-a2", OwnerID: "111", SessionID: firstConv, Kind: "event.text", FromSequence: 2, ToSequence: 2, Content: "alpha project notes", CreatedAt: time.Now().UTC()},
	}
	secondDocs := []memory.StoredDocument{
		{ID: "doc-b1", OwnerID: "111", SessionID: secondConv, Kind: "event.text", FromSequence: 1, ToSequence: 1, Content: "beta project secret", CreatedAt: time.Now().UTC()},
		{ID: "doc-b2", OwnerID: "111", SessionID: secondConv, Kind: "event.text", FromSequence: 2, ToSequence: 2, Content: "beta project notes", CreatedAt: time.Now().UTC()},
	}
	for i := range firstDocs {
		if _, err := store.UpsertDocument(t.Context(), &firstDocs[i]); err != nil {
			t.Fatalf("seed first doc: %v", err)
		}
	}
	for i := range secondDocs {
		if _, err := store.UpsertDocument(t.Context(), &secondDocs[i]); err != nil {
			t.Fatalf("seed second doc: %v", err)
		}
	}

	firstResults, err := svc.Recall(t.Context(), &memory.RecallRequest{OwnerID: "111", SessionID: firstConv, Query: "project"})
	if err != nil {
		t.Fatalf("first recall: %v", err)
	}
	if len(firstResults) != 2 {
		t.Fatalf("first recall = %d docs, want 2", len(firstResults))
	}
	for _, doc := range firstResults {
		if doc.SessionID != firstConv {
			t.Errorf("first recall returned doc from session %q, want %q", doc.SessionID, firstConv)
		}
	}

	secondResults, err := svc.Recall(t.Context(), &memory.RecallRequest{OwnerID: "111", SessionID: secondConv, Query: "project"})
	if err != nil {
		t.Fatalf("second recall: %v", err)
	}
	if len(secondResults) != 2 {
		t.Fatalf("second recall = %d docs, want 2", len(secondResults))
	}
	for _, doc := range secondResults {
		if doc.SessionID != secondConv {
			t.Errorf("second recall returned doc from session %q, want %q", doc.SessionID, secondConv)
		}
	}

	for _, doc := range firstResults {
		if doc.ID == "doc-b1" || doc.ID == "doc-b2" {
			t.Errorf("first recall leaked beta doc %q", doc.ID)
		}
	}
	for _, doc := range secondResults {
		if doc.ID == "doc-a1" || doc.ID == "doc-a2" {
			t.Errorf("second recall leaked alpha doc %q", doc.ID)
		}
	}
}

func TestThreads_BotOwnThreadMessageDropped(t *testing.T) {
	sink := newMemorySink()
	sessions := &fakeSessionEnsurer{}
	stub := threadCreateStub(t, "777005")
	adapter := newThreadAdapter(t, sink, sessions, stub)

	if _, err := adapter.admit(t.Context(), threadTrigger("9061", "open", []string{"999"})); err != nil {
		t.Fatalf("trigger admit(): %v", err)
	}
	botMsg := threadFollowUp("9062", "777005", "bot echo")
	botMsg.Author = userPayload{ID: "999"}
	admitted, err := adapter.admit(t.Context(), botMsg)
	if err != nil {
		t.Fatalf("admit(): %v", err)
	}
	if admitted {
		t.Fatal("admit() = true for bot own thread message, want drop")
	}
	systemMsg := threadFollowUp("9063", "777005", "thread created")
	systemMsg.Type = 21
	admitted, err = adapter.admit(t.Context(), systemMsg)
	if err != nil {
		t.Fatalf("admit(): %v", err)
	}
	if admitted {
		t.Fatal("admit() = true for system thread message, want drop")
	}
}

func TestThreads_ForumMentionSkipsThreadCreate(t *testing.T) {
	sink := newMemorySink()
	sessions := &fakeSessionEnsurer{}
	stub := threadCreateStub(t, "777006")
	adapter := newThreadAdapter(t, sink, sessions, stub)

	forumType := channelTypeGuildForum
	msg := threadTrigger("9071", "forum mention", []string{"999"})
	msg.ChannelType = &forumType
	admitted, err := adapter.admit(t.Context(), msg)
	if err != nil {
		t.Fatalf("admit(): %v", err)
	}
	if !admitted {
		t.Fatal("admit() = false, want channel admission for forum")
	}
	conv, ref := envelopeThreadID(t, sink, "message:9071")
	if conv != "channel:333" {
		t.Fatalf("conversation = %q, want channel:333 for forum", conv)
	}
	if ref.ThreadID != "" {
		t.Fatalf("thread id = %q, want empty for forum", ref.ThreadID)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	for _, call := range stub.calls {
		if call.method == http.MethodPost && strings.HasSuffix(call.path, "/threads") {
			t.Fatal("thread create attempted for forum channel")
		}
	}
}

func TestThreadName_TruncatesAndDefaults(t *testing.T) {
	long := strings.Repeat("w", threadNameMaxRunes+50) + " tail"
	if got := threadName(long); len([]rune(got)) != threadNameMaxRunes {
		t.Fatalf("thread name runes = %d, want %d", len([]rune(got)), threadNameMaxRunes)
	}
	if got := threadName("  hello   world \n next "); got != "hello world next" {
		t.Fatalf("thread name = %q, want collapsed spacing", got)
	}
	if got := threadName("   "); got == "" {
		t.Fatal("empty trigger produced empty thread name")
	}
}
