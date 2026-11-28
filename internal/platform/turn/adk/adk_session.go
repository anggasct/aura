package runtimeadk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"sync"
	"sync/atomic"
	"time"

	"github.com/anggasct/aura/internal/platform/turn"
	runtimeengine "github.com/anggasct/aura/internal/platform/turn/engine"
	turnstore "github.com/anggasct/aura/internal/platform/turn/store"

	"google.golang.org/adk/v2/session"
)

const maxEventLoad = 1 << 20

type ADKSessionService struct {
	sessions      SessionPort
	historyAuthor string
	leaveOutTurn  string
	compaction    *sessionCompaction
	current       atomic.Pointer[adkSession]

	errMu   sync.Mutex
	loadErr error
}

type SessionPort interface {
	Create(ctx context.Context, sess *turn.Session) error
	Get(ctx context.Context, sessionID string) (turn.Session, error)
	ListEvents(ctx context.Context, sessionID string, afterSequence uint64, limit int) ([]turn.Event, error)
}

func NewADKSessionService(sessions SessionPort) (*ADKSessionService, error) {
	if sessions == nil {
		return nil, invalidArgument("session port must not be nil")
	}
	return &ADKSessionService{sessions: sessions}, nil
}

func (s *ADKSessionService) recordEventsError(err error) {
	if s == nil || err == nil {
		return
	}
	s.errMu.Lock()
	defer s.errMu.Unlock()
	if s.loadErr == nil {
		s.loadErr = err
	}
}

func (s *ADKSessionService) clearEventsError() {
	if s == nil {
		return
	}
	s.errMu.Lock()
	defer s.errMu.Unlock()
	s.loadErr = nil
}

func (s *ADKSessionService) eventsError() error {
	if s == nil {
		return nil
	}
	s.errMu.Lock()
	defer s.errMu.Unlock()
	return s.loadErr
}

type adkSession struct {
	id            string
	userID        string
	metadata      json.RawMessage
	events        func() ([]*session.Event, error)
	loadRaw       func() ([]turn.Event, map[uint64]*session.Event, error)
	compact       func(step uint64, prior, current []turn.Event, appended []*session.Event) (*CompactionResult, error)
	onEventsError func(error)
	onAppend      func()

	mu            sync.Mutex
	appended      []*session.Event
	view          []*session.Event
	viewErr       error
	viewed        bool
	priorLoaded   bool
	priorRaw      []turn.Event
	priorBySeq    map[uint64]*session.Event
	compactedBase []*session.Event
	hasCompacted  bool
	compactStep   uint64
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
	return &adkEvents{load: s.merged, onErr: s.onEventsError}
}

func (s *adkSession) merged() ([]*session.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.viewed {
		return s.view, s.viewErr
	}
	if s.compact == nil {
		stored, err := s.events()
		if err != nil {
			s.view, s.viewErr, s.viewed = nil, err, true
			return nil, err
		}
		out := mergeEvents(stored, s.appended)
		s.view, s.viewErr, s.viewed = out, nil, true
		return out, nil
	}
	if !s.priorLoaded {
		raw, bySeq, err := s.loadRaw()
		if err != nil {
			s.view, s.viewErr, s.viewed = nil, err, true
			return nil, err
		}
		s.priorRaw = raw
		s.priorBySeq = bySeq
		s.priorLoaded = true
	}
	if !s.hasCompacted && len(s.appended) == 0 {
		out := make([]*session.Event, 0, len(s.priorRaw))
		for i := range s.priorRaw {
			out = append(out, s.priorBySeq[s.priorRaw[i].Sequence])
		}
		return out, nil
	}
	if s.hasCompacted && hasPendingFunctionCall(s.appended) {
		out := mergeEvents(s.compactedBase, s.appended)
		s.view, s.viewErr, s.viewed = out, nil, true
		return out, nil
	}
	currentRows := make([]turn.Event, 0, len(s.appended))
	for _, ev := range s.appended {
		re, err := turnstore.RuntimeEventFromADK(s.id, "", ev)
		if err != nil {
			s.view, s.viewErr, s.viewed = nil, err, true
			return nil, err
		}
		currentRows = append(currentRows, re)
	}
	s.compactStep++
	var toolBase []*session.Event
	if s.hasCompacted {
		toolBase = s.compactedBase
	} else {
		toolBase = make([]*session.Event, 0, len(s.priorRaw))
		for i := range s.priorRaw {
			toolBase = append(toolBase, s.priorBySeq[s.priorRaw[i].Sequence])
		}
	}
	toolEvents := mergeEvents(toolBase, s.appended)
	res, err := s.compact(s.compactStep, s.priorRaw, currentRows, toolEvents)
	if err != nil {
		s.view, s.viewErr, s.viewed = nil, err, true
		return nil, err
	}
	base, err := materializeCompactedEvents(res, s.priorBySeq)
	if err != nil {
		s.view, s.viewErr, s.viewed = nil, err, true
		return nil, err
	}
	s.compactedBase = base
	s.hasCompacted = true
	out := mergeEvents(base, s.appended)
	s.view, s.viewErr, s.viewed = out, nil, true
	return out, nil
}

func mergeEvents(stored, extra []*session.Event) []*session.Event {
	if len(extra) == 0 {
		return stored
	}
	seen := make(map[string]struct{}, len(stored)+len(extra))
	for _, ev := range stored {
		if ev != nil {
			seen[ev.ID] = struct{}{}
		}
	}
	out := make([]*session.Event, 0, len(stored)+len(extra))
	out = append(out, stored...)
	for _, ev := range extra {
		if ev == nil {
			continue
		}
		if _, ok := seen[ev.ID]; ok {
			continue
		}
		seen[ev.ID] = struct{}{}
		out = append(out, ev)
	}
	return out
}

