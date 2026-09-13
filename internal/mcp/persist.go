package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const fileStoreVersion = 1

func writeFileAtomic(path string, data []byte) error {
	clean := filepath.Clean(path)
	dir := filepath.Dir(clean)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Errorf(ErrServerUnavailable, "token storage directory is not writable")
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return Errorf(ErrServerUnavailable, "token storage is not writable")
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return Errorf(ErrServerUnavailable, "token storage write failed")
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return Errorf(ErrServerUnavailable, "token storage write failed")
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return Errorf(ErrServerUnavailable, "token storage write failed")
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		_ = os.Remove(tmpName)
		return Errorf(ErrServerUnavailable, "token storage protection failed")
	}
	if err := os.Rename(tmpName, clean); err != nil {
		_ = os.Remove(tmpName)
		return Errorf(ErrServerUnavailable, "token storage write failed")
	}
	dirHandle, err := os.Open(dir)
	if err != nil {
		return Errorf(ErrServerUnavailable, "token storage write failed")
	}
	defer func() { _ = dirHandle.Close() }()
	if err := dirHandle.Sync(); err != nil {
		return Errorf(ErrServerUnavailable, "token storage write failed")
	}
	return nil
}

type TokenRecord struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	TokenType    string    `json:"token_type,omitempty"`
	Expiry       time.Time `json:"expiry,omitempty"`
}

type FileTokenStore struct {
	mu   sync.Mutex
	path string
}

func NewFileTokenStore(path string) (*FileTokenStore, error) {
	if path == "" {
		return nil, Errorf(ErrConfigInvalid, "token store path must not be empty")
	}
	return &FileTokenStore{path: filepath.Clean(path)}, nil
}

func (s *FileTokenStore) Load() (TokenRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return TokenRecord{}, false, nil
		}
		return TokenRecord{}, false, Errorf(ErrServerUnavailable, "token storage is not readable")
	}
	var record TokenRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return TokenRecord{}, false, Errorf(ErrServerUnavailable, "token storage is corrupt")
	}
	return record, true, nil
}

func (s *FileTokenStore) Store(record TokenRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, err := json.Marshal(record)
	if err != nil {
		return Errorf(ErrServerUnavailable, "token storage encoding failed")
	}
	return writeFileAtomic(s.path, raw)
}

type fileTrustEnvelope struct {
	Version int                    `json:"version"`
	Servers map[string]TrustRecord `json:"servers"`
}

type FileTrustRegistry struct {
	mu      sync.Mutex
	path    string
	records map[string]*TrustRecord
	loaded  bool
}

func NewFileTrustRegistry(path string) (*FileTrustRegistry, error) {
	if path == "" {
		return nil, Errorf(ErrConfigInvalid, "trust registry path must not be empty")
	}
	return &FileTrustRegistry{path: filepath.Clean(path), records: make(map[string]*TrustRecord)}, nil
}

func (r *FileTrustRegistry) loadLocked() error {
	if r.loaded {
		return nil
	}
	r.loaded = true
	raw, err := os.ReadFile(r.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return Errorf(ErrServerUnavailable, "trust registry is not readable")
	}
	var envelope fileTrustEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return Errorf(ErrServerUnavailable, "trust registry is corrupt")
	}
	for name := range envelope.Servers {
		record := envelope.Servers[name]
		copied := record
		copied.Capabilities = append([]string(nil), record.Capabilities...)
		copied.Tools = append([]string(nil), record.Tools...)
		r.records[name] = &copied
	}
	return nil
}

func (r *FileTrustRegistry) persistLocked() error {
	servers := make(map[string]TrustRecord, len(r.records))
	for name, record := range r.records {
		servers[name] = *record
	}
	raw, err := json.Marshal(fileTrustEnvelope{Version: fileStoreVersion, Servers: servers})
	if err != nil {
		return Errorf(ErrServerUnavailable, "trust registry encoding failed")
	}
	return writeFileAtomic(r.path, raw)
}

