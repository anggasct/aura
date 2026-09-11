package discord

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/websocket"

	runtimeingress "github.com/anggasct/aura/internal/runtime/ingress"
)

func newMediaStub(t *testing.T, body, contentType string, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		w.Header().Set("Content-Type", contentType)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func messageWithAttachments(id, content string, attachments []attachmentPayload) *messagePayload {
	return &messagePayload{
		ID:          id,
		ChannelID:   "333",
		GuildID:     "222",
		Author:      userPayload{ID: "111"},
		Content:     content,
		Mentions:    []userPayload{{ID: "999"}},
		Attachments: attachments,
	}
}

func imageAttachment(srv *httptest.Server, size int64) attachmentPayload {
	return attachmentPayload{
		URL:         srv.URL + "/cat.png",
		Filename:    "cat.png",
		Size:        size,
		ContentType: "image/png",
	}
}

func sendAndWait(t *testing.T, gateway *fakeGateway, ws *websocket.Conn, msg *messagePayload, seq int64, sink *memorySink) {
	t.Helper()
	gateway.send(ws, opDispatch, msg, seqPtr(seq), eventMessageCreate)
	waitFor(t, 5*time.Second, func() bool {
		calls, _ := sink.stats("message:" + msg.ID)
		return calls == 1
	}, "message "+msg.ID)
}

func admittedEnvelope(t *testing.T, sink *memorySink, id string) *runtimeingress.IngressEnvelope {
	t.Helper()
	sink.mu.Lock()
	defer sink.mu.Unlock()
	env, ok := sink.envelopes["message:"+id]
	if !ok {
		t.Fatalf("message %s has no admitted envelope", id)
	}
	return env
}

func startMediaSession(t *testing.T, gateway *fakeGateway) (*websocket.Conn, *memorySink, *testContext) {
	t.Helper()
	sink := newMemorySink()
	tc := startTestAdapter(t, gateway, sink, &memoryResumeStore{}, &logCapture{})
	ws := gateway.nextConn(5 * time.Second)
	gateway.send(ws, opHello, helloData(30), nil, "")
	gateway.nextFrame(5 * time.Second)
	gateway.send(ws, opDispatch, readyData("session-a", "999"), seqPtr(0), eventReady)
	return ws, sink, tc
}

func TestAdapter_AdmitsMessageWithStoredAttachment(t *testing.T) {
	gateway := newFakeGateway(t, true)
	media := newMediaStub(t, "fake-png-bytes", "image/png", http.StatusOK)
	ws, sink, tc := startMediaSession(t, gateway)
	defer tc.stop(t)

	sendAndWait(t, gateway, ws, messageWithAttachments("3001", "look", []attachmentPayload{imageAttachment(media, 15)}), 1, sink)

	var reference replyReference
	if err := json.Unmarshal(admittedEnvelope(t, sink, "3001").ReplyContext, &reference); err != nil {
		t.Fatalf("reply context: %v", err)
	}
	if len(reference.Artifacts) != 1 {
		t.Fatalf("artifacts = %+v, want 1", reference.Artifacts)
	}
	got := reference.Artifacts[0]
	if got.Digest == "" || got.RefID == "" || got.SizeBytes != 14 || got.Filename != "cat.png" {
		t.Errorf("artifact = %+v", got)
	}
	tc.doubles.sessions.mu.Lock()
	owner := tc.doubles.sessions.sessions["channel:333"]
	tc.doubles.sessions.mu.Unlock()
	if owner != "111" {
		t.Errorf("ensured session owner = %q, want principal", owner)
	}
	if tc.doubles.media.puts != 1 {
		t.Errorf("media puts = %d, want 1", tc.doubles.media.puts)
	}
	if strings.Contains(tc.logs.String(), testToken) {
		t.Error("bot token leaked into logs")
	}
}

func TestAdapter_DropsMessageWithBadMedia(t *testing.T) {
	media := newMediaStub(t, "fake-png-bytes", "image/png", http.StatusOK)
	html := newMediaStub(t, "<html></html>", "text/html", http.StatusOK)
	broken := newMediaStub(t, "", "image/png", http.StatusInternalServerError)
	cases := map[string][]attachmentPayload{
		"oversize declared": {
			{URL: media.URL + "/big.png", Filename: "big.png", Size: 1 << 30, ContentType: "image/png"},
		},
		"ineligible type": {
			{URL: media.URL + "/page", Filename: "page.html", Size: 10, ContentType: "text/html"},
		},
		"spoofed content type": {
			{URL: html.URL + "/spoof.png", Filename: "spoof.png", Size: 10, ContentType: "image/png"},
		},
		"failed download": {
			{URL: broken.URL + "/gone", Filename: "gone.png", Size: 10, ContentType: "image/png"},
		},
		"non-https url": {
			{URL: "http://example.com/cat.png", Filename: "cat.png", Size: 10, ContentType: "image/png"},
		},
	}
	for name, attachments := range cases {
		t.Run(name, func(t *testing.T) {
			gateway := newFakeGateway(t, true)
			ws, sink, tc := startMediaSession(t, gateway)
			defer tc.stop(t)

			msg := messageWithAttachments("3100", "look", attachments)
			gateway.send(ws, opDispatch, msg, seqPtr(1), eventMessageCreate)
			time.Sleep(200 * time.Millisecond)
			if calls, _ := sink.stats("message:3100"); calls != 0 {
				t.Errorf("calls = %d, want no admission", calls)
			}
			if tc.doubles.media.puts != 0 {
				t.Errorf("media puts = %d, want none", tc.doubles.media.puts)
			}
		})
	}
}

func TestAdapter_DuplicateMediaAdmitsBothTurns(t *testing.T) {
	gateway := newFakeGateway(t, true)
	media := newMediaStub(t, "same-bytes", "image/png", http.StatusOK)
	ws, sink, tc := startMediaSession(t, gateway)
	defer tc.stop(t)

	sendAndWait(t, gateway, ws, messageWithAttachments("3201", "one", []attachmentPayload{imageAttachment(media, 10)}), 1, sink)
	sendAndWait(t, gateway, ws, messageWithAttachments("3202", "two", []attachmentPayload{imageAttachment(media, 10)}), 2, sink)

	first := admittedEnvelope(t, sink, "3201")
	second := admittedEnvelope(t, sink, "3202")
	var firstRef, secondRef replyReference
	_ = json.Unmarshal(first.ReplyContext, &firstRef)
	_ = json.Unmarshal(second.ReplyContext, &secondRef)
	if len(firstRef.Artifacts) != 1 || len(secondRef.Artifacts) != 1 {
		t.Fatalf("artifacts missing: %+v %+v", firstRef.Artifacts, secondRef.Artifacts)
	}
	if firstRef.Artifacts[0].Digest != secondRef.Artifacts[0].Digest {
		t.Error("duplicate content produced different digests")
	}
	if tc.doubles.media.puts != 2 {
		t.Errorf("media puts = %d, want 2", tc.doubles.media.puts)
	}
}

func TestSanitizeFilename(t *testing.T) {
	cases := map[string]string{
		"cat.png":                "cat.png",
		"../../etc/passwd":       "passwd",
		"":                       "attachment",
		"   ":                    "attachment",
		"a\x00b.png":             "ab.png",
		strings.Repeat("n", 200): strings.Repeat("n", 128),
	}
	for input, want := range cases {
		if got := sanitizeFilename(input); got != want {
			t.Errorf("sanitize(%q) = %q, want %q", input, got, want)
		}
	}
}
