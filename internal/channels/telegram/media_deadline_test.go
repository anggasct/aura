package telegram

import (
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	runtimeingress "github.com/anggasct/aura/internal/platform/turn/ingress"
)

type stalledBody struct {
	done <-chan struct{}
}

func (b *stalledBody) Read([]byte) (int, error) {
	<-b.done
	return 0, io.ErrUnexpectedEOF
}

func (b *stalledBody) Close() error {
	return nil
}

type scriptedTransport func(req *http.Request) (*http.Response, error)

func (s scriptedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return s(req)
}

func infoResponse() *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"ok":true,"result":{"file_path":"voice/secret-voice-path.oga","file_size":5}}`)),
	}
}

func mediaEnvelope() *runtimeingress.IngressEnvelope {
	return &runtimeingress.IngressEnvelope{
		Source:         "telegram:pocket",
		ConversationID: "main/telegram:pocket/chat:1",
		ReplyContext:   []byte(`{"chat_id":1,"message_id":2}`),
		Parts:          []runtimeingress.InputPart{{Text: "hi"}},
	}
}

func voiceMessage() *message {
	return &message{MessageID: 2, Chat: chat{ID: 1, Type: chatTypePrivate}, Voice: &mediaFile{FileID: "v", FileSize: 5, MimeType: "audio/ogg"}}
}

func TestStalledTransfersEndAtTheMediaDeadline(t *testing.T) {
	cases := []struct {
		name  string
		stall func(req *http.Request) (*http.Response, error)
	}{
		{
			name: "getFile never answers",
			stall: func(req *http.Request) (*http.Response, error) {
				<-req.Context().Done()
				return nil, req.Context().Err()
			},
		},
		{
			name: "download body never arrives",
			stall: func(req *http.Request) (*http.Response, error) {
				if strings.HasSuffix(req.URL.Path, "/getFile") {
					return infoResponse(), nil
				}
				return &http.Response{
					StatusCode:    http.StatusOK,
					Header:        http.Header{"Content-Type": []string{"audio/ogg"}},
					ContentLength: -1,
					Body:          &stalledBody{done: req.Context().Done()},
				}, nil
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newMediaRig(t, accessOpts{})
			rig.adapter.bot.http = &http.Client{Transport: scriptedTransport(tc.stall)}
			synctest.Test(t, func(t *testing.T) {
				started := time.Now()
				err := rig.adapter.intakeMedia(t.Context(), voiceMessage(), mediaEnvelope())
				if err == nil {
					t.Fatal("intakeMedia() succeeded on a stalled transfer")
				}
				if elapsed := time.Since(started); elapsed != defaultMediaTimeout {
					t.Errorf("gave up after %v, want exactly %v", elapsed, defaultMediaTimeout)
				}
				for _, secret := range []string{testToken, "secret-voice-path"} {
					if strings.Contains(err.Error(), secret) {
						t.Errorf("error carries %q: %v", secret, err)
					}
				}
			})
			if entries, err := os.ReadDir(rig.scratch); err != nil || len(entries) != 0 {
				t.Errorf("temp files left behind: %v (%v)", entries, err)
			}
			if got := len(rig.artifacts.stored()); got != 0 {
				t.Errorf("stored %d artifacts, want none", got)
			}
		})
	}
}

func TestTransportFailuresNeverExposeTheTokenOrTheFilePath(t *testing.T) {
	rig := newMediaRig(t, accessOpts{})
	rig.adapter.bot.http = &http.Client{Transport: scriptedTransport(func(req *http.Request) (*http.Response, error) {
		if strings.HasSuffix(req.URL.Path, "/getFile") {
			return infoResponse(), nil
		}
		return nil, &url.Error{Op: "Get", URL: req.URL.String(), Err: errors.New("reset while fetching " + req.URL.String())}
	})}
	err := rig.adapter.intakeMedia(t.Context(), voiceMessage(), mediaEnvelope())
	if err == nil {
		t.Fatal("intakeMedia() succeeded on a failing transport")
	}
	for _, secret := range []string{testToken, "secret-voice-path"} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("error carries %q: %v", secret, err)
		}
	}
	if code, ok := CodeOf(err); !ok || code != ErrorCodeConnectionFailed {
		t.Errorf("error code = %q, want %q", code, ErrorCodeConnectionFailed)
	}
}
