package runtimechannelhost

import (
	"context"
	"encoding/json"
	"time"

	"github.com/anggasct/aura/internal/runtime/ingress"
)

// OutputPart is one normalized piece of outbound delivery. The runtime maps
// turn output onto these parts; the channel adapter maps them onto its wire
// format.
type OutputPart struct {
	Text string
}

// DeliveryRequest is one outbound send the runtime asks a channel adapter to
// perform. It is an effect intent: the adapter reports a provider receipt on
// known success and an ambiguous outcome otherwise, never a blind retry.
type DeliveryRequest struct {
	EffectID       string
	Channel        string
	ConversationID string
	ReplyContext   json.RawMessage
	Parts          []OutputPart
	IdempotencyKey string
	Deadline       time.Time
}

// ProviderReceipt is the channel provider's acknowledgement of a delivery.
type ProviderReceipt struct {
	ProviderID string
	ExternalID string
	At         time.Time
}

// HealthStatus is a channel adapter's coarse readiness state.
type HealthStatus string

const (
	ChannelHealthy  HealthStatus = "healthy"
	ChannelDegraded HealthStatus = "degraded"
	ChannelDown     HealthStatus = "down"
)

// ChannelHealth reports an adapter's readiness and an operator-facing detail.
type ChannelHealth struct {
	Status HealthStatus
	Detail string
}

// ChannelPort is the contract a gateway adapter satisfies. Start receives the
// runtime as an IngressSink — the adapter's only way to run work — and blocks
// until the context is cancelled at shutdown. Deliver and Health are the
// outbound and readiness edges. An adapter never imports model or tool
// implementations and never builds its own turn loop.
type ChannelPort interface {
	Start(ctx context.Context, sink runtimeingress.IngressSink) error
	Deliver(ctx context.Context, req *DeliveryRequest) (ProviderReceipt, error)
	Health(ctx context.Context) ChannelHealth
}
