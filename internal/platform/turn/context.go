package turn

import "context"

const (
	ContextOrderRecall    = 100
	ContextOrderProfile   = 200
	ContextOrderSkills    = 300
	ContextOrderKnowledge = 400
)

type ContextPlacement int

const (
	ContextPlacementEphemeral ContextPlacement = iota
	ContextPlacementUserPart
)

type ContextBlock struct {
	Order     int
	Placement ContextPlacement
	Content   string
	Caveat    string
	Events    []Event
}

type ContextProvider interface {
	TurnContext(ctx context.Context, req *TurnRequest) ([]ContextBlock, error)
	Required(req *TurnRequest) bool
}
