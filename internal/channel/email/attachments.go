package email

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"unicode"

	"github.com/emersion/go-message"

	"github.com/anggasct/aura/internal/channel/media"
	"github.com/anggasct/aura/internal/vision"
)

const (
	maxAttachmentsPerMessage = 10
	defaultAttachmentName    = "attachment"
)

type stagedAttachment struct {
	file      *os.File
	filename  string
	mediaType string
}

type attachmentStage struct {
	items []stagedAttachment
	seen  int
	limit int64
	err   error
}

func (s *attachmentStage) discard() {
	for i := range s.items {
		media.Discard(s.items[i].file)
	}
	s.items = nil
}

func stageAttachments(raw []byte, limit int64) (*attachmentStage, error) {
	stage := &attachmentStage{limit: limit}
	entity, err := message.Read(bytes.NewReader(raw))
	if entity == nil || !tolerableMIMEError(err) {
		return nil, &Error{Code: ErrorCodeAttachmentRejected, Detail: "message structure is not readable", Cause: err}
	}
	stage.walk(entity, 0)
	if stage.err != nil {
		stage.discard()
		return nil, stage.err
	}
	return stage, nil
}

func tolerableMIMEError(err error) bool {
	return err == nil || message.IsUnknownCharset(err) || message.IsUnknownEncoding(err)
}

func (s *attachmentStage) walk(entity *message.Entity, depth int) {
	if s.err != nil {
		return
	}
	if depth > maxMIMEDepth || s.seen >= maxMIMEParts {
		s.err = Errorf(ErrorCodeAttachmentRejected, "message structure is deeper or wider than supported")
		return
	}
	s.seen++
	if reader := entity.MultipartReader(); reader != nil {
		for s.err == nil {
			child, err := reader.NextPart()
			if errors.Is(err, io.EOF) && child == nil {
				return
			}
			if child == nil || !tolerableMIMEError(err) {
				s.err = &Error{Code: ErrorCodeAttachmentRejected, Detail: "message structure is not readable", Cause: err}
				return
			}
			s.walk(child, depth+1)
		}
		return
	}
	mediaType, params, err := entity.Header.ContentType()
	if err != nil || !media.Eligible(mediaType) {
		return
	}
	mediaType = media.BaseType(mediaType)
	if len(s.items) >= maxAttachmentsPerMessage {
		s.err = Errorf(ErrorCodeAttachmentRejected, "message carries more attachments than supported")
		return
	}
	file, err := media.Spool(entity.Body, s.limit)
	if err != nil {
		s.err = &Error{Code: ErrorCodeAttachmentRejected, Detail: "attachment is over the bound or unreadable", Cause: err}
		return
	}
	s.items = append(s.items, stagedAttachment{file: file, filename: attachmentName(entity, params, mediaType), mediaType: mediaType})
}

var attachmentExtensions = map[string]string{
	"image/jpeg":      ".jpg",
	"image/png":       ".png",
	"image/webp":      ".webp",
	"image/gif":       ".gif",
	"audio/ogg":       ".ogg",
	"audio/mpeg":      ".mp3",
	"audio/mp4":       ".m4a",
	"video/mp4":       ".mp4",
	"application/pdf": ".pdf",
}

func attachmentName(entity *message.Entity, typeParams map[string]string, mediaType string) string {
	name := typeParams["name"]
	if _, params, err := entity.Header.ContentDisposition(); err == nil && params["filename"] != "" {
		name = params["filename"]
	}
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, strings.ToValidUTF8(name, ""))
	if strings.TrimSpace(name) == "" {
		return defaultAttachmentName + attachmentExtensions[mediaType]
	}
	return media.SanitizeFilename(name)
}

func (a *Adapter) storeAttachments(ctx context.Context, stage *attachmentStage, found *admission) ([]media.ArtifactSummary, []json.RawMessage, error) {
	if len(stage.items) == 0 {
		return nil, nil, nil
	}
	if a.artifacts == nil {
		return nil, nil, Errorf(ErrorCodeAttachmentRejected, "media storage is not configured")
	}
	summaries := make([]media.ArtifactSummary, 0, len(stage.items))
	var images []json.RawMessage
	for i := range stage.items {
		item := &stage.items[i]
		if svc := a.images.Load(); svc != nil && media.IsVisionImage(item.mediaType) {
			summary, raw, err := a.putVisionImage(ctx, svc, item, found)
			if err != nil {
				return nil, nil, err
			}
			summaries = append(summaries, summary)
			images = append(images, raw)
			continue
		}
		summary, err := a.putAttachment(ctx, item, found)
		if err != nil {
			return nil, nil, err
		}
		summaries = append(summaries, summary)
	}
	return summaries, images, nil
}

func (a *Adapter) putVisionImage(ctx context.Context, svc *vision.Service, item *stagedAttachment, found *admission) (media.ArtifactSummary, json.RawMessage, error) {
	if _, err := item.file.Seek(0, io.SeekStart); err != nil {
		return media.ArtifactSummary{}, nil, &Error{Code: ErrorCodeAttachmentRejected, Detail: "attachment is not rewindable", Cause: err}
	}
	part, err := svc.Ingest(ctx, &vision.IngestRequest{
		Content:      item.file,
		DeclaredMIME: item.mediaType,
		Filename:     item.filename,
		SessionID:    found.session,
		Provenance: vision.Provenance{
			Source:     a.source(),
			ExternalID: "message:" + found.messageID,
			TurnID:     found.messageID,
		},
	})
	if err != nil {
		return media.ArtifactSummary{}, nil, &Error{Code: ErrorCodeAttachmentRejected, Detail: "image intake failed", Cause: err}
	}
	raw, err := vision.MarshalPart(&part)
	if err != nil {
		return media.ArtifactSummary{}, nil, &Error{Code: ErrorCodeAttachmentRejected, Detail: "image reference is not serializable", Cause: err}
	}
	return media.ArtifactSummary{RefID: part.ArtifactID, Digest: part.DerivedDigest, SizeBytes: part.EncodedBytes, Filename: item.filename}, raw, nil
}

func (a *Adapter) putAttachment(ctx context.Context, item *stagedAttachment, found *admission) (media.ArtifactSummary, error) {
	extra, err := json.Marshal(map[string]string{"message_id": found.messageID})
	if err != nil {
		return media.ArtifactSummary{}, Errorf(ErrorCodeAttachmentRejected, "artifact metadata is not serializable")
	}
	receipt, err := a.artifacts.Put(ctx, item.file, &media.Meta{
		SessionID: found.session,
		Filename:  item.filename,
		MediaType: item.mediaType,
		Extra:     extra,
	})
	if err != nil {
		return media.ArtifactSummary{}, &Error{Code: ErrorCodeAttachmentRejected, Detail: "artifact storage failed", Cause: err}
	}
	return media.ArtifactSummary{RefID: receipt.RefID, Digest: receipt.Digest, SizeBytes: receipt.SizeBytes, Filename: item.filename}, nil
}

func attachArtifacts(found *admission, summaries []media.ArtifactSummary) error {
	var reference replyReference
	if err := json.Unmarshal(found.reply, &reference); err != nil {
		return Errorf(ErrorCodeAttachmentRejected, "reply context is not decodable")
	}
	reference.Artifacts = summaries
	raw, err := json.Marshal(reference)
	if err != nil {
		return Errorf(ErrorCodeAttachmentRejected, "reply context is not serializable")
	}
	found.reply = raw
	return nil
}
