package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
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
	mu   sync.Mutex
	path string
}

func NewFileTrustRegistry(path string) (*FileTrustRegistry, error) {
	if path == "" {
		return nil, Errorf(ErrConfigInvalid, "trust registry path must not be empty")
	}
	return &FileTrustRegistry{path: filepath.Clean(path)}, nil
}

func (r *FileTrustRegistry) readLocked() (map[string]*TrustRecord, error) {
	records := make(map[string]*TrustRecord)
	raw, err := os.ReadFile(r.path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return records, nil
		}
		return nil, Errorf(ErrServerUnavailable, "trust registry is not readable")
	}
	var envelope fileTrustEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, Errorf(ErrServerUnavailable, "trust registry is corrupt")
	}
	if envelope.Version != fileStoreVersion {
		return nil, Errorf(ErrServerUnavailable, "trust registry version is not supported")
	}
	for name := range envelope.Servers {
		record := envelope.Servers[name]
		record.Capabilities = append([]string(nil), record.Capabilities...)
		record.Tools = append([]string(nil), record.Tools...)
		records[name] = &record
	}
	return records, nil
}

func (r *FileTrustRegistry) writeLocked(records map[string]*TrustRecord) error {
	servers := make(map[string]TrustRecord, len(records))
	for name, record := range records {
		servers[name] = *record
	}
	raw, err := json.Marshal(fileTrustEnvelope{Version: fileStoreVersion, Servers: servers})
	if err != nil {
		return Errorf(ErrServerUnavailable, "trust registry encoding failed")
	}
	return writeFileAtomic(r.path, raw)
}

func (r *FileTrustRegistry) mutate(serverName string, update func(rec *TrustRecord, existed bool) error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	records, err := r.readLocked()
	if err != nil {
		return err
	}
	rec, existed := records[serverName]
	if !existed {
		rec = &TrustRecord{ServerName: serverName}
	}
	if err := update(rec, existed); err != nil {
		return err
	}
	records[serverName] = rec
	return r.writeLocked(records)
}

func (r *FileTrustRegistry) GetTrust(_ context.Context, serverName string) (*TrustRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	records, err := r.readLocked()
	if err != nil {
		return nil, err
	}
	rec, ok := records[serverName]
	if !ok {
		return nil, ErrTrustNotFound
	}
	return rec, nil
}

func (r *FileTrustRegistry) SaveSessionTrust(_ context.Context, serverName, digest string, capabilities, tools []string) error {
	if serverName == "" {
		return Errorf(ErrConfigInvalid, "server name must not be empty")
	}
	if digest == "" {
		return Errorf(ErrConfigInvalid, "trust digest must not be empty")
	}
	return r.mutate(serverName, func(rec *TrustRecord, _ bool) error {
		if rec.Digest == digest && rec.Decision == TrustDecisionApproved {
			return nil
		}
		rec.Digest = digest
		rec.Decision = TrustDecisionPending
		rec.Capabilities = append([]string(nil), capabilities...)
		rec.Tools = append([]string(nil), tools...)
		return nil
	})
}

func (r *FileTrustRegistry) Approve(_ context.Context, serverName, digest string) error {
	if serverName == "" {
		return Errorf(ErrConfigInvalid, "server name must not be empty")
	}
	if digest == "" {
		return Errorf(ErrConfigInvalid, "trust digest must not be empty")
	}
	return r.mutate(serverName, func(rec *TrustRecord, existed bool) error {
		if !existed || rec.Digest != digest {
			return ErrTrustNotFound
		}
		rec.Decision = TrustDecisionApproved
		rec.ReviewedAt = time.Now().UTC()
		return nil
	})
}

func (r *FileTrustRegistry) IsTrusted(_ context.Context, serverName, digest string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	records, err := r.readLocked()
	if err != nil {
		return false, err
	}
	rec, ok := records[serverName]
	return ok && digest != "" && rec.Digest == digest && rec.Decision == TrustDecisionApproved, nil
}

func (r *FileTrustRegistry) ApproveConfig(_ context.Context, serverName, digest string) error {
	if serverName == "" {
		return Errorf(ErrConfigInvalid, "server name must not be empty")
	}
	if digest == "" {
		return Errorf(ErrConfigInvalid, "config digest must not be empty")
	}
	return r.mutate(serverName, func(rec *TrustRecord, _ bool) error {
		approveConfigRecord(rec, digest, time.Now().UTC())
		return nil
	})
}

func (r *FileTrustRegistry) IsConfigTrusted(_ context.Context, serverName, digest string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	records, err := r.readLocked()
	if err != nil {
		return false, err
	}
	return configTrusted(records[serverName], digest), nil
}

func (r *FileTrustRegistry) Revoke(_ context.Context, serverName string) (bool, error) {
	if serverName == "" {
		return false, Errorf(ErrConfigInvalid, "server name must not be empty")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	records, err := r.readLocked()
	if err != nil {
		return false, err
	}
	if _, existed := records[serverName]; !existed {
		return false, nil
	}
	delete(records, serverName)
	return true, r.writeLocked(records)
}
