package auth

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"time"

	"github.com/anggasct/aura/internal/config"
)

const (
	dirMode           = 0o700
	fileMode          = 0o600
	tokenFileName     = "token.json"
	maxTokenFileBytes = 64 << 10
	tokenFileVersion  = 1

	stateOK     = "ok"
	stateFailed = "failed"
)

type tokenFile struct {
	Version      int       `json:"version"`
	State        string    `json:"state"`
	Fingerprint  string    `json:"fingerprint"`
	AccessToken  string    `json:"access_token,omitempty"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	Expiry       time.Time `json:"expiry,omitzero"`
}

type tokenStore struct {
	dir string
	id  string
}

func (s *tokenStore) valid() error {
	if !config.ValidPluginID(s.id) {
		return Errorf(ErrInvalid, "the plugin id is not valid")
	}
	return nil
}

func (s *tokenStore) namespace(create bool) (*os.Root, error) {
	if err := s.valid(); err != nil {
		return nil, err
	}
	if create {
		if err := os.MkdirAll(s.dir, dirMode); err != nil {
			return nil, Wrap(ErrUnavailable, err, "the secrets directory cannot be created")
		}
		if err := os.Chmod(s.dir, dirMode); err != nil {
			return nil, Wrap(ErrUnavailable, err, "the secrets directory cannot be protected")
		}
	}
	base, err := os.OpenRoot(s.dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = base.Close() }()
	if create {
		if err := base.Mkdir(s.id, dirMode); err != nil && !errors.Is(err, fs.ErrExist) {
			return nil, Wrap(ErrUnavailable, err, "the plugin secrets directory cannot be created")
		}
		if err := base.Chmod(s.id, dirMode); err != nil {
			return nil, Wrap(ErrUnavailable, err, "the plugin secrets directory cannot be protected")
		}
	}
	return base.OpenRoot(s.id)
}

func (s *tokenStore) load() (tokenFile, bool, error) {
	ns, err := s.namespace(false)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return tokenFile{}, false, nil
		}
		return tokenFile{}, false, asUnavailable(err, "the stored token cannot be read")
	}
	defer func() { _ = ns.Close() }()
	file, err := ns.Open(tokenFileName)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return tokenFile{}, false, nil
		}
		return tokenFile{}, false, Wrap(ErrUnavailable, err, "the stored token cannot be read")
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, maxTokenFileBytes+1))
	if err != nil {
		return tokenFile{}, false, Wrap(ErrUnavailable, err, "the stored token cannot be read")
	}
	if len(data) > maxTokenFileBytes {
		return tokenFile{}, false, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var stored tokenFile
	if err := decoder.Decode(&stored); err != nil || decoder.More() {
		return tokenFile{}, false, nil
	}
	if stored.Version != tokenFileVersion || (stored.State != stateOK && stored.State != stateFailed) {
		return tokenFile{}, false, nil
	}
	return stored, true, nil
}

func asUnavailable(err error, detail string) error {
	if _, coded := CodeOf(err); coded {
		return err
	}
	return Wrap(ErrUnavailable, err, detail)
}

func (s *tokenStore) save(stored *tokenFile) error {
	stored.Version = tokenFileVersion
	data, err := json.Marshal(*stored)
	if err != nil {
		return Wrap(ErrUnavailable, err, "the token cannot be encoded")
	}
	ns, err := s.namespace(true)
	if err != nil {
		return asUnavailable(err, "the token cannot be stored")
	}
	defer func() { _ = ns.Close() }()
	temp := ".tmp-" + rand.Text()
	file, err := ns.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fileMode)
	if err != nil {
		return Wrap(ErrUnavailable, err, "the token cannot be stored")
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		_ = ns.Remove(temp)
		return Wrap(ErrUnavailable, err, "the token cannot be stored")
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = ns.Remove(temp)
		return Wrap(ErrUnavailable, err, "the token cannot be stored")
	}
	if err := file.Close(); err != nil {
		_ = ns.Remove(temp)
		return Wrap(ErrUnavailable, err, "the token cannot be stored")
	}
	if err := ns.Rename(temp, tokenFileName); err != nil {
		_ = ns.Remove(temp)
		return Wrap(ErrUnavailable, err, "the token cannot be stored")
	}
	return syncDirectory(ns)
}

func syncDirectory(ns *os.Root) error {
	dir, err := ns.Open(".")
	if err != nil {
		return Wrap(ErrUnavailable, err, "the token cannot be stored")
	}
	defer func() { _ = dir.Close() }()
	if err := dir.Sync(); err != nil {
		return Wrap(ErrUnavailable, err, "the token cannot be stored")
	}
	return nil
}

func (s *tokenStore) erase() error {
	if err := s.valid(); err != nil {
		return err
	}
	base, err := os.OpenRoot(s.dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return Wrap(ErrUnavailable, err, "the secrets directory cannot be opened")
	}
	defer func() { _ = base.Close() }()
	if err := base.RemoveAll(s.id); err != nil {
		return Wrap(ErrUnavailable, err, "the plugin secrets cannot be erased")
	}
	return nil
}

func (r *Registry) store(id string) *tokenStore {
	return &tokenStore{dir: r.dir, id: id}
}

func (r *Registry) Erase(id string) error {
	return r.store(id).erase()
}
