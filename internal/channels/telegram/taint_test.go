package telegram

import (
	"testing"

	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/turn"
	runtimeingress "github.com/anggasct/aura/internal/platform/turn/ingress"
)

func TestRelayedAndAttachedInputTaintsTheTurn(t *testing.T) {
	t.Run("forwarded telegram message produces relayed_input", func(t *testing.T) {
		msg := &message{
			MessageID:     10,
			ForwardOrigin: []byte(`{"type":"user"}`),
			Text:          "forwarded content",
		}
		if !msg.relayed() {
			t.Fatal("expected message with ForwardOrigin to be relayed")
		}
		parts := []runtimeingress.InputPart{
			{Text: msg.Text, Source: runtimeingress.PartSourceRelayed, Detail: msg.relayedDetail()},
		}
		req := &turn.TurnRequest{
			Origin:         "telegram:main",
			PrincipalClass: principal.ClassOwner,
			Parts:          parts,
		}
		mark, tainted := req.AdmissionTaint()
		if !tainted || mark.Source != turn.TaintSourceRelayedInput {
			t.Fatalf("AdmissionTaint() = %+v %v, want relayed_input", mark, tainted)
		}
	})

	t.Run("telegram attachment ingestion with caption taints every media type", func(t *testing.T) {
		cases := []struct {
			name   string
			attach func(*message)
			file   fakeFile
			detail string
		}{
			{
				name:   "caption with pdf",
				attach: asDocument(mediaFile{FileID: "doc-1", FileSize: 8, MimeType: "application/pdf", FileName: "spec.pdf"}),
				file:   fakeFile{id: "doc-1", path: "documents/spec.pdf", declared: 8, contentType: "application/pdf", body: []byte("%PDF-1.4")},
				detail: "document",
			},
			{
				name: "caption with audio",
				attach: func(m *message) {
					m.Audio = &mediaFile{FileID: "aud-1", FileSize: 4, MimeType: "audio/mpeg", FileName: "clip.mp3"}
				},
				file:   fakeFile{id: "aud-1", path: "audio/clip.mp3", declared: 4, contentType: "audio/mpeg", body: []byte("ID3\x00")},
				detail: "audio",
			},
			{
				name:   "caption with non-vision image",
				attach: asPhoto(mediaFile{FileID: "img-1", FileSize: 4, Width: 10, Height: 10}),
				file:   fakeFile{id: "img-1", path: "photos/pic.jpg", declared: 4, contentType: "image/jpeg", body: []byte("jpg!")},
				detail: "image",
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				rig := newMediaRig(t, accessOpts{})
				file := tc.file
				rig.h.api.addFile(&file)
				rig.run(t, privateMedia(10, ownerUserID, "check this", tc.attach))

				accepted := rig.sink.accepted()
				if len(accepted) != 1 {
					t.Fatalf("accepted %d envelopes (drops=%v), want 1", len(accepted), rig.dropped())
				}
				req := &turn.TurnRequest{
					Origin:         turn.Origin(accepted[0].Source),
					PrincipalClass: principal.ClassOwner,
					Parts:          accepted[0].Parts,
				}
				mark, tainted := req.AdmissionTaint()
				if !tainted || mark.Source != turn.TaintSourceAttachment || mark.Detail != tc.detail {
					t.Fatalf("parts=%+v AdmissionTaint() = %+v %v, want attachment %q", accepted[0].Parts, mark, tainted, tc.detail)
				}
			})
		}
	})
}
