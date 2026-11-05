package telegram

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/anggasct/aura/internal/channel/media"
	runtimeingress "github.com/anggasct/aura/internal/runtime/ingress"
	"github.com/anggasct/aura/internal/vision"
)

const defaultMediaTimeout = 60 * time.Second

const (
	kindPhoto     = "photo"
	kindDocument  = "document"
	kindAudio     = "audio"
	kindVoice     = "voice"
	kindVideo     = "video"
	kindVideoNote = "video_note"
	kindAnimation = "animation"
)

const photoMediaType = "image/jpeg"

var mediaExtensions = map[string]string{
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

type MediaStore interface {
	Put(ctx context.Context, content io.Reader, meta *media.Meta) (media.Receipt, error)
}

type UpdateLedger interface {
	Claimed(ctx context.Context, source, externalID string) (bool, error)
}

type attachment struct {
	kind      string
	fileID    string
	declared  int64
	mediaType string
	filename  string
}

func (m *message) hasMedia() bool {
	return len(m.Photo) > 0 || m.Document != nil || m.Audio != nil || m.Voice != nil || m.Video != nil || m.VideoNote != nil || m.Animation != nil
}

func (m *message) body() string {
	if m.Text != "" {
		return m.Text
	}
	return m.Caption
}

func (m *message) bodyEntities() []messageEntity {
	if m.Text != "" {
		return m.Entities
	}
	return m.CaptionEntities
}

func (m *message) hasContent() bool {
	return strings.TrimSpace(m.body()) != "" || m.hasMedia()
}

func (m *message) attachment(limit int64) (attachment, bool) {
	candidates := []struct {
		kind     string
		file     *mediaFile
		fallback string
	}{
		{kindAnimation, m.Animation, "video/mp4"},
		{kindVideoNote, m.VideoNote, "video/mp4"},
		{kindVideo, m.Video, "video/mp4"},
		{kindVoice, m.Voice, "audio/ogg"},
		{kindAudio, m.Audio, "audio/mpeg"},
		{kindDocument, m.Document, ""},
	}
	for _, candidate := range candidates {
		if candidate.file == nil {
			continue
		}
		mediaType := candidate.file.MimeType
		if strings.TrimSpace(mediaType) == "" {
			mediaType = candidate.fallback
		}
		return newAttachment(candidate.kind, candidate.file, media.BaseType(mediaType)), true
	}
	if photo, ok := largestPhoto(m.Photo, limit); ok {
		return newAttachment(kindPhoto, &photo, photoMediaType), true
	}
	return attachment{}, false
}

func newAttachment(kind string, file *mediaFile, mediaType string) attachment {
	name := file.FileName
	if strings.TrimSpace(name) == "" {
		name = kind + mediaExtensions[mediaType]
	}
	return attachment{
		kind:      kind,
		fileID:    file.FileID,
		declared:  file.FileSize,
		mediaType: mediaType,
		filename:  media.SanitizeFilename(name),
	}
}

func largestPhoto(sizes []mediaFile, limit int64) (mediaFile, bool) {
	var best mediaFile
	found := false
	for _, size := range sizes {
		if size.FileSize > limit {
			continue
		}
		if !found || size.Width*size.Height > best.Width*best.Height || size.Width*size.Height == best.Width*best.Height && size.FileSize > best.FileSize {
			best, found = size, true
		}
	}
	return best, found
}

func (a *Adapter) visionService() *vision.Service {
	if a == nil {
		return nil
	}
	return a.images.Load()
}

func (a *Adapter) SetMediaStore(store MediaStore) {
	if a == nil {
		return
	}
	a.artifacts = store
}

func (a *Adapter) SetVisionService(svc *vision.Service) {
	if a == nil {
		return
	}
	a.images.Store(svc)
}

func (a *Adapter) SetUpdateLedger(ledger UpdateLedger) {
	if a == nil {
		return
	}
	a.ledger = ledger
}

func (a *Adapter) stageMedia(ctx context.Context, msg *message, envelope *runtimeingress.IngressEnvelope) (bool, error) {
	if a.ledger != nil {
		claimed, err := a.ledger.Claimed(ctx, envelope.Source, envelope.ExternalID)
		if err != nil {
			return false, &Error{Code: ErrorCodeIntakeFailed, Detail: "update ledger is not readable", Cause: err}
		}
		if claimed {
			return false, nil
		}
	}
	if err := a.intakeMedia(ctx, msg, envelope); err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		a.logger.WarnContext(ctx, "telegram attachment intake failed", "component", "telegram", "account", a.account.Name, "error", err)
		a.drop(ctx, messageScope(msg), dropAttachmentFailed, true)
		return false, nil
	}
	return true, nil
}

