package telegram

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	auraagent "github.com/anggasct/aura/internal/agent"
	"github.com/anggasct/aura/internal/channel/media"
	"github.com/anggasct/aura/internal/config"
	runtimeingress "github.com/anggasct/aura/internal/runtime/ingress"
	"github.com/anggasct/aura/internal/vision"
)

const (
	mediaUserID   = 770011
	smallMediaCap = 64
)

type storedArtifact struct {
	meta media.Meta
	body []byte
}

type fakeArtifacts struct {
	mu   sync.Mutex
	puts []storedArtifact
	err  error
}

func (s *fakeArtifacts) Put(_ context.Context, content io.Reader, meta *media.Meta) (media.Receipt, error) {
	body, err := io.ReadAll(content)
	if err != nil {
		return media.Receipt{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return media.Receipt{}, s.err
	}
	s.puts = append(s.puts, storedArtifact{meta: *meta, body: body})
	sum := sha256.Sum256(body)
	return media.Receipt{RefID: "ref-" + hex.EncodeToString(sum[:])[:12], Digest: hex.EncodeToString(sum[:]), SizeBytes: int64(len(body))}, nil
}

func (s *fakeArtifacts) stored() []storedArtifact {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.puts)
}

type visionStore struct {
	mu   sync.Mutex
	puts int
}

func (v *visionStore) Put(_ context.Context, r io.Reader, meta *vision.ArtifactMetadata) (vision.ArtifactRef, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return vision.ArtifactRef{}, err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.puts++
	return vision.ArtifactRef{ID: meta.ID, BlobDigest: "sha256:abc", SizeBytes: int64(len(raw))}, nil
}

func (v *visionStore) Unlink(context.Context, string) error {
	return nil
}

func (v *visionStore) count() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.puts
}

