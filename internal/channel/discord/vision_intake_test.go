package discord

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/vision"
)

type countingMediaStub struct {
	server *httptest.Server
	hits   int
	mu     sync.Mutex
}

func newCountingStub(t *testing.T, body []byte, contentType string) *countingMediaStub {
	t.Helper()
	stub := &countingMediaStub{}
	stub.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stub.mu.Lock()
		stub.hits++
		stub.mu.Unlock()
		w.Header().Set("Content-Type", contentType)
		_, _ = w.Write(body)
	}))
	t.Cleanup(stub.server.Close)
	return stub
}

func (s *countingMediaStub) hitCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hits
}

type memoryVisionStore struct {
	mu   sync.Mutex
	refs map[string][]byte
	puts int
}

func newMemoryVisionStore() *memoryVisionStore {
	return &memoryVisionStore{refs: map[string][]byte{}}
}

func (m *memoryVisionStore) Put(ctx context.Context, r io.Reader, meta *vision.ArtifactMetadata) (vision.ArtifactRef, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return vision.ArtifactRef{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refs[meta.ID] = raw
	m.puts++
	return vision.ArtifactRef{ID: meta.ID, BlobDigest: "sha256:abc", SizeBytes: int64(len(raw))}, nil
}

func (m *memoryVisionStore) Unlink(ctx context.Context, refID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.refs, refID)
	return nil
}

func testPNGBytes(t *testing.T) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 8, 8))
	for y := range 8 {
		for x := range 8 {
			img.Set(x, y, color.NRGBA{R: uint8(x * 32), G: uint8(y * 32), B: 128, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func testVisionService(t *testing.T, stores *memoryVisionStore) *vision.Service {
	t.Helper()
	limits := vision.Limits{
		MaxEncodedBytes:      20971520,
		MaxPixels:            40000000,
		MaxDimension:         8192,
		DecodeTimeout:        5 * time.Second,
		MaxDecodeConcurrency: 4,
		MaxTransformMemory:   268435456,
		StripMetadata:        true,
		OrientNormalize:      true,
		TransformVersion:     "v1",
	}
	svc, err := vision.NewService(&limits, vision.WithStore(stores))
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

func TestDeniedIdentityTriggersNoFetch(t *testing.T) {
	raw := testPNGBytes(t)
	stub := newCountingStub(t, raw, "image/png")
	gateway := newFakeGateway(t, true)
	ws, sink, tc := startMediaSession(t, gateway)
	defer tc.stop(t)

	denied := &messagePayload{
		ID:        "3901",
		ChannelID: "333",
		GuildID:   "222",
		Author:    userPayload{ID: "666"},
		Content:   "look",
		Mentions:  []userPayload{{ID: "999"}},
		Attachments: []attachmentPayload{{
			URL:         stub.server.URL + "/cat.png",
			Filename:    "cat.png",
			Size:        int64(len(raw)),
			ContentType: "image/png",
		}},
	}
	gateway.send(ws, opDispatch, denied, seqPtr(11), eventMessageCreate)
	time.Sleep(300 * time.Millisecond)
	if calls, _ := sink.stats("message:3901"); calls != 0 {
		t.Errorf("denied message admitted, want dropped")
	}
	if got := stub.hitCount(); got != 0 {
		t.Errorf("media hits = %d, want 0 (no fetch on denial)", got)
	}
	tc.doubles.media.mu.Lock()
	puts := tc.doubles.media.puts
	tc.doubles.media.mu.Unlock()
	if puts != 0 {
		t.Errorf("media puts = %d, want 0", puts)
	}
}

func TestVisionImageAdmittedAsImagePart(t *testing.T) {
	raw := testPNGBytes(t)
	stub := newCountingStub(t, raw, "image/png")
	gateway := newFakeGateway(t, true)
	ws, sink, tc := startMediaSession(t, gateway)
	defer tc.stop(t)

	stores := newMemoryVisionStore()
	tc.adapter.SetVisionService(testVisionService(t, stores))

	msg := messageWithAttachments("3902", "what is this", []attachmentPayload{{
		URL:         stub.server.URL + "/cat.png",
		Filename:    "cat.png",
		Size:        int64(len(raw)),
		ContentType: "image/png",
	}})
	gateway.send(ws, opDispatch, msg, seqPtr(12), eventMessageCreate)
	waitFor(t, 5*time.Second, func() bool {
		calls, _ := sink.stats("message:3902")
		return calls == 1
	}, "vision message 3902")

	env := admittedEnvelope(t, sink, "3902")
	if len(env.Parts) != 2 {
		t.Fatalf("parts = %d, want 2 (text + image)", len(env.Parts))
	}
	if env.Parts[0].Text != "what is this" {
		t.Errorf("text part = %q", env.Parts[0].Text)
	}
	if len(env.Parts[1].Image) == 0 {
		t.Fatal("image part is empty")
	}
	part, err := vision.UnmarshalPart(env.Parts[1].Image)
	if err != nil {
		t.Fatalf("UnmarshalPart: %v", err)
	}
	if part.Kind != vision.PartKindImageRef || part.Trust != vision.TrustUntrustedExternal {
		t.Errorf("part kind/trust = %+v", part)
	}
	if stores.puts != 2 {
		t.Errorf("vision puts = %d, want 2 (source+derived)", stores.puts)
	}
}

func TestVisionHostileImageDropped(t *testing.T) {
	stub := newCountingStub(t, []byte("fake-png-bytes"), "image/png")
	gateway := newFakeGateway(t, true)
	ws, sink, tc := startMediaSession(t, gateway)
	defer tc.stop(t)

	stores := newMemoryVisionStore()
	tc.adapter.SetVisionService(testVisionService(t, stores))

	msg := messageWithAttachments("3903", "look", []attachmentPayload{{
		URL:         stub.server.URL + "/evil.png",
		Filename:    "evil.png",
		Size:        14,
		ContentType: "image/png",
	}})
	gateway.send(ws, opDispatch, msg, seqPtr(13), eventMessageCreate)
	time.Sleep(300 * time.Millisecond)
	if calls, _ := sink.stats("message:3903"); calls != 0 {
		t.Errorf("hostile image admitted, want dropped")
	}
	stores.mu.Lock()
	puts := stores.puts
	stores.mu.Unlock()
	if puts != 0 {
		t.Errorf("vision puts = %d, want 0 (rejected before store)", puts)
	}
}
