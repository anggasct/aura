package discord

import (
	"net/http"
	"testing"

	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/turn"
	runtimeingress "github.com/anggasct/aura/internal/platform/turn/ingress"
)

func TestDiscordForwardSnapshotIsRelayedInput(t *testing.T) {
	msg := &messagePayload{
		ID:        "msg-fwd-1",
		ChannelID: "chan-dm",
		Author:    userPayload{ID: "owner-1", Username: "owner"},
		Content:   "",
		MessageReference: &messageReferencePayload{
			Type:      referenceTypeForward,
			MessageID: "orig-1",
			ChannelID: "chan-other",
		},
		MessageSnapshots: []messageSnapshotPayload{
			{
				Message: snapshotMessagePayload{
					Content: "forwarded text from another channel",
					Attachments: []attachmentPayload{
						{Filename: "note.txt", ContentType: "text/plain", Size: 12, URL: "https://cdn.discordapp.com/attachments/1/note.txt"},
					},
				},
			},
		},
	}

	if !msg.isForward() || !msg.hasForwardContent() {
		t.Fatal("forward message not recognized as forward with content")
	}
	if atts := msg.allAttachments(); len(atts) != 1 || atts[0].Filename != "note.txt" {
		t.Fatalf("allAttachments() = %+v, want snapshot attachment note.txt", atts)
	}

	parts := []runtimeingress.InputPart{
		{Text: msg.MessageSnapshots[0].Message.Content, Source: runtimeingress.PartSourceRelayed, Detail: "discord forward"},
		{Text: "[attachment \"note.txt\"]", Source: runtimeingress.PartSourceAttachment, Detail: "document"},
	}
	req := &turn.TurnRequest{
		Origin:         "discord:main",
		PrincipalClass: principal.ClassOwner,
		Parts:          parts,
	}
	mark, tainted := req.AdmissionTaint()
	if !tainted || mark.Source != turn.TaintSourceRelayedInput || mark.Detail != "discord forward" {
		t.Fatalf("AdmissionTaint() = %+v %v, want relayed_input discord forward", mark, tainted)
	}
}

func TestDiscordTextPlusNonVisionAttachmentTaints(t *testing.T) {
	cases := []struct {
		name        string
		id          string
		body        string
		filename    string
		contentType string
		detail      string
	}{
		{name: "text plus non-vision image", id: "4101", body: "fake-png-bytes", filename: "cat.png", contentType: "image/png", detail: "image"},
		{name: "text plus pdf", id: "4102", body: "%PDF-1.4", filename: "spec.pdf", contentType: "application/pdf", detail: "document"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gateway := newFakeGateway(t, true)
			stub := newMediaStub(t, tc.body, tc.contentType, http.StatusOK)
			ws, sink, testCtx := startMediaSession(t, gateway)
			defer testCtx.stop(t)

			sendAndWait(t, gateway, ws, messageWithAttachments(tc.id, "please inspect", []attachmentPayload{
				{URL: stub.URL + "/" + tc.filename, Filename: tc.filename, Size: int64(len(tc.body)), ContentType: tc.contentType},
			}), 1, sink)

			env := admittedEnvelope(t, sink, tc.id)
			req := &turn.TurnRequest{
				Origin:         turn.Origin(env.Source),
				PrincipalClass: principal.ClassOwner,
				Parts:          env.Parts,
			}
			mark, tainted := req.AdmissionTaint()
			if !tainted || mark.Source != turn.TaintSourceAttachment || mark.Detail != tc.detail {
				t.Fatalf("parts=%+v AdmissionTaint() = %+v %v, want attachment %q", env.Parts, mark, tainted, tc.detail)
			}
		})
	}
}
