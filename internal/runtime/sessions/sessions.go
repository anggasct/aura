package runtimesessions

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/anggasct/aura/internal/runtime"
)

const SchemaVersion uint16 = 1

const MaxDedupeEntries = 256

type Part struct {
	Text string `json:"text"`
}

type Descriptor struct {
	TurnID         string    `json:"turn_id"`
	SessionID      string    `json:"session_id"`
	PrincipalID    string    `json:"principal_id"`
	Origin         string    `json:"origin"`
	Parts          []Part    `json:"parts"`
	IdempotencyKey string    `json:"idempotency_key"`
	Deadline       time.Time `json:"deadline,omitempty"`
	MaxTokens      int64     `json:"max_tokens"`
	MaxCost        float64   `json:"max_cost"`
	TraceParent    string    `json:"trace_parent"`
	AgentID        string    `json:"agent_id"`
}

type QueuedTurn struct {
	Descriptor Descriptor `json:"descriptor"`
	Sequence   uint64     `json:"sequence"`
}

type ActiveTurn struct {
	Descriptor Descriptor `json:"descriptor"`
	Sequence   uint64     `json:"sequence"`
}

type State struct {
	SchemaVersion uint16            `json:"schema_version"`
	Queue         []QueuedTurn      `json:"queue"`
	Active        *ActiveTurn       `json:"active,omitempty"`
	Dedupe        map[string]string `json:"dedupe"`
	LastSequence  uint64            `json:"last_sequence"`
}

func NewState() *State {
	return &State{SchemaVersion: SchemaVersion, Dedupe: map[string]string{}}
}

func DecodeState(raw []byte) (*State, error) {
	if len(raw) == 0 {
		return NewState(), nil
	}
	var state State
	if err := json.Unmarshal(raw, &state); err != nil {
		return nil, invalidArgument(fmt.Sprintf("decode session state: %v", err))
	}
	if err := state.Validate(); err != nil {
		return nil, err
	}
	return &state, nil
}

func (s *State) Validate() error {
	if s == nil {
		return invalidArgument("session state must not be nil")
	}
	if s.SchemaVersion != SchemaVersion {
		return invalidArgument(fmt.Sprintf("session state schema version %d is unsupported", s.SchemaVersion))
	}
	if s.Dedupe == nil {
		return invalidArgument("session state dedupe map must not be nil")
	}
	for i := range s.Queue {
		if err := s.Queue[i].Descriptor.Validate(); err != nil {
			return err
		}
		if s.Queue[i].Sequence == 0 {
			return invalidArgument("queued turn sequence must be positive")
		}
	}
	if s.Active != nil {
		if s.Active.Descriptor.TurnID == "" {
			return invalidArgument("active turn id must not be empty")
		}
		if s.Active.Sequence == 0 {
			return invalidArgument("active turn sequence must be positive")
		}
	}
	return nil
}

func (d *Descriptor) Validate() error {
	if d == nil {
		return invalidArgument("turn descriptor must not be nil")
	}
	if d.TurnID == "" {
		return invalidArgument("turn id must not be empty")
	}
	if d.SessionID == "" {
		return invalidArgument("session id must not be empty")
	}
	if d.Origin == "" {
		return invalidArgument("origin must not be empty")
	}
	return nil
}

func invalidArgument(detail string) error {
	return &runtime.Error{Code: runtime.ErrorCodeInvalidArgument, Detail: detail}
}

func overloaded(detail string) error {
	return &runtime.Error{Code: runtime.ErrorCodeRuntimeOverloaded, Detail: detail}
}

func CheckReplay(s *State, idempotencyKey string) (string, error) {
	if err := s.Validate(); err != nil {
		return "", err
	}
	if idempotencyKey == "" {
		return "", nil
	}
	return s.Dedupe[idempotencyKey], nil
}

func NoteReplay(s *State, idempotencyKey, turnID string) error {
	if err := s.Validate(); err != nil {
		return err
	}
	if idempotencyKey == "" || turnID == "" {
		return invalidArgument("replay note requires an idempotency key and a turn id")
	}
	s.Dedupe[idempotencyKey] = turnID
	evictDedupe(s)
	return nil
}

func Enqueue(s *State, d *Descriptor, sequence uint64, maxPending int) (AdmitResult, error) {
	if err := s.Validate(); err != nil {
		return AdmitResult{}, err
	}
	if err := d.Validate(); err != nil {
		return AdmitResult{}, err
	}
	if sequence == 0 {
		return AdmitResult{}, invalidArgument("admit sequence must be positive")
	}
	if sequence <= s.LastSequence {
		return AdmitResult{}, invalidArgument(fmt.Sprintf("admit sequence %d does not advance past %d", sequence, s.LastSequence))
	}
	if maxPending <= 0 {
		return AdmitResult{}, invalidArgument("max pending turns must be positive")
	}
	held := len(s.Queue)
	if s.Active != nil {
		held++
	}
	if held >= maxPending {
		return AdmitResult{}, overloaded("session turn queue is full")
	}
	s.LastSequence = sequence
	entry := QueuedTurn{Descriptor: *d, Sequence: sequence}
	if d.IdempotencyKey != "" {
		s.Dedupe[d.IdempotencyKey] = d.TurnID
		evictDedupe(s)
	}
	s.Queue = append(s.Queue, entry)
	if s.Active != nil {
		return AdmitResult{}, nil
	}
	return AdmitResult{ToStart: s.popLocked()}, nil
}

func Release(s *State, turnID string) (ReleaseResult, error) {
	if err := s.Validate(); err != nil {
		return ReleaseResult{}, err
	}
	if turnID == "" {
		return ReleaseResult{}, invalidArgument("release requires a turn id")
	}
	if s.Active == nil || s.Active.Descriptor.TurnID != turnID {
		return ReleaseResult{}, invalidArgument(fmt.Sprintf("release for turn %q does not match the active turn", turnID))
	}
	s.Active = nil
	if len(s.Queue) == 0 {
		return ReleaseResult{}, nil
	}
	popped := s.popLocked()
	return ReleaseResult{ToStart: popped, QueueDepth: len(s.Queue)}, nil
}

func (s *State) popLocked() *QueuedTurn {
	head := s.Queue[0]
	s.Queue = s.Queue[1:]
	s.Active = &ActiveTurn{Descriptor: head.Descriptor, Sequence: head.Sequence}
	out := head
	return &out
}

func Abort(s *State) ([]QueuedTurn, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	var dropped []QueuedTurn
	if s.Active != nil {
		dropped = append(dropped, QueuedTurn{Descriptor: s.Active.Descriptor, Sequence: s.Active.Sequence})
		s.Active = nil
	}
	dropped = append(dropped, s.Queue...)
	clear(s.Queue)
	s.Queue = nil
	return dropped, nil
}

func evictDedupe(s *State) {
	for len(s.Dedupe) > MaxDedupeEntries {
		for key := range s.Dedupe {
			delete(s.Dedupe, key)
			break
		}
	}
}
