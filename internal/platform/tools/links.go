package tools

import (
	"strings"
	"unicode"

	"golang.org/x/net/idna"
)

type LinkProvenance string

const (
	LinkProvenanceNone         LinkProvenance = "none"
	LinkProvenanceOwnerMessage LinkProvenance = "owner_message"
	LinkProvenanceFetched      LinkProvenance = "fetched"
	LinkProvenanceSearchResult LinkProvenance = "search_result"
)

func ValidLinkProvenance(p LinkProvenance) bool {
	switch p {
	case LinkProvenanceNone, LinkProvenanceOwnerMessage, LinkProvenanceFetched, LinkProvenanceSearchResult:
		return true
	default:
		return false
	}
}

type ResultBatch struct {
	CallID  string
	Queries []string
	URLs    []string
}

const (
	MaxTrackedURLs   = 4096
	MaxMatchURLBytes = 2048
)

const (
	urlStopChars     = "<>\"`"
	urlTrailingTrim  = ".,;:!?)]}'"
	schemeHTTP       = "http"
	schemeHTTPS      = "https"
	defaultPortHTTP  = "80"
	defaultPortHTTPS = "443"
)

func ExtractURLs(text string) []string {
	var found []string
	lower := strings.ToLower(text)
	pos := 0
	for pos < len(text) {
		idx := nextSchemeIndex(lower[pos:])
		if idx < 0 {
			break
		}
		start := pos + idx
		end := start
		for end < len(text) {
			if isURLStopByte(text[end]) {
				break
			}
			end++
		}
		raw := strings.TrimRight(text[start:end], urlTrailingTrim)
		if canonical, ok := CanonicalURL(raw); ok {
			found = append(found, canonical)
		}
		pos = max(end, start+1)
	}
	return found
}

func nextSchemeIndex(lower string) int {
	httpIdx := strings.Index(lower, "http://")
	httpsIdx := strings.Index(lower, "https://")
	switch {
	case httpIdx < 0:
		return httpsIdx
	case httpsIdx < 0:
		return httpIdx
	case httpIdx < httpsIdx:
		return httpIdx
	default:
		return httpsIdx
	}
}

func isURLStopByte(b byte) bool {
	if b <= 0x20 || b == 0x7f {
		return true
	}
	return strings.IndexByte(urlStopChars, b) >= 0
}

func CanonicalURL(raw string) (string, bool) {
	if raw == "" || len(raw) > MaxMatchURLBytes {
		return "", false
	}
	for _, r := range raw {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return "", false
		}
	}
	schemeRaw, rest, ok := strings.Cut(raw, "://")
	if !ok {
		return "", false
	}
	scheme := strings.ToLower(schemeRaw)
	if scheme != schemeHTTP && scheme != schemeHTTPS {
		return "", false
	}
	if fragIdx := strings.IndexByte(rest, '#'); fragIdx >= 0 {
		rest = rest[:fragIdx]
	}
	authorityEnd := strings.IndexAny(rest, "/?")
	authority := rest
	tail := ""
	if authorityEnd >= 0 {
		authority = rest[:authorityEnd]
		tail = rest[authorityEnd:]
	}
	if authority == "" || strings.Contains(authority, "@") {
		return "", false
	}
	host, port, ok := splitCanonicalAuthority(authority, scheme)
	if !ok {
		return "", false
	}
	switch {
	case tail == "":
		tail = "/"
	case tail[0] == '?':
		tail = "/" + tail
	}
	if port != "" {
		return scheme + "://" + host + ":" + port + tail, true
	}
	return scheme + "://" + host + tail, true
}

func splitCanonicalAuthority(authority, scheme string) (host, port string, ok bool) {
	if strings.HasPrefix(authority, "[") {
		closeIdx := strings.IndexByte(authority, ']')
		if closeIdx <= 1 {
			return "", "", false
		}
		host = strings.ToLower(authority[:closeIdx+1])
		rem := authority[closeIdx+1:]
		if rem != "" {
			if rem[0] != ':' || !allDigits(rem[1:]) {
				return "", "", false
			}
			port = rem[1:]
		}
	} else {
		hostPart := authority
		if colon := strings.LastIndexByte(authority, ':'); colon >= 0 {
			hostPart = authority[:colon]
			port = authority[colon+1:]
			if !allDigits(port) {
				return "", "", false
			}
		}
		if hostPart == "" {
			return "", "", false
		}
		asciiHost, err := idna.Lookup.ToASCII(strings.ToLower(hostPart))
		if err != nil || asciiHost == "" {
			return "", "", false
		}
		host = strings.ToLower(asciiHost)
	}
	if (scheme == schemeHTTP && port == defaultPortHTTP) || (scheme == schemeHTTPS && port == defaultPortHTTPS) {
		port = ""
	}
	return host, port, true
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