func (s *adkSession) appendInMemory(ev *session.Event) {
	if s == nil || ev == nil || ev.Partial {
		return
	}
	s.mu.Lock()
	s.appended = append(s.appended, ev)
	s.view, s.viewErr, s.viewed = nil, nil, false
	onAppend := s.onAppend
	s.mu.Unlock()
	if onAppend != nil {
		onAppend()
	}
}

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

type adkEvents struct {
	load  func() ([]*session.Event, error)
	onErr func(error)
	cache []*session.Event
	err   error
}

func (e *adkEvents) loadOnce() {
	if e.cache != nil || e.err != nil {
		return
	}
	e.cache, e.err = e.load()
	if e.err != nil && e.onErr != nil {
		e.onErr(e.err)
	}
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

func (s *ADKSessionService) Create(ctx context.Context, req *session.CreateRequest) (*session.CreateResponse, error) {
	if req == nil {
		return nil, invalidArgument("create request must not be nil")
	}
	if req.UserID == "" {
		return nil, invalidArgument("user id must not be empty")
	}
	id := req.SessionID
	if id == "" {
		id = runtimeengine.NewTurnID()
	}
	state := req.State
	if state == nil {
		state = map[string]any{}
	}
	metadata, err := json.Marshal(state)
	if err != nil {
		return nil, fmt.Errorf("marshal adk session state: %w", err)
	}
	now := time.Now().UTC()
	stored := &turn.Session{
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
			id:            id,
			userID:        req.UserID,
			metadata:      metadata,
			events:        func() ([]*session.Event, error) { return []*session.Event{}, nil },
			onEventsError: s.recordEventsError,
			onAppend:      s.clearEventsError,
		},
	}, nil
}

func (s *ADKSessionService) Get(ctx context.Context, req *session.GetRequest) (*session.GetResponse, error) {
	if req == nil {
		return nil, invalidArgument("get request must not be nil")
	}
	stored, err := s.sessions.Get(ctx, req.SessionID)
	if err != nil {
		return nil, err
	}
	loaded := &adkSession{
		id:            stored.ID,
		userID:        stored.OwnerID,
		metadata:      stored.Metadata,
		events:        func() ([]*session.Event, error) { return s.loadEvents(ctx, stored.ID) },
		onEventsError: s.recordEventsError,
		onAppend:      s.clearEventsError,
	}
	if s.compaction != nil {
		cfg := s.compaction
		loaded.loadRaw = func() ([]turn.Event, map[uint64]*session.Event, error) {
			return s.loadRawAndADKEvents(ctx, stored.ID)
		}
		loaded.compact = func(step uint64, prior, current []turn.Event, appended []*session.Event) (*CompactionResult, error) {
			return compactWithJournal(ctx, cfg, stored.ID, step, prior, current, appended)
		}
	}
	s.clearEventsError()
	s.current.Store(loaded)
	return &session.GetResponse{Session: loaded}, nil
}

func (s *ADKSessionService) CurrentEvents(sessionID string) ([]*session.Event, error) {
	current := s.current.Load()
	if current == nil || current.ID() != sessionID {
		return nil, invalidArgument("the session has not been opened for this turn")
	}
	return current.merged()
}

func (s *ADKSessionService) List(ctx context.Context, req *session.ListRequest) (*session.ListResponse, error) {
	if req == nil {
		return nil, invalidArgument("list request must not be nil")
	}
	return nil, invalidArgument("adk session listing is not supported by the aura store")
}

func (s *ADKSessionService) Delete(ctx context.Context, req *session.DeleteRequest) error {
	if req == nil {
		return invalidArgument("delete request must not be nil")
	}
	return invalidArgument("adk session deletion is not supported by the aura store")
}

func (s *ADKSessionService) AppendEvent(ctx context.Context, cur session.Session, ev *session.Event) error {
	if cur == nil {
		return invalidArgument("session must not be nil")
	}
	if ev == nil {
		return invalidArgument("event must not be nil")
	}
	if _, err := turnstore.RuntimeEventFromADK(cur.ID(), "", ev); err != nil {
		return err
	}
	if inner, ok := cur.(*adkSession); ok {
		inner.appendInMemory(ev)
	}
	return nil
}

func (s *ADKSessionService) loadEvents(ctx context.Context, sessionID string) ([]*session.Event, error) {
	raw, bySeq, err := s.loadRawAndADKEvents(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	events := make([]*session.Event, 0, len(raw))
	for i := range raw {
		events = append(events, bySeq[raw[i].Sequence])
	}
	return events, nil
}

func (s *ADKSessionService) loadRawAndADKEvents(ctx context.Context, sessionID string) ([]turn.Event, map[uint64]*session.Event, error) {
	stored, err := s.sessions.ListEvents(ctx, sessionID, 0, maxEventLoad)
	if err != nil {
		return nil, nil, err
	}
	raw := make([]turn.Event, 0, len(stored))
	bySeq := make(map[uint64]*session.Event, len(stored))
	for i := range stored {
		if stored[i].Kind != turnstore.EventKindADK {
			continue
		}
		if s.leaveOutTurn != "" && stored[i].TurnID == s.leaveOutTurn {
			continue
		}
		ev, err := turnstore.RuntimeEventToADK(&stored[i])
		if err != nil {
			return nil, nil, err
		}
		if s.historyAuthor != "" && ev.Author != userAuthor {
			ev.Author = s.historyAuthor
		}
		raw = append(raw, stored[i])
		bySeq[stored[i].Sequence] = ev
	}
	return raw, bySeq, nil
}
