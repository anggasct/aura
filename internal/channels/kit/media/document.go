package media

import (
	"strconv"
	"strings"
)

var textDocumentTypes = []string{"text/markdown", "text/plain", "text/html"}

func IsTextDocument(contentType string) bool {
	mediaType := BaseType(contentType)
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

func ReferencePart(summary *ArtifactSummary, contentType string) string {
	var builder strings.Builder
	builder.WriteString("[attachment ")
	builder.WriteString(strconv.Quote(SanitizeFilename(summary.Filename)))
	builder.WriteByte(' ')
	builder.WriteString(BaseType(contentType))
	builder.WriteByte(' ')
	builder.WriteString(strconv.FormatInt(summary.SizeBytes, 10))
	builder.WriteString(" bytes artifact:")
	builder.WriteString(summary.RefID)
	builder.WriteByte(']')
	return builder.String()
}