func testVisionService(t *testing.T, store *visionStore) *vision.Service {
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
	svc, err := vision.NewService(&limits, vision.WithStore(store))
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

func testJPEG(t *testing.T) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 8, 8))
	for y := range 8 {
		for x := range 8 {
			img.Set(x, y, color.NRGBA{R: uint8(x * 32), G: uint8(y * 32), B: 64, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

type ledgerFromSink struct {
	sink *memorySink
	mu   sync.Mutex
	asks int
	err  error
}

func (l *ledgerFromSink) Claimed(_ context.Context, _, externalID string) (bool, error) {
	l.mu.Lock()
	l.asks++
	failure := l.err
	l.mu.Unlock()
	if failure != nil {
		return false, failure
	}
	calls, _ := l.sink.stats(externalID)
	return calls > 0, nil
}

func (l *ledgerFromSink) asked() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.asks
}

type mediaRig struct {
	*accessRig
	artifacts *fakeArtifacts
	scratch   string
}

func newMediaRig(t *testing.T, opts accessOpts) *mediaRig {
	t.Helper()
	scratch := t.TempDir()
	t.Setenv("TMPDIR", scratch)
	rig := newGroupRig(t, opts)
	artifacts := &fakeArtifacts{}
	rig.adapter.SetMediaStore(artifacts)
	return &mediaRig{accessRig: rig, artifacts: artifacts, scratch: scratch}
}

func (r *mediaRig) leftovers(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(r.scratch)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func (r *mediaRig) assertNoLeak(t *testing.T, secrets ...string) {
	t.Helper()
	logs := r.h.logs.String()
	for _, secret := range append([]string{testToken}, secrets...) {
		if strings.Contains(logs, secret) {
			t.Errorf("logs carry %q", secret)
		}
	}
}

func privateMedia(updateID, userID int64, caption string, attach func(*message)) update {
	item := privateText(updateID, userID, "")
	item.Message.Caption = caption
	attach(item.Message)
	return item
}

func groupMedia(updateID, userID int64, caption string, attach func(*message)) update {
	item := groupMessage(updateID, userID, "")
	item.Message.Caption = caption
	attach(item.Message)
	return item
}

func asPhoto(sizes ...mediaFile) func(*message) {
	return func(m *message) { m.Photo = sizes }
}

func asDocument(file mediaFile) func(*message) {
	return func(m *message) { m.Document = &file }
}

func artifactsOf(t *testing.T, env *runtimeingress.IngressEnvelope) []media.ArtifactSummary {
	t.Helper()
	var reference replyReference
	if err := json.Unmarshal(env.ReplyContext, &reference); err != nil {
		t.Fatalf("reply context: %v", err)
	}
	return reference.Artifacts
}

func TestMediaKindsLandInTheArtifactStore(t *testing.T) {
	cases := []struct {
		name     string
		attach   func(*message)
		file     fakeFile
		wantType string
		wantName string
	}{
		{
			name:   "photo",
			attach: asPhoto(mediaFile{FileID: "ph-small", FileSize: 10, Width: 90, Height: 90}, mediaFile{FileID: "ph-large", FileSize: 20, Width: 800, Height: 600}),
			file:   fakeFile{id: "ph-large", path: "photos/file_501.jpg", declared: 20, contentType: "image/jpeg", body: []byte("jpeg-bytes-of-photo")},

			wantType: "image/jpeg",
			wantName: "photo.jpg",
		},
		{
			name:     "document",
			attach:   asDocument(mediaFile{FileID: "doc", FileSize: 12, MimeType: "application/pdf", FileName: "report.pdf"}),
			file:     fakeFile{id: "doc", path: "documents/file_502.pdf", declared: 12, contentType: "application/pdf", body: []byte("pdf-contents")},
			wantType: "application/pdf",
			wantName: "report.pdf",
		},
		{
			name:     "voice",
			attach:   func(m *message) { m.Voice = &mediaFile{FileID: "vo", FileSize: 9, MimeType: "audio/ogg"} },
			file:     fakeFile{id: "vo", path: "voice/file_503.oga", declared: 9, contentType: "audio/ogg", body: []byte("ogg-voice")},
			wantType: "audio/ogg",
			wantName: "voice.ogg",
		},
		{
			name:     "voice served as a generic binary",
			attach:   func(m *message) { m.Voice = &mediaFile{FileID: "vo", FileSize: 9, MimeType: "audio/ogg"} },
			file:     fakeFile{id: "vo", path: "voice/file_503.oga", declared: 9, contentType: "application/octet-stream", body: []byte("ogg-voice")},
			wantType: "audio/ogg",
			wantName: "voice.ogg",
		},
		{
			name: "video",
			attach: func(m *message) {
				m.Video = &mediaFile{FileID: "vi", FileSize: 9, MimeType: "video/mp4", FileName: "clip.mp4"}
			},
			file:     fakeFile{id: "vi", path: "videos/file_504.mp4", declared: 9, contentType: "video/mp4", body: []byte("mp4-video")},
			wantType: "video/mp4",
			wantName: "clip.mp4",
		},
		{
			name:     "video note without a declared type",
			attach:   func(m *message) { m.VideoNote = &mediaFile{FileID: "vn", FileSize: 8} },
			file:     fakeFile{id: "vn", path: "video_notes/file_505.mp4", declared: 8, contentType: "video/mp4", body: []byte("mp4-note")},
			wantType: "video/mp4",
			wantName: "video_note.mp4",
		},
		{
			name: "audio",
			attach: func(m *message) {
				m.Audio = &mediaFile{FileID: "au", FileSize: 7, MimeType: "audio/mpeg", FileName: "song.mp3"}
			},
			file:     fakeFile{id: "au", path: "music/file_506.mp3", declared: 7, contentType: "audio/mpeg", body: []byte("mp3-aud")},
			wantType: "audio/mpeg",
			wantName: "song.mp3",
		},
		{
			name: "animation that also carries a document",
			attach: func(m *message) {
				m.Animation = &mediaFile{FileID: "an", FileSize: 8, MimeType: "video/mp4", FileName: "loop.mp4"}
				m.Document = &mediaFile{FileID: "an", FileSize: 8, MimeType: "video/mp4", FileName: "loop.mp4"}
			},
			file:     fakeFile{id: "an", path: "animations/file_507.mp4", declared: 8, contentType: "video/mp4", body: []byte("mp4-loop")},
			wantType: "video/mp4",
			wantName: "loop.mp4",
		},
		{
			name:     "document name is reduced to its base",
			attach:   asDocument(mediaFile{FileID: "dn", FileSize: 4, MimeType: "image/png", FileName: "../../etc/passwd"}),
			file:     fakeFile{id: "dn", path: "documents/file_508.png", declared: 4, contentType: "image/png", body: []byte("png!")},
			wantType: "image/png",
			wantName: "passwd",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newMediaRig(t, accessOpts{})
			file := tc.file
			rig.h.api.addFile(&file)
			rig.run(t, privateMedia(10, ownerUserID, "see this", tc.attach))

			accepted := rig.sink.accepted()
			if len(accepted) != 1 {
				t.Fatalf("accepted %d envelopes, want 1 (drops %v)", len(accepted), rig.dropped())
			}
			if accepted[0].Parts[0].Text != "see this" || len(accepted[0].Parts) != 1 {
				t.Errorf("parts = %+v, want the caption only", accepted[0].Parts)
			}
			stored := rig.artifacts.stored()
			if len(stored) != 1 {
				t.Fatalf("stored %d artifacts, want 1", len(stored))
			}
			got := stored[0]
			if !bytes.Equal(got.body, file.body) || got.meta.MediaType != tc.wantType || got.meta.Filename != tc.wantName {
				t.Errorf("stored = %q %q %q, want %q %q %q", got.body, got.meta.MediaType, got.meta.Filename, file.body, tc.wantType, tc.wantName)
			}
			if got.meta.SessionID != accepted[0].ConversationID {
				t.Errorf("artifact session = %q, want the conversation %q", got.meta.SessionID, accepted[0].ConversationID)
			}
			var extra map[string]string
			if err := json.Unmarshal(got.meta.Extra, &extra); err != nil || extra["message_id"] != strconv.Itoa(ownerUserID)+":100" {
				t.Errorf("artifact metadata = %s (%v)", got.meta.Extra, err)
			}
			summaries := artifactsOf(t, accepted[0])
			sum := sha256.Sum256(file.body)
			want := media.ArtifactSummary{RefID: "ref-" + hex.EncodeToString(sum[:])[:12], Digest: hex.EncodeToString(sum[:]), SizeBytes: int64(len(file.body)), Filename: tc.wantName}
			if len(summaries) != 1 || summaries[0] != want {
				t.Errorf("summaries = %+v, want [%+v]", summaries, want)
			}
			if fetched := rig.h.api.fetched(); !slices.Equal(fetched, []string{file.path}) {
				t.Errorf("fetched = %v, want only %q", fetched, file.path)
			}
			if got := rig.dropped(); len(got) != 0 {
				t.Errorf("admitted message produced drop records %v", got)
			}
			if left := rig.leftovers(t); len(left) != 0 {
				t.Errorf("temp files left behind: %v", left)
			}
			rig.assertNoLeak(t, file.path)
		})
	}
}

func TestPhotoPicksTheLargestSizeWithinTheBound(t *testing.T) {
	sizes := []mediaFile{
		{FileID: "small", FileSize: 10, Width: 90, Height: 90},
		{FileID: "medium", FileSize: 30, Width: 320, Height: 320},
		{FileID: "huge", FileSize: 90, Width: 1280, Height: 1280},
	}
	cases := []struct {
		name  string
		limit int64
		want  string
	}{
		{name: "unbounded picks the largest", limit: 1000, want: "huge"},
		{name: "bound excludes the largest", limit: 40, want: "medium"},
		{name: "bound leaves the smallest", limit: 10, want: "small"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newMediaRig(t, accessOpts{})
			rig.adapter.cfg.MaxAttachmentBytes = tc.limit
			for _, size := range sizes {
				rig.h.api.addFile(&fakeFile{id: size.FileID, path: "photos/" + size.FileID + ".jpg", declared: size.FileSize, contentType: "image/jpeg", body: []byte(size.FileID)})
			}
			rig.run(t, privateMedia(10, ownerUserID, "", asPhoto(sizes...)))
			stored := rig.artifacts.stored()
			if len(stored) != 1 || string(stored[0].body) != tc.want {
				t.Fatalf("stored = %+v, want the %q size", stored, tc.want)
			}
		})
	}
}

func TestPhotoWithNoSizeWithinTheBoundIsDropped(t *testing.T) {
	rig := newMediaRig(t, accessOpts{})
	rig.adapter.cfg.MaxAttachmentBytes = 5
	rig.run(t, privateMedia(10, ownerUserID, "", asPhoto(mediaFile{FileID: "only", FileSize: 50, Width: 100, Height: 100})))
	rig.assertQuiet(t, "attachment_failed")
	if got := rig.h.api.count("getFile"); got != 0 {
		t.Errorf("getFile calls = %d, want none", got)
	}
}

func TestCaptionIsTheTurnTextAndCaptionlessPrivateMediaIsAdmitted(t *testing.T) {
	cases := []struct {
		name     string
		caption  string
		wantText string
	}{
		{name: "caption becomes the turn text", caption: "what is this", wantText: "what is this"},
		{name: "captionless media is admitted with a static note", caption: "", wantText: "[document attachment]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newMediaRig(t, accessOpts{})
			rig.h.api.addFile(&fakeFile{id: "d", path: "documents/file_9.pdf", declared: 3, contentType: "application/pdf", body: []byte("pdf")})
			rig.run(t, privateMedia(10, ownerUserID, tc.caption, asDocument(mediaFile{FileID: "d", FileSize: 3, MimeType: "application/pdf"})))
			accepted := rig.sink.accepted()
			if len(accepted) != 1 {
				t.Fatalf("accepted %d envelopes, want 1", len(accepted))
			}
			if got := accepted[0].Parts[0].Text; got != tc.wantText {
				t.Errorf("turn text = %q, want %q", got, tc.wantText)
			}
		})
	}
}

func TestVisionServiceTakesImagesAndStoreTakesTheRest(t *testing.T) {
	jpegBytes := testJPEG(t)
	pngBytes := testPNG(t)
	cases := []struct {
		name       string
		attach     func(*message)
		file       fakeFile
		caption    string
		wantParts  int
		wantStored int
	}{
		{
			name:       "photo with a caption",
			attach:     asPhoto(mediaFile{FileID: "ph", FileSize: int64(len(jpegBytes)), Width: 8, Height: 8}),
			file:       fakeFile{id: "ph", path: "photos/file_1.jpg", contentType: "image/jpeg", body: jpegBytes},
			caption:    "what is this",
			wantParts:  2,
			wantStored: 0,
		},
		{
			name:       "captionless photo carries only the image part text empty",
			attach:     asPhoto(mediaFile{FileID: "ph", FileSize: int64(len(jpegBytes)), Width: 8, Height: 8}),
			file:       fakeFile{id: "ph", path: "photos/file_1.jpg", contentType: "image/jpeg", body: jpegBytes},
			wantParts:  2,
			wantStored: 0,
		},
		{
			name:       "png document",
			attach:     asDocument(mediaFile{FileID: "pd", FileSize: int64(len(pngBytes)), MimeType: "image/png", FileName: "cat.png"}),
			file:       fakeFile{id: "pd", path: "documents/file_2.png", contentType: "image/png", body: pngBytes},
			caption:    "look",
			wantParts:  2,
			wantStored: 0,
		},
		{
			name:       "pdf is not an image",
			attach:     asDocument(mediaFile{FileID: "pf", FileSize: 3, MimeType: "application/pdf"}),
			file:       fakeFile{id: "pf", path: "documents/file_3.pdf", contentType: "application/pdf", body: []byte("pdf")},
			caption:    "read",
			wantParts:  1,
			wantStored: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newMediaRig(t, accessOpts{})
			images := &visionStore{}
			rig.adapter.SetVisionService(testVisionService(t, images))
			file := tc.file
			rig.h.api.addFile(&file)
			rig.run(t, privateMedia(10, ownerUserID, tc.caption, tc.attach))
			accepted := rig.sink.accepted()
			if len(accepted) != 1 {
				t.Fatalf("accepted %d envelopes (drops %v), want 1", len(accepted), rig.dropped())
			}
			env := accepted[0]
			if len(env.Parts) != tc.wantParts || env.Parts[0].Text != tc.caption && tc.caption != "" {
				t.Fatalf("parts = %+v, want %d starting with the caption", env.Parts, tc.wantParts)
			}
			if got := len(rig.artifacts.stored()); got != tc.wantStored {
				t.Errorf("artifact store puts = %d, want %d", got, tc.wantStored)
			}
			if tc.wantParts == 2 {
				part, err := vision.UnmarshalPart(env.Parts[1].Image)
				if err != nil {
					t.Fatalf("UnmarshalPart: %v", err)
				}
				if part.Kind != vision.PartKindImageRef || part.Trust != vision.TrustUntrustedExternal {
					t.Errorf("part = %+v", part)
				}
				if images.count() != 2 {
					t.Errorf("vision puts = %d, want source and derived", images.count())
				}
				summaries := artifactsOf(t, env)
				if len(summaries) != 1 || summaries[0].RefID != part.ArtifactID {
					t.Errorf("summaries = %+v, want the vision artifact %q", summaries, part.ArtifactID)
				}
			}
			if left := rig.leftovers(t); len(left) != 0 {
				t.Errorf("temp files left behind: %v", left)
			}
		})
	}
}

