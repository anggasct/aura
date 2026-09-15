package sync

import (
	"strings"
	"sync"
	"time"
)

type WorkerState string

const (
	WorkerDisabled WorkerState = "disabled"
	WorkerIdle     WorkerState = "idle"
	WorkerRunning  WorkerState = "running"
	WorkerConflict WorkerState = "conflict"
	WorkerUnknown  WorkerState = "unknown"
)

type Checkpoint struct {
	State        WorkerState
	LocalRef     string
	RemoteRef    string
	VerifiedAt   time.Time
	ConflictAt   time.Time
	UnknownAt    time.Time
	UnknownID    string
	LastResult   string
	LastDuration time.Duration
	Runs         uint64
	Conflicts    uint64
	Unknowns     uint64
}

type StateStore struct {
	mu         sync.Mutex
	checkpoint Checkpoint
}

func NewStateStore() *StateStore {
	return &StateStore{checkpoint: Checkpoint{State: WorkerIdle}}
}

func (s *StateStore) Load() Checkpoint {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.checkpoint
}

func (s *StateStore) MarkDisabled() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.checkpoint.State = WorkerDisabled
}

func (s *StateStore) MarkRunning() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.checkpoint.State = WorkerRunning
}

func (s *StateStore) MarkIdle(localRef, remoteRef string, at time.Time, duration time.Duration, result string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.checkpoint.State = WorkerIdle
	s.checkpoint.LocalRef = localRef
	s.checkpoint.RemoteRef = remoteRef
	s.checkpoint.VerifiedAt = at.UTC()
	s.checkpoint.LastResult = result
	s.checkpoint.LastDuration = duration
	s.checkpoint.Runs++
}

func (s *StateStore) MarkConflict(localRef, remoteRef string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.checkpoint.State = WorkerConflict
	if strings.TrimSpace(localRef) != "" {
		s.checkpoint.LocalRef = localRef
	}
	if strings.TrimSpace(remoteRef) != "" {
		s.checkpoint.RemoteRef = remoteRef
	}
	s.checkpoint.ConflictAt = at.UTC()
	s.checkpoint.LastResult = string(AdvanceConflict)
	s.checkpoint.Conflicts++
	s.checkpoint.Runs++
}

func (s *StateStore) MarkUnknown(intentID string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.checkpoint.State = WorkerUnknown
	s.checkpoint.UnknownAt = at.UTC()
	s.checkpoint.UnknownID = intentID
	s.checkpoint.LastResult = string(WorkerUnknown)
	s.checkpoint.Unknowns++
	s.checkpoint.Runs++
}

func (s *StateStore) ClearUnknown(localRef, remoteRef string, at time.Time, result string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.checkpoint.State = WorkerIdle
	s.checkpoint.LocalRef = localRef
	s.checkpoint.RemoteRef = remoteRef
	s.checkpoint.VerifiedAt = at.UTC()
	s.checkpoint.UnknownID = ""
	s.checkpoint.LastResult = result
	s.checkpoint.Runs++
}
