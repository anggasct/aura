package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"time"

	"github.com/anggasct/aura/internal/store"

	"google.golang.org/adk/v2/session"
)

// maxEventLoad is the batch size for loading a session's full event log; a
// session with more events loads in one query since the store rejects a
// zero limit as "nothing".
const maxEventLoad = 1 << 20

// ADKSessionService adapts the Aura session/event ports to the ADK session
// storage interface, so the ADK runner's event state is the Aura runtime
// event log — never a message projection. ADK concrete types stay in this
// adapter; the rest of the runtime sees only store ports.
type ADKSessionService struct {
	sessions SessionPort
	events   EventStore
}

// SessionPort is the Aura session lifecycle the adapter maps onto.
type SessionPort interface {
	Create(ctx context.Context, sess *store.Session) error
	Get(ctx context.Context, sessionID string) (store.Session, error)
	ListEvents(ctx context.Context, sessionID string, afterSequence uint64, limit int) ([]store.RuntimeEvent, error)
}

// NewADKSessionService wraps the Aura session and event stores for ADK.
func NewADKSessionService(sessions SessionPort, events EventStore) (*ADKSessionService, error) {
	if sessions == nil {
		return nil, invalidArgument("session port must not be nil")
	}
	if events == nil {
		return nil, invalidArgument("event store must not be nil")
	}
	return &ADKSessionService{sessions: sessions, events: events}, nil
}

// adkSession is the ADK session view over a stored Aura session. Events are
// lazily read from the store on first access.
type adkSession struct {
	id       string
	userID   string
	metadata json.RawMessage
	events   func() ([]*session.Event, error)
}

func (s *adkSession) ID() string                { return s.id }
func (s *adkSession) AppName() string           { return "" }
func (s *adkSession) UserID() string            { return s.userID }
func (s *adkSession) LastUpdateTime() time.Time { return time.Time{} }

func (s *adkSession) State() session.State {
	values := map[string]any{}
	if len(s.metadata) > 0 {
		_ = json.Unmarshal(s.metadata, &values)
	}
	return adkState{values: values}
}

func (s *adkSession) Events() session.Events {
	return &adkEvents{load: s.events}
}

// adkState is an immutable snapshot of the session metadata viewed as ADK
// state. ADK writes state through EventActions.StateDelta; those deltas are
// stored in event payloads, so this view is read-only by construction.
type adkState struct {
	values map[string]any
}

func (s adkState) Get(key string) (any, error) {
	v, ok := s.values[key]
	if !ok {
		return nil, session.ErrStateKeyNotExist
	}
	return v, nil
}

func (s adkState) Set(key string, value any) error {
	return errors.New("adk state is read-only; state delta belongs in the event")
}

func (s adkState) All() iter.Seq2[string, any] {
	return func(yield func(string, any) bool) {
		for k, v := range s.values {
			if !yield(k, v) {
				return
			}
		}
	}
}

// adkEvents lazily loads the session's stored events in sequence order.
type adkEvents struct {
	load  func() ([]*session.Event, error)
	cache []*session.Event
	err   error
}

func (e *adkEvents) loadOnce() {
	if e.cache != nil || e.err != nil {
		return
	}
	e.cache, e.err = e.load()
}

func (e *adkEvents) All() iter.Seq[*session.Event] {
	return func(yield func(*session.Event) bool) {
		e.loadOnce()
		if e.err != nil {
			return
		}
		for _, ev := range e.cache {
			if !yield(ev) {
				return
			}
		}
	}
}

func (e *adkEvents) Len() int {
	e.loadOnce()
	return len(e.cache)
}

func (e *adkEvents) At(i int) *session.Event {
	e.loadOnce()
	if i < 0 || i >= len(e.cache) {
		return nil
	}
	return e.cache[i]
}

// Create stores a new Aura session, mapping ADK state into the metadata
// column. A client-supplied session ID is honored; otherwise one is
// generated, mirroring ADK's autogenerate behavior.
func (s *ADKSessionService) Create(ctx context.Context, req *session.CreateRequest) (*session.CreateResponse, error) {
	if req == nil {
		return nil, invalidArgument("create request must not be nil")
	}
	if req.UserID == "" {
		return nil, invalidArgument("user id must not be empty")
	}
	id := req.SessionID
	if id == "" {
		id = newTurnID()
	}
	metadata, err := json.Marshal(req.State)
	if err != nil {
		return nil, fmt.Errorf("marshal adk session state: %w", err)
	}
	if metadata == nil {
		metadata = []byte("{}")
	}
	now := time.Now().UTC()
	stored := &store.Session{
		ID:        id,
		OwnerID:   req.UserID,
		Metadata:  metadata,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.sessions.Create(ctx, stored); err != nil {
		return nil, err
	}
	return &session.CreateResponse{
		Session: &adkSession{
			id:       id,
			userID:   req.UserID,
			metadata: metadata,
			events:   func() ([]*session.Event, error) { return []*session.Event{}, nil },
		},
	}, nil
}

// Get loads a stored session and its events, mapping them back to ADK.
func (s *ADKSessionService) Get(ctx context.Context, req *session.GetRequest) (*session.GetResponse, error) {
	if req == nil {
		return nil, invalidArgument("get request must not be nil")
	}
	stored, err := s.sessions.Get(ctx, req.SessionID)
	if err != nil {
		return nil, err
	}
	return &session.GetResponse{
		Session: &adkSession{
			id:       stored.ID,
			userID:   stored.OwnerID,
			metadata: stored.Metadata,
			events:   func() ([]*session.Event, error) { return s.loadEvents(ctx, stored.ID) },
		},
	}, nil
}

// List returns the sessions of a user. The Aura store has no user-scoped
// list port, so this reports invalid_argument rather than a partial answer.
func (s *ADKSessionService) List(ctx context.Context, req *session.ListRequest) (*session.ListResponse, error) {
	if req == nil {
		return nil, invalidArgument("list request must not be nil")
	}
	return nil, invalidArgument("adk session listing is not supported by the aura store")
}

// Delete is not supported: the Aura store keeps sessions as a durable log
// with no destructive delete port.
func (s *ADKSessionService) Delete(ctx context.Context, req *session.DeleteRequest) error {
	if req == nil {
		return invalidArgument("delete request must not be nil")
	}
	return invalidArgument("adk session deletion is not supported by the aura store")
}

// AppendEvent persists an ADK event into the Aura runtime event log at the
// session's next sequence, preserving invocation, branch, author, actions,
// content, usage, and long-running tool identifiers.
func (s *ADKSessionService) AppendEvent(ctx context.Context, adkSession session.Session, ev *session.Event) error {
	if adkSession == nil {
		return invalidArgument("session must not be nil")
	}
	if ev == nil {
		return invalidArgument("event must not be nil")
	}
	re, err := store.RuntimeEventFromADK(adkSession.ID(), "", ev)
	if err != nil {
		return err
	}
	seq, err := s.events.LastSequence(ctx, adkSession.ID())
	if err != nil {
		return fmt.Errorf("last sequence for session %s: %w", adkSession.ID(), err)
	}
	re.Sequence = seq + 1
	return s.events.Append(ctx, &re)
}

func (s *ADKSessionService) loadEvents(ctx context.Context, sessionID string) ([]*session.Event, error) {
	stored, err := s.sessions.ListEvents(ctx, sessionID, 0, maxEventLoad)
	if err != nil {
		return nil, err
	}
	events := make([]*session.Event, 0, len(stored))
	for i := range stored {
		ev, err := store.RuntimeEventToADK(&stored[i])
		if err != nil {
			return nil, err
		}
		events = append(events, ev)
	}
	return events, nil
}