func TestHostileImageIsDroppedWithoutAnArtifact(t *testing.T) {
	rig := newMediaRig(t, accessOpts{})
	images := &visionStore{}
	rig.adapter.SetVisionService(testVisionService(t, images))
	rig.h.api.addFile(&fakeFile{id: "bad", path: "photos/file_6.jpg", contentType: "image/jpeg", body: []byte("not an image at all")})
	rig.run(t, privateMedia(10, ownerUserID, "look", asPhoto(mediaFile{FileID: "bad", FileSize: 19, Width: 8, Height: 8})))
	rig.assertQuiet(t, "attachment_failed")
	if images.count() != 0 || len(rig.artifacts.stored()) != 0 {
		t.Errorf("artifacts written: vision %d, store %d", images.count(), len(rig.artifacts.stored()))
	}
	if left := rig.leftovers(t); len(left) != 0 {
		t.Errorf("temp files left behind: %v", left)
	}
}

func TestAttachmentFailuresDropTheMessageAndLeaveNothingBehind(t *testing.T) {
	big := bytes.Repeat([]byte("x"), smallMediaCap*3)
	pdf := mediaFile{FileID: "f", FileSize: 10, MimeType: "application/pdf", FileName: "a.pdf"}
	cases := []struct {
		name         string
		message      mediaFile
		file         *fakeFile
		storeFails   bool
		withoutStore bool
		wantInfo     int
		wantFetch    int
	}{
		{
			name:    "declared size above the bound in the message",
			message: mediaFile{FileID: "f", FileSize: smallMediaCap + 1, MimeType: "application/pdf"},
			file:    &fakeFile{id: "f", path: "documents/a.pdf", contentType: "application/pdf", body: []byte("pdf")},
		},
		{
			name:      "declared size above the bound in getFile",
			message:   mediaFile{FileID: "f", MimeType: "application/pdf"},
			file:      &fakeFile{id: "f", path: "documents/a.pdf", declared: smallMediaCap + 1, contentType: "application/pdf", body: []byte("pdf")},
			wantInfo:  1,
			wantFetch: 0,
		},
		{
			name:      "streamed body above the bound with a small declared size",
			message:   pdf,
			file:      &fakeFile{id: "f", path: "documents/a.pdf", declared: 10, contentType: "application/pdf", body: big, chunked: true},
			wantInfo:  1,
			wantFetch: 1,
		},
		{
			name:      "content length above the bound with a small declared size",
			message:   pdf,
			file:      &fakeFile{id: "f", path: "documents/a.pdf", declared: 10, contentType: "application/pdf", body: big},
			wantInfo:  1,
			wantFetch: 1,
		},
		{
			name:    "ineligible declared type",
			message: mediaFile{FileID: "f", FileSize: 10, MimeType: "text/html"},
			file:    &fakeFile{id: "f", path: "documents/a.html", declared: 10, contentType: "text/html", body: []byte("<html></html>")},
		},
		{
			name:      "spoofed response type",
			message:   pdf,
			file:      &fakeFile{id: "f", path: "documents/a.pdf", declared: 10, contentType: "text/html", body: []byte("<html></html>")},
			wantInfo:  1,
			wantFetch: 1,
		},
		{
			name:    "getFile failure",
			message: pdf,
			file:    &fakeFile{id: "f", path: "documents/a.pdf", declared: 10, contentType: "application/pdf", body: []byte("pdf"), infoFails: true},

			wantInfo: 1,
		},
		{
			name:      "download that does not succeed",
			message:   pdf,
			file:      &fakeFile{id: "f", path: "documents/a.pdf", declared: 10, contentType: "application/pdf", body: []byte("pdf"), status: http.StatusInternalServerError},
			wantInfo:  1,
			wantFetch: 1,
		},
		{
			name:      "cut-off body",
			message:   pdf,
			file:      &fakeFile{id: "f", path: "documents/a.pdf", declared: 10, contentType: "application/pdf", body: []byte("par"), cutOff: true},
			wantInfo:  1,
			wantFetch: 1,
		},
		{
			name:     "path that climbs out",
			message:  pdf,
			file:     &fakeFile{id: "f", path: "documents/../../secret.pdf", declared: 10, contentType: "application/pdf", body: []byte("pdf")},
			wantInfo: 1,
		},
		{
			name:     "absolute path",
			message:  pdf,
			file:     &fakeFile{id: "f", path: "/etc/passwd", declared: 10, contentType: "application/pdf", body: []byte("pdf")},
			wantInfo: 1,
		},
		{
			name:     "path that changes the request",
			message:  pdf,
			file:     &fakeFile{id: "f", path: "documents/a.pdf?x=1#frag", declared: 10, contentType: "application/pdf", body: []byte("pdf")},
			wantInfo: 1,
		},
		{
			name:     "empty path",
			message:  pdf,
			file:     &fakeFile{id: "f", path: "", declared: 10, contentType: "application/pdf", body: []byte("pdf")},
			wantInfo: 1,
		},
		{
			name:       "artifact store failure",
			message:    pdf,
			file:       &fakeFile{id: "f", path: "documents/a.pdf", declared: 10, contentType: "application/pdf", body: []byte("pdf")},
			storeFails: true,
			wantInfo:   1,
			wantFetch:  1,
		},
		{
			name:         "no artifact store configured",
			message:      pdf,
			file:         &fakeFile{id: "f", path: "documents/a.pdf", declared: 10, contentType: "application/pdf", body: []byte("pdf")},
			withoutStore: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newMediaRig(t, accessOpts{})
			rig.adapter.cfg.MaxAttachmentBytes = smallMediaCap
			rig.h.api.addFile(tc.file)
			if tc.storeFails {
				rig.artifacts.err = errors.New("quota exceeded")
			}
			if tc.withoutStore {
				rig.adapter.SetMediaStore(nil)
			}
			rig.run(t, privateMedia(10, ownerUserID, "here", asDocument(tc.message)))

			rig.assertQuiet(t, "attachment_failed")
			if got := len(rig.artifacts.stored()); got != 0 {
				t.Errorf("stored %d artifacts, want none", got)
			}
			if got := rig.h.api.count("getFile"); got != tc.wantInfo {
				t.Errorf("getFile calls = %d, want %d", got, tc.wantInfo)
			}
			if got := len(rig.h.api.fetched()); got != tc.wantFetch {
				t.Errorf("downloads = %d, want %d", got, tc.wantFetch)
			}
			if left := rig.leftovers(t); len(left) != 0 {
				t.Errorf("temp files left behind: %v", left)
			}
			if calls, _ := rig.sink.stats("update:10"); calls != 0 {
				t.Errorf("runtime was offered the update %d times, want never", calls)
			}
			secrets := []string{"secret.pdf", "/etc/passwd", "documents/a"}
			rig.assertNoLeak(t, secrets...)
		})
	}
}

