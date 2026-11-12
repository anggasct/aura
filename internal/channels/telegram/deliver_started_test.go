package telegram

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/anggasct/aura/internal/platform/effect"
	runtimechannelhost "github.com/anggasct/aura/internal/platform/turn/channelhost"
)

func seedStartedSend(t *testing.T, h *harness, req *runtimechannelhost.DeliveryRequest) {
	t.Helper()
	text := req.Parts[0].Text
	request, err := json.Marshal(&sendOperation{ChatID: ownerUserID, Text: text, HTML: renderHTML(text), ReplyTo: 100})
	if err != nil {
		t.Fatalf("marshal the send: %v", err)
	}
	journal := h.effects.Journal()
	intent, err := journal.Prepare(t.Context(), &effect.PrepareRequest{
		SessionID:      req.ConversationID,
		IdempotencyKey: idempotencyKey(keyBase(req), ownerUserID, 0, text),
		Provider:       ProviderName,
		Operation:      OpSendMessage,
		Classification: effect.ClassificationEffectful,
		Request:        request,
		EventKind:      effect.EventKindChannelRequested,
	})
	if err != nil {
		t.Fatalf("prepare the send: %v", err)
	}
	if _, err := journal.Start(t.Context(), intent.ID); err != nil {
		t.Fatalf("start the send: %v", err)
	}
}

func TestDeliverStartedSendIsAmbiguous(t *testing.T) {
	h := newHarness(t)
	adapter := h.adapter(t, nil)
	req := deliveryRequest(t, h, "turn-started", "possibly sent")
	seedStartedSend(t, h, req)

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
	if got := len(h.api.sends()); got != 0 {
		t.Fatalf("sendMessage calls = %d, want none", got)
	}
	if states := intentStates(t, h); states[effect.StateStarted] != 1 || len(states) != 1 {
		t.Errorf("intent states = %v, want the one started intent", states)
	}
}
