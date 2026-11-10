package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

const signatureVersion = "v1"

func EventSigningBytes(timestamp, nonce string, body []byte) []byte {
	digest := sha256.Sum256(body)
	var b strings.Builder
	b.WriteString(signatureVersion)
	b.WriteByte('\n')
	b.WriteString(timestamp)
	b.WriteByte('\n')
	b.WriteString(nonce)
	b.WriteByte('\n')
	b.WriteString(hex.EncodeToString(digest[:]))
	return []byte(b.String())
}

func StatusSigningBytes(timestamp, nonce, path string) []byte {
	var b strings.Builder
	b.WriteString(signatureVersion)
	b.WriteByte('\n')
	b.WriteString(timestamp)
	b.WriteByte('\n')
	b.WriteString(nonce)
	b.WriteByte('\n')
	b.WriteString("GET")
	b.WriteByte('\n')
	b.WriteString(path)
	return []byte(b.String())
}

func Sign(secret string, payload []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	return signatureVersion + "=" + hex.EncodeToString(mac.Sum(nil))
}

func VerifySignature(header, secret string, payload []byte) bool {
	value, ok := strings.CutPrefix(header, signatureVersion+"=")
	if !ok || len(value) != 2*sha256.Size {
		return false
	}
	if strings.ToLower(value) != value {
		return false
	}
	given := make([]byte, sha256.Size)
	if _, err := hex.Decode(given, []byte(value)); err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	return hmac.Equal(given, mac.Sum(nil))
}