func TestRejectedSendersAndClosedGatesTriggerNoMediaCall(t *testing.T) {
	document := asDocument(mediaFile{FileID: "f", FileSize: 3, MimeType: "application/pdf"})
	allowed := strconv.Itoa(mediaUserID)
	cases := []struct {
		name      string
		opts      accessOpts
		update    update
		wantDrops []string
	}{
		{name: "unknown private sender", update: privateMedia(10, strangerID, "hi", document), wantDrops: []string{"author_not_allowed"}},
		{name: "unknown sender without a caption", update: privateMedia(10, strangerID, "", document), wantDrops: []string{"author_not_allowed"}},
		{
			name:      "private chats disabled for a trusted sender",
			opts:      accessOpts{allowed: []string{allowed}, privateOff: true},
			update:    privateMedia(10, mediaUserID, "hi", document),
			wantDrops: []string{"private_chats_disabled"},
		},
		{
			name:      "budget refusal",
			opts:      accessOpts{allowed: []string{allowed}, settings: AccessSettings{Budgets: &fakeBudgets{refusal: config.MessageRateLimited}}},
			update:    privateMedia(10, mediaUserID, "hi", document),
			wantDrops: []string{"rate_limited"},
		},
		{
			name:      "unrouted persona",
			opts:      accessOpts{allowed: []string{allowed}, unrouted: []string{allowed}},
			update:    privateMedia(10, mediaUserID, "hi", document),
			wantDrops: []string{"persona_unrouted"},
		},
		{
			name:      "persona audience rejects the sender",
			opts:      accessOpts{allowed: []string{allowed}, definition: auraagent.Definition{ID: testPersona, Name: "Main", Audience: config.AudienceOwner}},
			update:    privateMedia(10, mediaUserID, "hi", document),
			wantDrops: []string{"audience"},
		},
		{
			name:      "group that is not allowlisted",
			opts:      accessOpts{chats: []string{strconv.Itoa(otherChatID)}},
			update:    groupMedia(10, ownerUserID, "hi", document),
			wantDrops: []string{"chat_not_allowed"},
		},
		{
			name:      "group media that does not address the bot",
			update:    groupMedia(10, ownerUserID, "hi there", document),
			wantDrops: []string{"not_mentioned"},
		},
		{
			name:      "captionless group media that does not address the bot",
			update:    groupMedia(10, ownerUserID, "", document),
			wantDrops: []string{"not_mentioned"},
		},
		{
			name:      "chat type that is not served",
			update:    update{UpdateID: 10, Message: &message{MessageID: 100, From: &user{ID: ownerUserID}, Chat: chat{ID: -5, Type: "channel"}, Document: &mediaFile{FileID: "f"}}},
			wantDrops: []string{"chat_not_supported"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newMediaRig(t, tc.opts)
			rig.h.api.addFile(&fakeFile{id: "f", path: "documents/file_1.pdf", declared: 3, contentType: "application/pdf", body: []byte("pdf")})
			rig.runGroup(t, tc.update)
			rig.assertQuiet(t, tc.wantDrops...)
			if got := rig.h.api.count("getFile"); got != 0 {
				t.Errorf("getFile calls = %d, want none", got)
			}
			if got := rig.h.api.fetched(); len(got) != 0 {
				t.Errorf("downloads = %v, want none", got)
			}
			if got := len(rig.artifacts.stored()); got != 0 {
				t.Errorf("stored %d artifacts, want none", got)
			}
		})
	}
}

