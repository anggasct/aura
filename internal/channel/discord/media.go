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
	"path"
	"strings"
	"time"
)

const (
	maxAttachmentsPerMessage = 10
	mediaDownloadTimeout     = 60 * time.Second
	maxFilenameRunes         = 128
)

var allowedMediaPrefixes = []string{"image/", "audio/", "video/"}

const allowedPDFMediaType = "application/pdf"

type attachmentPayload struct {
	URL         string `json:"url"`
	Filename    string `json:"filename"`
	Size        int64  `json:"size"`
	ContentType string `json:"content_type"`
}

type ArtifactMeta struct {
	SessionID string
	Filename  string
	MediaType string
	Extra     json.RawMessage
}

type ArtifactReceipt struct {
	RefID     string
	Digest    string
	SizeBytes int64
}

type MediaStore interface {
	Put(ctx context.Context, content io.Reader, meta *ArtifactMeta) (ArtifactReceipt, error)
}

type SessionEnsurer interface {
	EnsureSession(ctx context.Context, sessionID, ownerID string) error
}

type artifactSummary struct {
	RefID     string `json:"ref_id"`
	Digest    string `json:"digest"`
	SizeBytes int64  `json:"size_bytes"`
	Filename  string `json:"filename"`
}

func isLoopbackHost(host string) bool {
	trimmed := strings.TrimSuffix(host, ".")
	if trimmed == "localhost" {
		return true
	}
	ip := net.ParseIP(trimmed)
	return ip != nil && ip.IsLoopback()
}

func sanitizeFilename(name string) string {

	base := path.Base(strings.ReplaceAll(name, "\x00", ""))
	base = strings.TrimSpace(base)
	if base == "" || base == "." || base == "/" {
		return "attachment"
	}
	runes := []rune(base)
	if len(runes) > maxFilenameRunes {
		base = string(runes[:maxFilenameRunes])
	}
	return base
}

func allowedMediaType(contentType string) bool {
	mediaType := strings.ToLower(strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0]))
	if mediaType == allowedPDFMediaType {
		return true
	}
	for _, prefix := range allowedMediaPrefixes {
		if strings.HasPrefix(mediaType, prefix) {
			return true
		}
	}
	return false
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
	if !allowedMediaType(attachment.ContentType) {
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
	if actual != "" && !allowedMediaType(actual) {
		return nil, Errorf(ErrorCodeAttachmentRejected, "attachment media type is not eligible")
	}
	tmp, err := os.CreateTemp("", "discord-media-*")
	if err != nil {
		return nil, err
	}
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
	}
	written, err := io.Copy(tmp, io.LimitReader(resp.Body, limit+1))
	if err != nil {
		cleanup()
		return nil, Errorf(ErrorCodeAttachmentRejected, "attachment download did not complete")
	}
	if written > limit {
		cleanup()
		return nil, Errorf(ErrorCodeAttachmentRejected, "attachment size is outside the allowed bound")
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		cleanup()
		return nil, Errorf(ErrorCodeAttachmentRejected, "attachment download did not complete")
	}
	return tmp, nil
}

func (a *Adapter) storeAttachments(ctx context.Context, attachments []attachmentPayload, sessionID, principal, messageID string) ([]artifactSummary, error) {
	if len(attachments) == 0 {
		return nil, nil
	}
	if len(attachments) > maxAttachmentsPerMessage {
		return nil, Errorf(ErrorCodeAttachmentRejected, "message carries more attachments than supported")
	}
	stash := a.cfg.MaxAttachmentBytes
	staged := make([]*os.File, 0, len(attachments))
	defer func() {
		for _, tmp := range staged {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	for index := range attachments {
		tmp, err := a.downloadAttachment(ctx, &attachments[index], stash)
		if err != nil {
			return nil, err
		}
		staged = append(staged, tmp)
	}
	if err := a.sessions.EnsureSession(ctx, sessionID, principal); err != nil {
		return nil, err
	}
	summaries := make([]artifactSummary, 0, len(attachments))
	for index := range attachments {
		receipt, err := a.putAttachment(ctx, &attachments[index], staged[index], sessionID, messageID)
		if err != nil {
			return nil, err
		}
		summaries = append(summaries, artifactSummary{
			RefID:     receipt.RefID,
			Digest:    receipt.Digest,
			SizeBytes: receipt.SizeBytes,
			Filename:  attachments[index].Filename,
		})
	}
	return summaries, nil
}

func (a *Adapter) putAttachment(ctx context.Context, attachment *attachmentPayload, content io.Reader, sessionID, messageID string) (ArtifactReceipt, error) {
	extra, err := json.Marshal(map[string]string{"message_id": messageID})
	if err != nil {
		return ArtifactReceipt{}, Errorf(ErrorCodeProtocolInvalid, "artifact metadata is not serializable")
	}
	return a.media.Put(ctx, content, &ArtifactMeta{
		SessionID: sessionID,
		Filename:  sanitizeFilename(attachment.Filename),
		MediaType: attachment.ContentType,
		Extra:     extra,
	})
}
