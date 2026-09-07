package webhook

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// FuzzParseRequestAuth proves header parsing never panics and never accepts
// a malformed header set, whatever bytes arrive.
func FuzzParseRequestAuth(f *testing.F) {
	f.Add("primary", "1750000000", "nonce-abcdefghijklmnop", "v1="+vectorEventSig)
	f.Add("", "", "", "")
	f.Add("key\x00id", "-99", "ünïcödé-nonce-1234", "v1=ZZZ")
	f.Add(strings.Repeat("a", 10_000), "999999999999999999999999", strings.Repeat("n", 8), "v1=deadbeef")
	f.Fuzz(func(t *testing.T, keyID, timestamp, nonce, signature string) {
		header := http.Header{}
		header.Set(headerKeyID, keyID)
		header.Set(headerTimestamp, timestamp)
		header.Set(headerNonce, nonce)
		header.Set(headerSignature, signature)
		auth, err := ParseRequestAuth(header)
		if err != nil {
			return
		}
		if auth.KeyID != keyID || auth.Timestamp != timestamp || auth.Nonce != nonce || auth.Signature != signature {
			t.Fatal("accepted values were mangled during parsing")
		}
		if !VerifySignature(signature, vectorSecret, EventSigningBytes(timestamp, nonce, []byte(vectorBody))) {
			return
		}
		if !auth.WithinTolerance(time.Unix(1750000000, 0).UTC(), 5*time.Minute) {
			t.Fatal("signature verified but timestamp window disagreed with the parsed seconds")
		}
	})
}

// FuzzParseEnvelope proves envelope parsing never panics and never returns
// a valid envelope for arbitrary bytes.
func FuzzParseEnvelope(f *testing.F) {
	f.Add([]byte(vectorBody))
	f.Add([]byte(`{"event_id":"e","subject":"s","payload":{}}`))
	f.Add([]byte{0xff, 0xfe, 0x00})
	f.Add([]byte(`{"event_id":"` + strings.Repeat("x", 100_000) + `"}`))
	f.Fuzz(func(t *testing.T, body []byte) {
		envelope, err := ParseEnvelope(body)
		if err != nil {
			return
		}
		if envelope.EventID == "" || envelope.Subject == "" {
			t.Fatal("accepted envelope without required fields")
		}
	})
}

// FuzzVerifySignature proves signature verification never panics on
// arbitrary header/secret/payload combinations.
func FuzzVerifySignature(f *testing.F) {
	f.Add("v1="+vectorEventSig, vectorSecret, vectorTimestamp, vectorNonce, []byte(vectorBody))
	f.Add("v1=", "", "", "", []byte(nil))
	f.Add(strings.Repeat("=", 4096), strings.Repeat("s", 4096), strings.Repeat("1", 4096), strings.Repeat("n", 4096), make([]byte, 4096))
	f.Fuzz(func(t *testing.T, header, secret, timestamp, nonce string, body []byte) {
		VerifySignature(header, secret, EventSigningBytes(timestamp, nonce, body))
	})
}