func TestOpenHandoffHoldsMediaBeforeAnyDownload(t *testing.T) {
	handoffs := &fakeHandoffs{state: "open"}
	rig := newMediaRig(t, accessOpts{
		allowed:  []string{strconv.Itoa(mediaUserID)},
		settings: AccessSettings{Handoffs: handoffs},
	})
	rig.h.api.addFile(&fakeFile{id: "f", path: "documents/file_1.pdf", declared: 3, contentType: "application/pdf", body: []byte("pdf")})
	rig.runGroup(t, privateMedia(10, mediaUserID, "hello", asDocument(mediaFile{FileID: "f", FileSize: 3, MimeType: "application/pdf"})))
	if got := rig.h.api.count("getFile"); got != 0 {
		t.Errorf("getFile calls = %d, want none while a handoff holds the conversation", got)
	}
	if got := len(rig.artifacts.stored()); got != 0 {
		t.Errorf("stored %d artifacts, want none", got)
	}
}

func TestGroupMediaIsAdmittedOnlyWhenItAddressesTheBot(t *testing.T) {
	handle := "@" + testBotName
	file := mediaFile{FileID: "f", FileSize: 3, MimeType: "application/pdf"}
	mention := groupMedia(10, ownerUserID, "hey "+handle+" read this", asDocument(file))
	mention.Message.CaptionEntities = []messageEntity{entityOf(mention.Message.Caption, handle, entityMention)}
	command := groupMedia(10, ownerUserID, "/status"+handle+" now", asDocument(file))
	command.Message.CaptionEntities = []messageEntity{entityOf(command.Message.Caption, "/status"+handle, entityBotCommand)}
	cases := []struct {
		name      string
		update    update
		wantText  string
		wantDrops []string
	}{
		{name: "caption mention", update: mention, wantText: "hey read this"},
		{name: "caption command", update: command, wantText: "/status now"},
		{name: "reply to the bot without a caption", update: replyingTo(groupMedia(10, ownerUserID, "", asDocument(file)), testBotID), wantText: "[document attachment]"},
		{name: "reply to the bot with a caption", update: replyingTo(groupMedia(10, ownerUserID, "and this", asDocument(file)), testBotID), wantText: "and this"},
		{name: "reply to someone else", update: replyingTo(groupMedia(10, ownerUserID, "and this", asDocument(file)), 31337), wantDrops: []string{"not_mentioned"}},
		{name: "mention of another bot", update: mentioningOther(groupMedia(10, ownerUserID, "@"+otherBotName+" read", asDocument(file)), "@"+otherBotName), wantDrops: []string{"not_mentioned"}},
		{name: "caption without any addressing", update: groupMedia(10, ownerUserID, "plain caption", asDocument(file)), wantDrops: []string{"not_mentioned"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newMediaRig(t, accessOpts{})
			rig.h.api.addFile(&fakeFile{id: "f", path: "documents/file_1.pdf", declared: 3, contentType: "application/pdf", body: []byte("pdf")})
			rig.runGroup(t, tc.update)
			if len(tc.wantDrops) > 0 {
				rig.assertQuiet(t, tc.wantDrops...)
				if got := rig.h.api.count("getFile"); got != 0 {
					t.Errorf("getFile calls = %d, want none", got)
				}
				return
			}
			if got := rig.onlyText(t); got != tc.wantText {
				t.Errorf("turn text = %q, want %q", got, tc.wantText)
			}
			if got := len(rig.artifacts.stored()); got != 1 {
				t.Errorf("stored %d artifacts, want 1", got)
			}
		})
	}
}

