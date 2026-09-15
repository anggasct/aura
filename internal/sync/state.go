package sync

import (
	"encoding/json"
	"os"
	"path/filepath"
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
	State        WorkerState   `json:"state"`
	LocalRef     string        `json:"local_ref"`
	RemoteRef    string        `json:"remote_ref"`
	VerifiedAt   time.Time     `json:"verified_at"`
	ConflictAt   time.Time     `json:"conflict_at"`
	UnknownAt    time.Time     `json:"unknown_at"`
	UnknownID    string        `json:"unknown_id"`
	LastResult   string        `json:"last_result"`
	LastDuration time.Duration `json:"last_duration_ns"`
	Runs         uint64        `json:"runs"`
	Conflicts    uint64        `json:"conflicts"`
	Unknowns     uint64        `json:"unknowns"`
}

type StateStore struct {
	mu         sync.Mutex
	checkpoint Checkpoint
	path       string
}

const (
	syncStateVersion  = 1
	syncStateFileName = "sync-checkpoint.json"
)

type persistedState struct {
	Version    int        `json:"version"`
	Checkpoint Checkpoint `json:"checkpoint"`
}

func SyncStatePath(dataRoot string) string {
	return filepath.Join(dataRoot, syncStateFileName)
}

func NewStateStore() *StateStore {
	return &StateStore{checkpoint: Checkpoint{State: WorkerIdle}}
}

func NewFileStateStore(path string) (*StateStore, error) {
	store := &StateStore{checkpoint: Checkpoint{State: WorkerIdle}, path: path}
	if strings.TrimSpace(path) == "" {
		return store, nil
	}
	//nolint:gosec // path is the operator-configured storage data root joined with a fixed filename.
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return store, nil
		}
		return nil, err
	}
	var persisted persistedState
	//nolint:nilerr // corrupted checkpoint fails open to idle so status remains available; next successful pass overwrites it.
	if err := json.Unmarshal(raw, &persisted); err != nil {
		return store, nil
	}
	if persisted.Version != syncStateVersion {
		return store, nil
	}
	if persisted.Checkpoint.State == "" {
		return store, nil
	}
	store.checkpoint = persisted.Checkpoint
	return store, nil
}

func (s *StateStore) persist(snapshot *Checkpoint) {
	if s.path == "" {
		return
	}
	persisted := persistedState{Version: syncStateVersion, Checkpoint: *snapshot}
	raw, err := json.Marshal(persisted)
	if err != nil {
		return
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	tmp, err := os.CreateTemp(dir, "sync-checkpoint-*.tmp")
	if err != nil {
		return
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		_ = os.Remove(tmpPath)
		return
	}
	if err := os.Rename(tmpPath, s.path); err != nil {
		_ = os.Remove(tmpPath)
		return
	}
	//nolint:gosec // dir is the operator-configured storage data root for fsync after atomic rename.
	if dirHandle, err := os.Open(dir); err == nil {
		_ = dirHandle.Sync()
		_ = dirHandle.Close()
	}
}

func (s *StateStore) Load() Checkpoint {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.checkpoint
}

func (s *StateStore) MarkDisabled() {
	s.mu.Lock()
	s.checkpoint.State = WorkerDisabled
	snapshot := s.checkpoint
	s.mu.Unlock()
	s.persist(&snapshot)
}

func (s *StateStore) MarkRunning() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.checkpoint.State = WorkerRunning
}

func (s *StateStore) MarkIdle(localRef, remoteRef string, at time.Time, duration time.Duration, result string) {
	s.mu.Lock()
	s.checkpoint.State = WorkerIdle
	s.checkpoint.LocalRef = localRef
	s.checkpoint.RemoteRef = remoteRef
	s.checkpoint.VerifiedAt = at.UTC()
	s.checkpoint.LastResult = normalizeSyncResult(result)
	s.checkpoint.LastDuration = duration
	s.checkpoint.Runs++
	snapshot := s.checkpoint
	s.mu.Unlock()
	s.persist(&snapshot)
}

func (s *StateStore) MarkConflict(localRef, remoteRef string, at time.Time) {
	s.mu.Lock()
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
	snapshot := s.checkpoint
	s.mu.Unlock()
	s.persist(&snapshot)
}

func (s *StateStore) MarkUnknown(intentID string, at time.Time) {
	s.mu.Lock()
	s.checkpoint.State = WorkerUnknown
	s.checkpoint.UnknownAt = at.UTC()
	s.checkpoint.UnknownID = intentID
	s.checkpoint.LastResult = string(WorkerUnknown)
	s.checkpoint.Unknowns++
	s.checkpoint.Runs++
	snapshot := s.checkpoint
	s.mu.Unlock()
	s.persist(&snapshot)
}

func (s *StateStore) ClearUnknown(localRef, remoteRef string, at time.Time, result string) {
	s.mu.Lock()
	s.checkpoint.State = WorkerIdle
	s.checkpoint.LocalRef = localRef
	s.checkpoint.RemoteRef = remoteRef
	s.checkpoint.VerifiedAt = at.UTC()
	s.checkpoint.UnknownID = ""
	s.checkpoint.LastResult = normalizeSyncResult(result)
	s.checkpoint.Runs++
	snapshot := s.checkpoint
	s.mu.Unlock()
	s.persist(&snapshot)
}
