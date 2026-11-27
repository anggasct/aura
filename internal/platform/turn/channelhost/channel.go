package runtimechannelhost

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/anggasct/aura/internal/platform/turn/ingress"
)

var ErrDeliveryAmbiguous = errors.New("channel delivery outcome is unknown")

type OutputPart struct {
	Text string
}

type DeliveryRequest struct {
	EffectID       string
	Channel        string
	ConversationID string
	PrincipalClass string
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

type StepOutcome string

const (
	StepRunning StepOutcome = "running"
	StepDone    StepOutcome = "done"
	StepFailed  StepOutcome = "failed"
)

type PresenceUpdate struct {
	CallIDs  []string
	Activity string
	Outcome  StepOutcome
}

type PresenceEnd struct {
	Stopped bool
}

type PresencePort interface {
	PresenceStarted(ctx context.Context, req *DeliveryRequest) error
	PresenceUpdated(ctx context.Context, req *DeliveryRequest, update PresenceUpdate) error
	PresenceEnded(ctx context.Context, turnID string, end PresenceEnd) error
}

type ChannelIdentity interface {
	ChannelName() string
}