func mentioningOther(item update, token string) update {
	item.Message.CaptionEntities = []messageEntity{entityOf(item.Message.Caption, token, entityMention)}
	return item
}

func TestReplayedAcceptedUpdateCreatesNoSecondArtifact(t *testing.T) {
	rig := newMediaRig(t, accessOpts{})
	ledger := &ledgerFromSink{sink: rig.sink}
	rig.adapter.SetUpdateLedger(ledger)
	rig.h.api.addFile(&fakeFile{id: "f", path: "documents/file_1.pdf", declared: 3, contentType: "application/pdf", body: []byte("pdf")})
	item := privateMedia(10, ownerUserID, "once", asDocument(mediaFile{FileID: "f", FileSize: 3, MimeType: "application/pdf"}))
	rig.run(t, item, item)

	if got := len(rig.artifacts.stored()); got != 1 {
		t.Errorf("stored %d artifacts after a replay, want 1", got)
	}
	if got := rig.h.api.count("getFile"); got != 1 {
		t.Errorf("getFile calls = %d, want 1", got)
	}
	if calls, turns := rig.sink.stats("update:10"); calls != 1 || turns != 1 {
		t.Errorf("sink calls %d turns %d, want 1 and 1", calls, turns)
	}
	if got := ledger.asked(); got != 2 {
		t.Errorf("ledger consulted %d times, want once per delivery", got)
	}
	if got := rig.dropped(); len(got) != 0 {
		t.Errorf("replay produced drop records %v", got)
	}
}

