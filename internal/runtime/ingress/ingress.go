package runtimeingress

import (
	"context"
	"encoding/json"
	"time"
)

type InputPart struct {
	Text  string
	Image json.RawMessage `json:"image,omitempty"`
}

type IngressEnvelope struct {
	Source         string
	AgentID        string
	ExternalID     string
	PrincipalID    string
	PrincipalClass string
	ConversationID string
	ReplyContext   json.RawMessage
	Parts          []InputPart
	ReceivedAt     time.Time
	TraceParent    string
}

// SessionKey scopes a channel conversation to one persona and one account, so two personas serving the same chat never share a session.
func SessionKey(agentID, source, conversationID string) string {
	return agentID + "/" + source + "/" + conversationID
}

type TurnRef struct {
	TurnID    string
	SessionID string
	Replayed  bool
}

type IngressSink interface {
	Accept(ctx context.Context, env *IngressEnvelope) (TurnRef, error)
}
