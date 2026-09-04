package webhook

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"time"
)

const (
	// Header names are part of the frozen wire contract.
	headerKeyID     = "X-Aura-Key-ID"
	headerTimestamp = "X-Aura-Timestamp"
	headerNonce     = "X-Aura-Nonce"
	headerSignature = "X-Aura-Signature"

	minNonceChars = 16
	maxNonceChars = 128
	maxKeyIDChars = 128
)

var (
	urlSafePattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	digitsPattern  = regexp.MustCompile(`^-?\d+$`)
)

// RequestAuth carries the parsed authentication headers of one request. The
// timestamp keeps its exact wire form: it participates in the signing bytes
// verbatim, so it is never normalized before verification.
type RequestAuth struct {
	KeyID     string
	Timestamp string
	Seconds   int64
	Nonce     string
	Signature string
}

// ParseRequestAuth validates header syntax only. Cryptographic checks happen
// after, against the canonical signing bytes, so a syntactically valid but
// forged header set fails exactly like a tampered one.
func ParseRequestAuth(header http.Header) (RequestAuth, error) {
	auth := RequestAuth{
		KeyID:     header.Get(headerKeyID),
		Timestamp: header.Get(headerTimestamp),
		Nonce:     header.Get(headerNonce),
		Signature: header.Get(headerSignature),
	}
	var problems []error
	if len(auth.KeyID) < 1 || len(auth.KeyID) > maxKeyIDChars || !urlSafePattern.MatchString(auth.KeyID) {
		problems = append(problems, fmt.Errorf("%s must be 1-%d URL-safe ASCII characters", headerKeyID, maxKeyIDChars))
	}
	if !digitsPattern.MatchString(auth.Timestamp) {
		problems = append(problems, fmt.Errorf("%s must be decimal Unix seconds", headerTimestamp))
	} else if seconds, err := strconv.ParseInt(auth.Timestamp, 10, 64); err != nil {
		problems = append(problems, fmt.Errorf("%s is out of range", headerTimestamp))
	} else {
		auth.Seconds = seconds
	}
	if len(auth.Nonce) < minNonceChars || len(auth.Nonce) > maxNonceChars || !urlSafePattern.MatchString(auth.Nonce) {
		problems = append(problems, fmt.Errorf("%s must be %d-%d URL-safe ASCII characters", headerNonce, minNonceChars, maxNonceChars))
	}
	if !isV1Signature(auth.Signature) {
		problems = append(problems, fmt.Errorf("%s must be v1=<lowercase hex HMAC-SHA256>", headerSignature))
	}
	if len(problems) > 0 {
		return RequestAuth{}, Errorf(ErrorCodeInvalidRequest, "%s", errors.Join(problems...))
	}
	return auth, nil
}

// WithinTolerance checks the parsed timestamp against now. A stale or
// far-future timestamp fails authentication, not request parsing, so it is
// indistinguishable from a bad signature to callers.
func (a RequestAuth) WithinTolerance(now time.Time, tolerance time.Duration) bool {
	delta := now.Sub(time.Unix(a.Seconds, 0).UTC())
	if delta < 0 {
		delta = -delta
	}
	return delta <= tolerance
}

func isV1Signature(value string) bool {
	if len(value) != 3+64 {
		return false
	}
	prefix, hexPart := value[:3], value[3:]
	if prefix != "v1=" {
		return false
	}
	for i := range hexPart {
		if !isLowerHex(hexPart[i]) {
			return false
		}
	}
	return true
}

func isLowerHex(b byte) bool {
	return (b >= '0' && b <= '9') || (b >= 'a' && b <= 'f')
}
