package telegram

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"

	"github.com/anggasct/aura/internal/channel/media"
)

func (c *botClient) getFile(ctx context.Context, token, fileID string) (fileInfo, error) {
	var info fileInfo
	if err := c.call(ctx, token, "getFile", getFileRequest{FileID: fileID}, &info); err != nil {
		return fileInfo{}, err
	}
	return info, nil
}

func (c *botClient) download(ctx context.Context, token, filePath string, limit int64) (*os.File, error) {
	if !safeFilePath(filePath) {
		return nil, Errorf(ErrorCodeAttachmentRejected, "file path is not acceptable")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/file/bot"+token+"/"+filePath, http.NoBody)
	if err != nil {
		return nil, Errorf(ErrorCodeConnectionFailed, "file download request is not constructible")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, &Error{Code: ErrorCodeConnectionFailed, Detail: "file download request failed", Cause: scrub(redact(err, token), filePath)}
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode != http.StatusOK:
		return nil, Errorf(ErrorCodeAttachmentRejected, "file download did not succeed")
	case !servedTypeAcceptable(resp.Header.Get("Content-Type")):
		return nil, Errorf(ErrorCodeAttachmentRejected, "file media type is not eligible")
	case resp.ContentLength > limit:
		return nil, Errorf(ErrorCodeAttachmentRejected, "file exceeds the size bound")
	}
	file, err := media.Spool(resp.Body, limit)
	if err != nil {
		if code, ok := media.CodeOf(err); ok && code == media.ErrorCodeSizeExceeded {
			return nil, Errorf(ErrorCodeAttachmentRejected, "file exceeds the size bound")
		}
		return nil, Errorf(ErrorCodeAttachmentRejected, "file download did not complete")
	}
	return file, nil
}

func scrub(err error, secret string) error {
	var wrapped *redactedError
	if !errors.As(err, &wrapped) {
		return err
	}
	return &redactedError{text: strings.ReplaceAll(wrapped.text, secret, "[redacted]"), cause: wrapped.cause}
}

func servedTypeAcceptable(contentType string) bool {
	served, _, _ := strings.Cut(contentType, ";")
	switch strings.ToLower(strings.TrimSpace(served)) {
	case "", "application/octet-stream":
		return true
	default:
		return media.Eligible(contentType)
	}
}

func safeFilePath(filePath string) bool {
	if filePath == "" {
		return false
	}
	for segment := range strings.SplitSeq(filePath, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return !strings.ContainsFunc(filePath, func(r rune) bool {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return false
		case r == '.', r == '_', r == '-', r == '/':
			return false
		default:
			return true
		}
	})
}
