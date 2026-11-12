package discord

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/anggasct/aura/internal/platform/effect"
	runtimechannelhost "github.com/anggasct/aura/internal/platform/turn/channelhost"
)

type startedPost struct {
	req       *runtimechannelhost.DeliveryRequest
	channelID string
}

func (f *fakeEffectRunner) seedStartedPost(t *testing.T, post startedPost) {
	t.Helper()
	text := post.req.Parts[0].Text
	request, err := marshalOperation(SendOperation{ChannelID: post.channelID, Text: text})
	if err != nil {
		t.Fatalf("marshal the send: %v", err)
	}
	digest := sha256.Sum256(request)
	key := "discord|" + OpSendMessage + "|" + idempotencyKey(keyBase(post.req), post.channelID, "c0", text)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.intents[key] = &fakeIntent{state: effect.StateStarted, digest: string(digest[:])}
}

func (f *fakeEffectRunner) invocationCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.invocations
}

func TestDeliver_StartedSendIsAmbiguous(t *testing.T) {
	stub := newRestStub(t, func(call int, w http.ResponseWriter, r *http.Request) {
		writeMessage(t, w, "m1")
	})
	adapter, doubles := newSendTestAdapter(t, stub, nil)
	req := deliverTestRequest("turn-started", "discord", "possibly sent")
	req.ReplyContext = json.RawMessage(`{"channel_id":"333","thread_id":"777006","message_id":"999"}`)
	doubles.effects.seedStartedPost(t, startedPost{req: req, channelID: "777006"})

	_, err := adapter.Deliver(t.Context(), req)
	if code, ok := CodeOf(err); !ok || code != ErrorCodeDeliveryAmbiguous {
		t.Fatalf("Deliver error = %v, want %s", err, ErrorCodeDeliveryAmbiguous)
	}
	if !errors.Is(err, runtimechannelhost.ErrDeliveryAmbiguous) {
		t.Errorf("Deliver error = %v, want it to fulfil the port's ambiguous contract", err)
	}
	if refused := Errorf(ErrorCodeDeliveryFailed, "delivery settled without success"); errors.Is(refused, runtimechannelhost.ErrDeliveryAmbiguous) {
		t.Errorf("%v fulfils the port's ambiguous contract, want a definitive failure", refused)
	}
	if posts := stub.postCount(); posts != 0 {
		t.Fatalf("posts = %d, want none into the thread or its parent channel", posts)
	}
	if got := doubles.effects.invocationCount(); got != 0 {
		t.Errorf("provider invocations = %d, want none", got)
	}
}