func (a *Adapter) intakeMedia(ctx context.Context, msg *message, envelope *runtimeingress.IngressEnvelope) error {
	limit := a.cfg.MaxAttachmentBytes
	item, ok := msg.attachment(limit)
	if !ok || item.fileID == "" || item.declared > limit || !media.Eligible(item.mediaType) {
		return Errorf(ErrorCodeAttachmentRejected, "attachment is not eligible")
	}
	if a.artifacts == nil {
		return Errorf(ErrorCodeAttachmentRejected, "media storage is not configured")
	}
	token, ok := a.botToken()
	if !ok {
		return errTokenUnavailable
	}
	call, cancel := context.WithTimeout(ctx, a.mediaTimeout)
	defer cancel()
	info, err := a.bot.getFile(call, token, item.fileID)
	if err != nil {
		return err
	}
	if info.FileSize > limit {
		return Errorf(ErrorCodeAttachmentRejected, "attachment exceeds the size bound")
	}
	file, err := a.bot.download(call, token, info.FilePath, limit)
	if err != nil {
		return err
	}
	defer media.Discard(file)
	summary, image, err := a.storeAttachment(call, &item, file, envelope, msg)
	if err != nil {
		return err
	}
	if err := attachArtifacts(envelope, []media.ArtifactSummary{summary}); err != nil {
		return err
	}
	if len(image) > 0 {
		envelope.Parts = append(envelope.Parts, runtimeingress.InputPart{Image: image})
	} else if strings.TrimSpace(envelope.Parts[0].Text) == "" {
		envelope.Parts[0].Text = "[" + item.kind + " attachment]"
	}
	return nil
}

func (a *Adapter) storeAttachment(ctx context.Context, item *attachment, content *os.File, envelope *runtimeingress.IngressEnvelope, msg *message) (media.ArtifactSummary, json.RawMessage, error) {
	origin := strconv.FormatInt(msg.Chat.ID, 10) + ":" + strconv.FormatInt(msg.MessageID, 10)
	if svc := a.visionService(); svc != nil && media.IsVisionImage(item.mediaType) {
		part, err := svc.Ingest(ctx, &vision.IngestRequest{
			Content:      content,
			DeclaredMIME: item.mediaType,
			Filename:     item.filename,
			SessionID:    envelope.ConversationID,
			Provenance: vision.Provenance{
				Source:     a.source(),
				ExternalID: "message:" + origin,
				TurnID:     strconv.FormatInt(msg.MessageID, 10),
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
	extra, err := json.Marshal(map[string]string{"message_id": origin, "kind": item.kind})
	if err != nil {
		return media.ArtifactSummary{}, nil, Errorf(ErrorCodeProtocolInvalid, "artifact metadata is not serializable")
	}
	receipt, err := a.artifacts.Put(ctx, content, &media.Meta{
		SessionID: envelope.ConversationID,
		Filename:  item.filename,
		MediaType: item.mediaType,
		Extra:     extra,
	})
	if err != nil {
		return media.ArtifactSummary{}, nil, &Error{Code: ErrorCodeAttachmentRejected, Detail: "artifact storage failed", Cause: err}
	}
	return media.ArtifactSummary{RefID: receipt.RefID, Digest: receipt.Digest, SizeBytes: receipt.SizeBytes, Filename: item.filename}, nil, nil
}

func attachArtifacts(envelope *runtimeingress.IngressEnvelope, summaries []media.ArtifactSummary) error {
	var reference replyReference
	if err := json.Unmarshal(envelope.ReplyContext, &reference); err != nil {
		return Errorf(ErrorCodeProtocolInvalid, "reply context is not decodable")
	}
	reference.Artifacts = summaries
	raw, err := json.Marshal(reference)
	if err != nil {
		return Errorf(ErrorCodeProtocolInvalid, "reply context is not serializable")
	}
	envelope.ReplyContext = raw
	return nil
}
