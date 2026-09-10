package runtimesessions

import "encoding/json"

type AdmitRequest struct {
	Turn            Descriptor `json:"turn"`
	AcceptedEventID string     `json:"accepted_event_id"`
	MaxPending      int        `json:"max_pending"`
}

type AdmitResult struct {
	Replayed       bool            `json:"replayed"`
	OriginalTurnID string          `json:"original_turn_id"`
	Overloaded     bool            `json:"overloaded"`
	Sequence       uint64          `json:"sequence"`
	EventPayload   json.RawMessage `json:"event_payload"`
	EventCreatedAt string          `json:"event_created_at"`
	ToStart        *QueuedTurn     `json:"to_start,omitempty"`
}

type ReleaseRequest struct {
	TurnID string `json:"turn_id"`
}

type ReleaseResult struct {
	ToStart    *QueuedTurn `json:"to_start,omitempty"`
	QueueDepth int         `json:"queue_depth"`
}

type RecoverRequest struct {
	Open     []string `json:"open"`
	Terminal []string `json:"terminal"`
}

type RecoverResult struct {
	ToStart    *QueuedTurn `json:"to_start,omitempty"`
	Found      bool        `json:"found"`
	QueueDepth int         `json:"queue_depth"`
}

type AbortResult struct {
	Dropped []QueuedTurn `json:"dropped"`
}

type StatusResult struct {
	QueueDepth   int    `json:"queue_depth"`
	ActiveTurnID string `json:"active_turn_id"`
	LastSequence uint64 `json:"last_sequence"`
	Deadline     string `json:"deadline,omitempty"`
}