func TestUnreadableLedgerHoldsTheUpdateForRetry(t *testing.T) {
	rig := newMediaRig(t, accessOpts{})
	ledger := &ledgerFromSink{sink: rig.sink, err: errors.New("database is locked")}
	rig.adapter.SetUpdateLedger(ledger)
	rig.h.api.addFile(&fakeFile{id: "f", path: "documents/file_1.pdf", declared: 3, contentType: "application/pdf", body: []byte("pdf")})
	item := privateMedia(10, ownerUserID, "later", asDocument(mediaFile{FileID: "f", FileSize: 3, MimeType: "application/pdf"}))
	rig.h.api.queue(item)
	rig.h.api.queue(item)
	stop := startAdapter(t, rig.adapter, rig.sink)
	defer stop()
	eventually(t, "a retried delivery", func() bool { return ledger.asked() >= 2 })

	if next, _, _ := rig.h.cursors.Load(t.Context(), "telegram:pocket"); next != 0 {
		t.Errorf("cursor = %d, want it held at the failing update", next)
	}
	if got := len(rig.artifacts.stored()); got != 0 {
		t.Errorf("stored %d artifacts, want none", got)
	}
	if got := rig.dropped(); len(got) != 0 {
		t.Errorf("a transient fault produced drop records %v", got)
	}
}

