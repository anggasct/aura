package runtimechannelhost

import (
	"context"
	"encoding/json"
	"time"

	"github.com/anggasct/aura/internal/runtime/ingress"
)

type OutputPart struct {
	Text string
}

type DeliveryRequest struct {
	EffectID       string
	Channel        string
	ConversationID string
	ReplyContext   json.RawMessage
	Parts          []OutputPart
	IdempotencyKey string
	Deadline       time.Time
}

type ProviderReceipt struct {
	ProviderID string
	ExternalID string
	At         time.Time
}

type HealthStatus string

const (
	ChannelHealthy  HealthStatus = "healthy"
	ChannelDegraded HealthStatus = "degraded"
	ChannelDown     HealthStatus = "down"
)

type ChannelHealth struct {
	Status HealthStatus
	Detail string
}

type ChannelPort interface {
	Start(ctx context.Context, sink runtimeingress.IngressSink) error
	Deliver(ctx context.Context, req *DeliveryRequest) (ProviderReceipt, error)
	Health(ctx context.Context) ChannelHealth
}