func (r *FileTrustRegistry) GetTrust(_ context.Context, serverName string) (*TrustRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.loadLocked(); err != nil {
		return nil, err
	}
	rec, ok := r.records[serverName]
	if !ok || rec == nil {
		return nil, ErrTrustNotFound
	}
	recCopy := *rec
	recCopy.Capabilities = append([]string(nil), rec.Capabilities...)
	recCopy.Tools = append([]string(nil), rec.Tools...)
	return &recCopy, nil
}

func (r *FileTrustRegistry) saveLocked(serverName string, update func(*TrustRecord)) {
	rec := r.records[serverName]
	if rec == nil {
		rec = &TrustRecord{ServerName: serverName}
	}
	update(rec)
	r.records[serverName] = rec
}

func (r *FileTrustRegistry) SaveSessionTrust(_ context.Context, serverName, digest string, capabilities, tools []string) error {
	if serverName == "" {
		return Errorf(ErrConfigInvalid, "server name must not be empty")
	}
	if digest == "" {
		return Errorf(ErrConfigInvalid, "trust digest must not be empty")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.loadLocked(); err != nil {
		return err
	}
	r.saveLocked(serverName, func(rec *TrustRecord) {
		if rec.Digest == digest && rec.Decision == TrustDecisionApproved {
			return
		}
		rec.Digest = digest
		rec.Decision = TrustDecisionPending
		rec.Capabilities = append([]string(nil), capabilities...)
		rec.Tools = append([]string(nil), tools...)
	})
	return r.persistLocked()
}

func (r *FileTrustRegistry) Approve(_ context.Context, serverName, digest string) error {
	return r.decide(serverName, digest, false, TrustDecisionApproved)
}

func (r *FileTrustRegistry) IsTrusted(_ context.Context, serverName, digest string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.loadLocked(); err != nil {
		return false, err
	}
	rec, ok := r.records[serverName]
	return ok && rec != nil && rec.Digest == digest && rec.Decision == TrustDecisionApproved, nil
}

func (r *FileTrustRegistry) SaveSpawnTrust(_ context.Context, serverName, spawnDigest string) error {
	if serverName == "" {
		return Errorf(ErrConfigInvalid, "server name must not be empty")
	}
	if spawnDigest == "" {
		return Errorf(ErrConfigInvalid, "spawn digest must not be empty")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.loadLocked(); err != nil {
		return err
	}
	r.saveLocked(serverName, func(rec *TrustRecord) {
		if rec.SpawnDigest == spawnDigest && rec.SpawnDecision == TrustDecisionApproved {
			return
		}
		rec.SpawnDigest = spawnDigest
		rec.SpawnDecision = TrustDecisionPending
	})
	return r.persistLocked()
}

func (r *FileTrustRegistry) ApproveSpawn(_ context.Context, serverName, spawnDigest string) error {
	return r.decide(serverName, spawnDigest, true, TrustDecisionApproved)
}

func (r *FileTrustRegistry) IsSpawnTrusted(_ context.Context, serverName, spawnDigest string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.loadLocked(); err != nil {
		return false, err
	}
	rec, ok := r.records[serverName]
	return ok && rec != nil && rec.SpawnDigest == spawnDigest && rec.SpawnDecision == TrustDecisionApproved, nil
}

func (r *FileTrustRegistry) decide(serverName, digest string, spawn bool, decision TrustDecision) error {
	if serverName == "" {
		return Errorf(ErrConfigInvalid, "server name must not be empty")
	}
	if digest == "" {
		return Errorf(ErrConfigInvalid, "trust digest must not be empty")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.loadLocked(); err != nil {
		return err
	}
	rec, ok := r.records[serverName]
	if !ok || rec == nil {
		return ErrTrustNotFound
	}
	if spawn {
		if rec.SpawnDigest != digest {
			return ErrTrustNotFound
		}
		rec.SpawnDecision = decision
	} else {
		if rec.Digest != digest {
			return ErrTrustNotFound
		}
		rec.Decision = decision
	}
	return r.persistLocked()
}
