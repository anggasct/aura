package telegram

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"

	"github.com/anggasct/aura/internal/channels/kit/media"
	"github.com/anggasct/aura/internal/platform/fault"
)

func (c *botClient) getFile(ctx context.Context, token, fileID string) (fileInfo, error) {
	var info fileInfo
	if err := c.call(ctx, token, "getFile", getFileRequest{FileID: fileID}, &info); err != nil {
		return fileInfo{}, err
	}
	return info, nil
}

func (c *botClient) download(ctx context.Context, token, filePath string, limit int64, intake media.Intake) (*os.File, error) {
	if !safeFilePath(filePath) {
		return nil, fault.New(ErrorCodeAttachmentRejected, "file path is not acceptable")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/file/bot"+token+"/"+filePath, http.NoBody)
	if err != nil {
		return nil, fault.New(ErrorCodeConnectionFailed, "file download request is not constructible")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fault.Wrap(ErrorCodeConnectionFailed, "file download request failed", scrub(redact(err, token), filePath))
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode != http.StatusOK:
		return nil, fault.New(ErrorCodeAttachmentRejected, "file download did not succeed")
	case !intake.Served(resp.Header.Get("Content-Type"), true):
		return nil, fault.New(ErrorCodeAttachmentRejected, "file media type is not eligible")
	case resp.ContentLength > limit:
		return nil, fault.New(ErrorCodeAttachmentRejected, "file exceeds the size bound")
	}
	file, err := media.Spool(resp.Body, limit)
	if err != nil {
		if fault.HasCode(err, media.ErrorCodeSizeExceeded) {
			return nil, fault.New(ErrorCodeAttachmentRejected, "file exceeds the size bound")
		}
		return nil, fault.New(ErrorCodeAttachmentRejected, "file download did not complete")
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
