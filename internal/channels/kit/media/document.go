package media

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/anggasct/aura/internal/platform/fault"

	"github.com/anggasct/aura/internal/platform/storage"
	"github.com/anggasct/aura/internal/platform/turn"
)

var textDocumentTypes = []string{"text/markdown", "text/plain", "text/html"}

func IsTextDocument(contentType string) bool {
	mediaType := storage.BaseMediaType(contentType)
	for _, candidate := range textDocumentTypes {
		if mediaType == candidate {
			return true
		}
	}
	return false
}

func Attachable(contentType string) bool {
	return Eligible(contentType) || IsTextDocument(contentType)
}

const (
	genericMediaType = "application/octet-stream"
	textChunkBytes   = 32 << 10
)

var textAliases = []string{"text/x-markdown"}

var textExtensions = map[string]string{
	".md":       "text/markdown",
	".markdown": "text/markdown",
	".txt":      "text/plain",
	".html":     "text/html",
	".htm":      "text/html",
}

type Intake struct {
	MediaType string
	Sniff     bool
}

func NewIntake(filename, declared string) (Intake, bool) {
	base := storage.BaseMediaType(declared)
	if Attachable(base) {
		return Intake{MediaType: base}, true
	}
	if base != "" && base != genericMediaType && !slices.Contains(textAliases, base) {
		return Intake{}, false
	}
	canonical, known := textExtensions[strings.ToLower(path.Ext(SanitizeFilename(filename)))]
	if !known {
		return Intake{}, false
	}
	return Intake{MediaType: canonical, Sniff: true}, true
}

func (i Intake) Served(contentType string) bool {
	served := storage.BaseMediaType(contentType)
	switch {
	case served == "" || served == genericMediaType:
		return true
	case i.Sniff:
		return IsTextDocument(served) || slices.Contains(textAliases, served)
	}
	return Attachable(served)
}

func (i Intake) Verify(content *os.File) error {
	if !i.Sniff {
		return nil
	}
	return requireText(content)
}

func requireText(content *os.File) error {
	if _, err := content.Seek(0, io.SeekStart); err != nil {
		return fault.Wrap(ErrorCodeIncomplete, "content is not rewindable", err)
	}
	buffer := make([]byte, textChunkBytes+utf8.UTFMax)
	carried := 0
	for {
		read, err := content.Read(buffer[carried:])
		window := buffer[:carried+read]
		if bytes.IndexByte(window[carried:], 0) >= 0 {
			return fault.New(ErrorCodeNotText, "content holds a NUL byte")
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return fault.Wrap(ErrorCodeIncomplete, "content did not arrive completely", err)
		}
		valid := len(window)
		if !errors.Is(err, io.EOF) {
			valid = completeRunes(window)
		}
		if !utf8.Valid(window[:valid]) {
			return fault.New(ErrorCodeNotText, "content is not UTF-8")
		}
		carried = copy(buffer, window[valid:])
		if errors.Is(err, io.EOF) {
			break
		}
	}
	if _, err := content.Seek(0, io.SeekStart); err != nil {
		return fault.Wrap(ErrorCodeIncomplete, "content is not rewindable", err)
	}
	return nil
}

func completeRunes(window []byte) int {
	end := len(window)
	for back := 1; back < utf8.UTFMax && back <= end; back++ {
		if utf8.RuneStart(window[end-back]) {
			if !utf8.FullRune(window[end-back:]) {
				return end - back
			}
			break
		}
	}
	return end
}

func ReferencePart(summary *ArtifactSummary, contentType string) string {
	var builder strings.Builder
	builder.WriteString("[attachment ")
	builder.WriteString(turn.QuoteUntrustedName(SanitizeFilename(summary.Filename)))
	builder.WriteByte(' ')
	builder.WriteString(storage.BaseMediaType(contentType))
	builder.WriteByte(' ')
	builder.WriteString(strconv.FormatInt(summary.SizeBytes, 10))
	builder.WriteString(" bytes artifact:")
	builder.WriteString(summary.RefID)
	builder.WriteByte(']')
	return builder.String()
}
