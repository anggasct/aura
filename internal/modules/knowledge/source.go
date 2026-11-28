package knowledge

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/anggasct/aura/internal/platform/fault"
)

const (
	MaxSourceBytes         = 8 << 20
	MaxChunksPerSource     = 8000
	MaxChunksPerCollection = 100000
	MaxChunksTotal         = 500000
	IngestVersion          = 1

	maxLabelRunes  = 120
	maxURIBytes    = 2048
	fetchTimeout   = 30 * time.Second
	acceptHeader   = "text/html, text/markdown, text/plain"
	fallbackLabel  = "source"
	labelSeparator = '-'
)

type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

type loadedSource struct {
	kind   string
	uri    string
	label  string
	format Format
	data   []byte
}

func fileFormat(name string) (Format, bool) {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".md":
		return FormatMarkdown, true
	case ".txt":
		return FormatText, true
	case ".html":
		return FormatHTML, true
	}
	return "", false
}

func loadStream(stream *Stream) (*loadedSource, error) {
	format, err := streamFormat(stream.Name, stream.MediaType)
	if err != nil {
		return nil, err
	}
	if stream.Size > MaxSourceBytes {
		return nil, Errorf(ErrorCodeSourceTooLarge, "source exceeds %d bytes", MaxSourceBytes)
	}
	data, err := readBounded(stream.Reader)
	if err != nil {
		return nil, err
	}
	if err := requireText(data); err != nil {
		return nil, err
	}
	return &loadedSource{
		kind:   SourceKindFile,
		uri:    stream.URI,
		label:  sanitizeLabel(path.Base(stream.Name)),
		format: format,
		data:   data,
	}, nil
}

func streamFormat(name, mediaType string) (Format, error) {
	if mediaType == "" {
		format, ok := fileFormat(name)
		if !ok {
			return "", Errorf(ErrorCodeSourceUnsupported, "only .md, .txt and .html files are supported")
		}
		return format, nil
	}
	switch mediaType {
	case "text/html":
		return FormatHTML, nil
	case "text/markdown":
		return FormatMarkdown, nil
	case "text/plain":
		if strings.EqualFold(path.Ext(name), ".md") {
			return FormatMarkdown, nil
		}
		return FormatText, nil
	}
	return "", Errorf(ErrorCodeSourceUnsupported, "only Markdown, plain text and HTML attachments are supported")
}

func readBounded(reader io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, MaxSourceBytes+1))
	if err != nil {
		return nil, Errorf(ErrorCodeFetchFailed, "source could not be read")
	}
	if len(data) > MaxSourceBytes {
		return nil, Errorf(ErrorCodeSourceTooLarge, "source exceeds %d bytes", MaxSourceBytes)
	}
	return data, nil
}

func requireText(data []byte) error {
	if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return Errorf(ErrorCodeSourceNotUTF8, "source is not UTF-8 text")
	}
	return nil
}

func parseSourceURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" || parsed.Hostname() == "" {
		return nil, Errorf(ErrorCodeURLRefused, "source is not a valid URL")
	}
	if !strings.EqualFold(parsed.Scheme, "https") {
		return nil, Errorf(ErrorCodeURLRefused, "only https URLs are accepted")
	}
	if parsed.User != nil {
		return nil, Errorf(ErrorCodeURLRefused, "URLs with credentials are refused")
	}
	parsed.Scheme = "https"
	parsed.Host = strings.ToLower(parsed.Host)
	parsed.Fragment = ""
	parsed.RawFragment = ""
	if len(parsed.String()) > maxURIBytes {
		return nil, Errorf(ErrorCodeURLRefused, "URL exceeds %d bytes", maxURIBytes)
	}
	return parsed, nil
}

func loadURL(ctx context.Context, doer HTTPDoer, raw string) (*loadedSource, error) {
	parsed, err := parseSourceURL(raw)
	if err != nil {
		return nil, err
	}
	fetchCtx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, parsed.String(), http.NoBody)
	if err != nil {
		return nil, Errorf(ErrorCodeURLRefused, "source is not a valid URL")
	}
	request.Header.Set("Accept", acceptHeader)
	response, err := doer.Do(request)
	if err != nil {
		return nil, classifyFetchError(ctx, err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, Errorf(ErrorCodeFetchFailed, "server answered with status %d", response.StatusCode)
	}
	if response.ContentLength > MaxSourceBytes {
		return nil, Errorf(ErrorCodeSourceTooLarge, "source exceeds %d bytes", MaxSourceBytes)
	}
	if encoding := response.Header.Get("Content-Encoding"); encoding != "" && !strings.EqualFold(encoding, "identity") {
		return nil, Errorf(ErrorCodeSourceUnsupported, "compressed responses are not accepted")
	}
	format, err := urlFormat(response.Header.Get("Content-Type"), parsed.Path)
	if err != nil {
		return nil, err
	}
	data, err := readBounded(response.Body)
	if err != nil {
		return nil, classifyFetchError(ctx, err)
	}
	if err := requireText(data); err != nil {
		return nil, err
	}
	return &loadedSource{
		kind:   SourceKindURL,
		uri:    parsed.String(),
		label:  sanitizeLabel(parsed.Hostname() + parsed.Path),
		format: format,
		data:   data,
	}, nil
}

func classifyFetchError(ctx context.Context, err error) error {
	var coded *fault.Error[ErrorCode]
	if errors.As(err, &coded) {
		return fault.New(coded.Code, coded.Detail)
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	return Errorf(ErrorCodeFetchFailed, "request failed")
}

func urlFormat(contentType, urlPath string) (Format, error) {
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return "", Errorf(ErrorCodeSourceUnsupported, "response content type is missing or invalid")
	}
	if charset := strings.ToLower(params["charset"]); charset != "" && charset != "utf-8" && charset != "us-ascii" {
		return "", Errorf(ErrorCodeSourceNotUTF8, "response is not declared as UTF-8")
	}
	switch mediaType {
	case "text/html", "application/xhtml+xml":
		return FormatHTML, nil
	case "text/markdown", "text/x-markdown":
		return FormatMarkdown, nil
	case "text/plain":
		if strings.EqualFold(path.Ext(urlPath), ".md") {
			return FormatMarkdown, nil
		}
		return FormatText, nil
	}
	return "", Errorf(ErrorCodeSourceUnsupported, "response content type %q is not supported", mediaType)
}

func sanitizeLabel(raw string) string {
	raw = Normalize(raw, FormatText)
	var out strings.Builder
	pendingSeparator := false
	for _, r := range raw {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '.' || r == '_' || r == '/' || r == labelSeparator {
			if pendingSeparator && out.Len() > 0 {
				out.WriteRune(labelSeparator)
			}
			pendingSeparator = false
			out.WriteRune(r)
			continue
		}
		pendingSeparator = true
	}
	label := out.String()
	if utf8.RuneCountInString(label) > maxLabelRunes {
		label = string([]rune(label)[:maxLabelRunes])
	}
	if label == "" {
		return fallbackLabel
	}
	return label
}
