package interpreter

import (
	"sync"
)

type State string

const (
	StateAbsent    State = "absent"
	StateInstalled State = "installed"
	StateInvalid   State = "invalid"
	StatePending   State = "pending"
)

type Reason string

const (
	ReasonNone              Reason = ""
	ReasonBundleMissing     Reason = "bundle_missing"
	ReasonDigestMissing     Reason = "digest_missing"
	ReasonDigestMismatch    Reason = "digest_mismatch"
	ReasonTreeUnreadable    Reason = "tree_unreadable"
	ReasonTreeUnsafe        Reason = "tree_unsafe"
	ReasonExecutableMissing Reason = "executable_missing"
	ReasonProbeFailed       Reason = "probe_failed"
)

type Verification struct {
	Runtime string
	Version string
	State   State
	Reason  Reason
}

func (v Verification) Verified() bool {
	return v.State == StateInstalled
}

type VerifiedSet struct {
	mu     sync.RWMutex
	order  []string
	status map[string]Verification
}

func NewVerifiedSet(manifest *Manifest) (*VerifiedSet, error) {
	if manifest == nil {
		return nil, Errorf(ErrorCodeInvalidArgument, "manifest is required")
	}
	set := &VerifiedSet{status: map[string]Verification{}}
	for _, runtime := range manifest.Runtimes() {
		set.order = append(set.order, runtime.ID)
		set.status[runtime.ID] = Verification{Runtime: runtime.ID, Version: runtime.Version, State: StatePending}
	}
	return set, nil
}

func (s *VerifiedSet) Status(id string) (Verification, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	status, ok := s.status[id]
	return status, ok
}

func (s *VerifiedSet) Verified(id string) bool {
	status, ok := s.Status(id)
	return ok && status.Verified()
}

func (s *VerifiedSet) Statuses() []Verification {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Verification, 0, len(s.order))
	for _, id := range s.order {
		out = append(out, s.status[id])
	}
	return out
}

func (s *VerifiedSet) record(verification Verification) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, known := s.status[verification.Runtime]; known {
		s.status[verification.Runtime] = verification
	}
}
