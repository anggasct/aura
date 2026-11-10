package media

import (
	"encoding/json"
	"io"
	"os"
	"path"
	"strings"
)

const maxFilenameRunes = 128

const pdfMediaType = "application/pdf"

var eligiblePrefixes = []string{"image/", "audio/", "video/"}

var visionImageTypes = []string{"image/png", "image/jpeg", "image/webp", "image/gif"}

type ArtifactSummary struct {
	RefID     string `json:"ref_id"`
	Digest    string `json:"digest"`
	SizeBytes int64  `json:"size_bytes"`
	Filename  string `json:"filename"`
}

type Meta struct {
	SessionID string
	Filename  string
	MediaType string
	Extra     json.RawMessage
}

type Receipt struct {
	RefID     string
	Digest    string
	SizeBytes int64
}

func BaseType(contentType string) string {
	head, _, _ := strings.Cut(contentType, ";")
	return strings.ToLower(strings.TrimSpace(head))
}

func Eligible(contentType string) bool {
	mediaType := BaseType(contentType)
	if mediaType == pdfMediaType {
		return true
	}
	for _, prefix := range eligiblePrefixes {
		if strings.HasPrefix(mediaType, prefix) {
			return true
		}
	}
	return false
}

func IsVisionImage(contentType string) bool {
	mediaType := BaseType(contentType)
	for _, candidate := range visionImageTypes {
		if mediaType == candidate {
			return true
		}
	}
	return false
}

func SanitizeFilename(name string) string {
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

func Spool(body io.Reader, limit int64) (*os.File, error) {
	tmp, err := os.CreateTemp("", "aura-media-*")
	if err != nil {
		return nil, &Error{Code: ErrorCodeSpoolFailed, Detail: "temporary file is not creatable", Cause: err}
	}
	discard := func() {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
	}
	written, err := io.Copy(tmp, io.LimitReader(body, limit+1))
	if err != nil {
		discard()
		return nil, &Error{Code: ErrorCodeIncomplete, Detail: "content did not arrive completely", Cause: err}
	}
	if written > limit {
		discard()
		return nil, &Error{Code: ErrorCodeSizeExceeded, Detail: "content exceeds the size bound"}
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		discard()
		return nil, &Error{Code: ErrorCodeSpoolFailed, Detail: "temporary file is not rewindable", Cause: err}
	}
	return tmp, nil
}

func Discard(file *os.File) {
	if file == nil {
		return
	}
	_ = file.Close()
	_ = os.Remove(file.Name())
}
