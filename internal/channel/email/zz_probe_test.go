package email

import (
	"crypto/tls"
	"errors"
	"fmt"
	"testing"
)

func TestZZProbe(t *testing.T) {
	authority := newAuthority(t)
	server := newFakeSMTP(t, authority)
	server.script(smtpBehavior{handshake: "alert"})
	mailer := newTestMailer(t, server, authority)
	err := mailer.Submit(t.Context(), wireSubmission(t))
	code, ok := CodeOf(err)
	fmt.Printf("PROBE code=%q ok=%v err=%v\n", code, ok, err)
	var alert *tls.AlertError
	var record *tls.RecordHeaderError
	fmt.Printf("PROBE asAlert=%v asRecord=%v\n", errors.As(err, &alert), errors.As(err, &record))
}