func TestMediaKindsDecodeFromBotAPIPayloads(t *testing.T) {
	envelope := func(extra string) string {
		return `{"message_id":5,"chat":{"id":1,"type":"private"},"from":{"id":1}` + extra + `}`
	}
	cases := []struct {
		name      string
		payload   string
		wantMedia bool
		wantBody  string
	}{
		{name: "photo", payload: envelope(`,"photo":[{"file_id":"a","file_unique_id":"u","file_size":3,"width":1,"height":1}],"caption":"c","media_group_id":"77"`), wantMedia: true, wantBody: "c"},
		{name: "document", payload: envelope(`,"document":{"file_id":"a","file_name":"n.pdf","mime_type":"application/pdf","file_size":3}`), wantMedia: true},
		{name: "voice", payload: envelope(`,"voice":{"file_id":"a","mime_type":"audio/ogg","duration":2}`), wantMedia: true},
		{name: "video note", payload: envelope(`,"video_note":{"file_id":"a","length":240,"duration":2}`), wantMedia: true},
		{name: "sticker", payload: envelope(`,"sticker":{"file_id":"a","type":"regular"}`)},
		{name: "location", payload: envelope(`,"location":{"latitude":1.5,"longitude":2.5}`)},
		{name: "plain text", payload: envelope(`,"text":"hi"`), wantBody: "hi"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var msg message
			if err := json.Unmarshal([]byte(tc.payload), &msg); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if got := msg.hasMedia(); got != tc.wantMedia {
				t.Errorf("hasMedia = %v, want %v", got, tc.wantMedia)
			}
			if got := msg.body(); got != tc.wantBody {
				t.Errorf("body = %q, want %q", got, tc.wantBody)
			}
			if got := msg.hasContent(); got != (tc.wantMedia || tc.wantBody != "") {
				t.Errorf("hasContent = %v", got)
			}
		})
	}
}

func TestMessagesWithOnlyUnsupportedKindsAreEmptyContent(t *testing.T) {
	rig := newMediaRig(t, accessOpts{})
	rig.run(t, update{UpdateID: 10, Message: &message{MessageID: 100, From: &user{ID: ownerUserID}, Chat: chat{ID: ownerUserID, Type: chatTypePrivate}}})
	rig.assertQuiet(t, "empty_content")
	if got := rig.h.api.count("getFile"); got != 0 {
		t.Errorf("getFile calls = %d, want none", got)
	}
}

func TestSafeFilePath(t *testing.T) {
	cases := map[string]bool{
		"photos/file_12.jpg":      true,
		"voice/file_3.oga":        true,
		"documents/file-4.PDF":    true,
		"a/b/c.mp4":               true,
		"":                        false,
		"/abs/file.jpg":           false,
		"photos//file.jpg":        false,
		"photos/":                 false,
		"../file.jpg":             false,
		"photos/../file.jpg":      false,
		"photos/./file.jpg":       false,
		"photos/file.jpg?x=1":     false,
		"photos/file.jpg#frag":    false,
		"photos/file%2e%2e.jpg":   false,
		"photos\\file.jpg":        false,
		"photos/fi le.jpg":        false,
		"photos/file.jpg\n":       false,
		"photos/файл.jpg":         false,
		"https://evil.example/x":  false,
		"photos/file.jpg:stream":  false,
		"photos/file.jpg/../../x": false,
		"photos/file.jpg/..":      false,
		"photos/.hidden/file.jpg": true,
	}
	for input, want := range cases {
		t.Run(input, func(t *testing.T) {
			if got := safeFilePath(input); got != want {
				t.Errorf("safeFilePath(%q) = %v, want %v", input, got, want)
			}
		})
	}
}
