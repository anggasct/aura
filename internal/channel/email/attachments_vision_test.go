package email

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/vision"
)

type visionWrite struct {
	meta    vision.ArtifactMetadata
	content []byte
}

type recordingVisionStore struct {
	mu     sync.Mutex
	writes []visionWrite
	fail   error
}

func (s *recordingVisionStore) Put(_ context.Context, r io.Reader, meta *vision.ArtifactMetadata) (vision.ArtifactRef, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return vision.ArtifactRef{}, s.fail
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return vision.ArtifactRef{}, err
	}
	s.writes = append(s.writes, visionWrite{meta: *meta, content: data})
	return vision.ArtifactRef{ID: meta.ID, BlobDigest: fmt.Sprintf("%064d", len(s.writes)), SizeBytes: int64(len(data))}, nil
}

func (s *recordingVisionStore) Unlink(context.Context, string) error { return nil }

func (s *recordingVisionStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.writes)
}

func newVisionStub(t *testing.T, store vision.ArtifactWriter) *vision.Service {
	t.Helper()
	service, err := vision.NewService(&vision.Limits{
		MaxEncodedBytes: 1 << 20, MaxPixels: 1 << 20, MaxDimension: 1024, DecodeTimeout: 5 * time.Second,
		MaxDecodeConcurrency: 2, MaxTransformMemory: 1 << 26, StripMetadata: true, OrientNormalize: true,
		TransformVersion: "v1", Now: time.Now,
	}, vision.WithStore(store))
	if err != nil {
		t.Fatalf("vision.NewService: %v", err)
	}
	return service
}

func pngFixture(t *testing.T) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 4, 4))
	for y := range 4 {
		for x := range 4 {
			img.Set(x, y, color.NRGBA{R: uint8(x * 40), G: uint8(y * 40), B: 90, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestAttachments_VisionService(t *testing.T) {
	photo := pngFixture(t)
	cases := []struct {
		name        string
		attachments []testAttachment
		visionFail  error
		wantDrop    string
		wantImages  int
		wantVision  int
		wantPlain   []string
	}{
		{name: "an image goes through the vision intake", attachments: []testAttachment{{filename: "photo.png", mediaType: "image/png", content: photo}}, wantImages: 1, wantVision: 2},
		{name: "a pdf still takes the plain artifact path", attachments: []testAttachment{{filename: "a.pdf", mediaType: "application/pdf", content: []byte("%PDF-1.4")}}, wantPlain: []string{"a.pdf"}},
		{name: "an image and a pdf each take their own path", attachments: []testAttachment{{filename: "photo.png", mediaType: "image/png", content: photo}, {filename: "a.pdf", mediaType: "application/pdf", content: []byte("%PDF-1.4")}}, wantImages: 1, wantVision: 2, wantPlain: []string{"a.pdf"}},
		{name: "an intake failure drops the mail", attachments: []testAttachment{{filename: "photo.png", mediaType: "image/png", content: photo}}, visionFail: errBoom, wantDrop: "attachment_failed"},
		{name: "bytes that are not an image drop the mail", attachments: []testAttachment{{filename: "fake.png", mediaType: "image/png", content: []byte("not a png at all")}}, wantDrop: "attachment_failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			visionStore := &recordingVisionStore{fail: tc.visionFail}
			plain := &recordingMedia{}
			h := newHarness(t, harnessOptions{media: plain, vision: newVisionStub(t, visionStore)})
			h.start()
			h.deliver(mailWithAttachments(mailSpec{from: friendAddress, subject: "Docs", messageID: "vis@example.com"}, "look", tc.attachments...))
			if tc.wantDrop != "" {
				h.wantTurns(0)
				h.wantDrops(tc.wantDrop)
				return
			}
			h.wantTurns(1)
			env := h.sink.all()[0]
			var images [][]byte
			for _, part := range env.Parts {
				if len(part.Image) != 0 {
					images = append(images, part.Image)
				}
			}
			if len(images) != tc.wantImages {
				t.Fatalf("image parts = %d, want %d", len(images), tc.wantImages)
			}
			for _, raw := range images {
				part, err := vision.UnmarshalPart(raw)
				if err != nil {
					t.Fatalf("UnmarshalPart: %v", err)
				}
				if part.Provenance.Source != "email:"+testAccount || part.Provenance.ExternalID != "message:vis@example.com" || part.Provenance.TurnID != "vis@example.com" || part.Provenance.SessionID != env.ConversationID {
					t.Errorf("provenance = %+v, want the email source, message and session", part.Provenance)
				}
			}
			if got := visionStore.count(); got != tc.wantVision {
				t.Errorf("vision artifacts = %d, want %d", got, tc.wantVision)
			}
			var names []string
			for _, item := range plain.stored() {
				names = append(names, item.meta.Filename)
			}
			if strings.Join(names, ",") != strings.Join(tc.wantPlain, ",") {
				t.Errorf("plain artifacts = %v, want %v", names, tc.wantPlain)
			}
		})
	}
}
