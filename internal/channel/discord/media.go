package discord

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/anggasct/aura/internal/channel/media"
	"github.com/anggasct/aura/internal/vision"
)

const (
	maxAttachmentsPerMessage = 10
	mediaDownloadTimeout     = 60 * time.Second
)

type attachmentPayload struct {
	URL         string `json:"url"`
	Filename    string `json:"filename"`
	Size        int64  `json:"size"`
	ContentType string `json:"content_type"`
}

type MediaStore interface {
	Put(ctx context.Context, content io.Reader, meta *media.Meta) (media.Receipt, error)
}

type SessionEnsurer interface {
	EnsureSession(ctx context.Context, sessionID, ownerID, agentID string) error
	SessionExists(ctx context.Context, sessionID string) (bool, error)
}

type SessionResolver interface {
	ActiveSession(ctx context.Context, conversationKey string) (string, error)
}

func isLoopbackHost(host string) bool {
	trimmed := strings.TrimSuffix(host, ".")
	if trimmed == "localhost" {
		return true
	}
	ip := net.ParseIP(trimmed)
	return ip != nil && ip.IsLoopback()
}

func (a *Adapter) mediaClient() *http.Client {
	return &http.Client{
		Timeout: mediaDownloadTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("discord: media redirect chain is too long")
			}
			if req.URL.Scheme != "https" {
				return errors.New("discord: media redirect leaves https")
			}
			return nil
		},
	}
}

func (a *Adapter) downloadAttachment(ctx context.Context, attachment *attachmentPayload, limit int64) (*os.File, error) {
	endpoint, err := url.Parse(attachment.URL)
	if err != nil || endpoint.Hostname() == "" {
		return nil, Errorf(ErrorCodeAttachmentRejected, "attachment url is not a valid endpoint")
	}
	if endpoint.Scheme != "https" && (endpoint.Scheme != "http" || !isLoopbackHost(endpoint.Hostname())) {
		return nil, Errorf(ErrorCodeAttachmentRejected, "attachment url is not a valid https endpoint")
	}
	if attachment.Size <= 0 || attachment.Size > limit {
		return nil, Errorf(ErrorCodeAttachmentRejected, "attachment size is outside the allowed bound")
	}
	if !media.Eligible(attachment.ContentType) {
		return nil, Errorf(ErrorCodeAttachmentRejected, "attachment media type is not eligible")
	}
	call, cancel := context.WithTimeout(ctx, mediaDownloadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(call, http.MethodGet, endpoint.String(), http.NoBody)
	if err != nil {
		return nil, err
	}
	resp, err := a.mediaClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, Errorf(ErrorCodeAttachmentRejected, "attachment download did not succeed")
	}
	actual := resp.Header.Get("Content-Type")
	if actual != "" && !media.Eligible(actual) {
		return nil, Errorf(ErrorCodeAttachmentRejected, "attachment media type is not eligible")
	}
	tmp, err := media.Spool(resp.Body, limit)
	if err != nil {
		if code, ok := media.CodeOf(err); ok && code == media.ErrorCodeSizeExceeded {
			return nil, Errorf(ErrorCodeAttachmentRejected, "attachment size is outside the allowed bound")
		}
		return nil, Errorf(ErrorCodeAttachmentRejected, "attachment download did not complete")
	}
	return tmp, nil
}

func (a *Adapter) storeAttachments(ctx context.Context, attachments []attachmentPayload, sessionID, principal, agentID, messageID string) ([]media.ArtifactSummary, []json.RawMessage, error) {
	if len(attachments) == 0 {
		return nil, nil, nil
	}
	if len(attachments) > maxAttachmentsPerMessage {
		return nil, nil, Errorf(ErrorCodeAttachmentRejected, "message carries more attachments than supported")
	}
	stash := a.cfg.MaxAttachmentBytes
	staged := make([]*os.File, 0, len(attachments))
	defer func() {
		for _, tmp := range staged {
			media.Discard(tmp)
		}
	}()
	for index := range attachments {
		tmp, err := a.downloadAttachment(ctx, &attachments[index], stash)
		if err != nil {
			return nil, nil, err
		}
		staged = append(staged, tmp)
	}
	if err := a.sessions.EnsureSession(ctx, sessionID, principal, agentID); err != nil {
		return nil, nil, err
	}
	summaries := make([]media.ArtifactSummary, 0, len(attachments))
	var images []json.RawMessage
	visionSvc := a.visionService()
	for index := range attachments {
		if visionSvc != nil && media.IsVisionImage(attachments[index].ContentType) {
			summary, raw, err := a.putVisionImage(ctx, &attachments[index], staged[index], sessionID, messageID)
			if err != nil {
				return nil, nil, err
			}
			summaries = append(summaries, summary)
			images = append(images, raw)
			continue
		}
		receipt, err := a.putAttachment(ctx, &attachments[index], staged[index], sessionID, messageID)
		if err != nil {
			return nil, nil, err
		}
		summaries = append(summaries, media.ArtifactSummary{
			RefID:     receipt.RefID,
			Digest:    receipt.Digest,
			SizeBytes: receipt.SizeBytes,
			Filename:  attachments[index].Filename,
		})
	}
	return summaries, images, nil
}

func (a *Adapter) putVisionImage(ctx context.Context, attachment *attachmentPayload, content *os.File, sessionID, messageID string) (media.ArtifactSummary, json.RawMessage, error) {
	visionSvc := a.visionService()
	if visionSvc == nil {
		return media.ArtifactSummary{}, nil, Errorf(ErrorCodeAttachmentRejected, "image intake is not configured")
	}
	if _, err := content.Seek(0, io.SeekStart); err != nil {
		return media.ArtifactSummary{}, nil, Errorf(ErrorCodeAttachmentRejected, "attachment download did not complete")
	}
	source := a.source()
	part, err := visionSvc.Ingest(ctx, &vision.IngestRequest{
		Content:      content,
		DeclaredMIME: attachment.ContentType,
		Filename:     media.SanitizeFilename(attachment.Filename),
		SessionID:    sessionID,
		Provenance: vision.Provenance{
			Source:     source,
			ExternalID: "message:" + messageID,
			TurnID:     messageID,
		},
	})
	if err != nil {
		return media.ArtifactSummary{}, nil, err
	}
	raw, err := vision.MarshalPart(&part)
	if err != nil {
		return media.ArtifactSummary{}, nil, err
	}
	return media.ArtifactSummary{
		RefID:     part.ArtifactID,
		Digest:    part.DerivedDigest,
		SizeBytes: part.EncodedBytes,
		Filename:  media.SanitizeFilename(attachment.Filename),
	}, raw, nil
}

func (a *Adapter) putAttachment(ctx context.Context, attachment *attachmentPayload, content io.Reader, sessionID, messageID string) (media.Receipt, error) {
	extra, err := json.Marshal(map[string]string{"message_id": messageID})
	if err != nil {
		return media.Receipt{}, Errorf(ErrorCodeProtocolInvalid, "artifact metadata is not serializable")
	}
	return a.media.Put(ctx, content, &media.Meta{
		SessionID: sessionID,
		Filename:  media.SanitizeFilename(attachment.Filename),
		MediaType: attachment.ContentType,
		Extra:     extra,
	})
}
