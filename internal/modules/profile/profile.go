package profile

import (
	stdcontext "context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/anggasct/aura/internal/platform/fault"
)

const (
	StatusCandidate = "candidate"
	StatusActive    = "active"
	StatusRejected  = "rejected"
	StatusExpired   = "expired"
	StatusDeleted   = "deleted"

	OriginDerived = "derived"
	OriginOwner   = "owner"
	OriginAgent   = "agent"

	ConfidencePolicyV1 = "conf-v1"

	maxKeyChars   = 128
	maxValueChars = 4000
)

var Categories = []string{"preference", "tool", "language", "timezone", "project", "habit"}

type Fact struct {
	ID               string
	OwnerID          string
	Category         string
	Key              string
	Value            string
	ValueDigest      string
	Status           string
	Origin           string
	OwnerVerified    bool
	TaintSource      string
	Confidence       float64
	ConfidencePolicy string
	ConflictsWithID  string
	ExpiresAt        *time.Time
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

type Evidence struct {
	SourceEventID string
	SourceDigest  string
	Provider      string
	Model         string
	ModelVersion  string
	PromptVersion string
	PromptDigest  string
	TaintSource   string
	ObservedAt    time.Time
}

type Registry interface {
	InsertCandidate(ctx stdcontext.Context, fact *Fact, evidence *Evidence, minConfidence float64) (Fact, error)
	Reinforce(ctx stdcontext.Context, factID string, evidence *Evidence, minConfidence float64) (Fact, error)
	SetState(ctx stdcontext.Context, factID, status, conflictsWith string, confidence float64, verified bool, at time.Time) error
	Promote(ctx stdcontext.Context, factID string, expiresAt *time.Time, at time.Time) error
	ReviveCandidate(ctx stdcontext.Context, factID string, at time.Time) error
	Get(ctx stdcontext.Context, id string) (Fact, bool, error)
	Active(ctx stdcontext.Context, ownerID, category, key string) ([]Fact, error)
	Evidence(ctx stdcontext.Context, factID string) ([]Evidence, error)
	Expire(ctx stdcontext.Context, now time.Time) ([]Fact, error)
	Search(ctx stdcontext.Context, ownerID, category, query string, verifiedOnly bool, limit int, now time.Time) ([]SearchHit, error)
	List(ctx stdcontext.Context, ownerID, status, category string, limit int) ([]Fact, error)
}

type ServiceOptions struct {
	MinConfidence float64
	Scanner       SecretScanner
	Actions       ActionSink
	Observer      Observer
}

type Service struct {
	registry      Registry
	minConfidence float64
	scanner       SecretScanner
	actions       ActionSink
	observer      Observer
}

func NewService(registry Registry, options ServiceOptions) (*Service, error) {
	if registry == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "registry must not be nil")
	}
	if options.MinConfidence < 0 || options.MinConfidence > 1 {
		return nil, fault.New(ErrorCodeInvalidArgument, "minimum confidence must be in [0,1]")
	}
	scanner := options.Scanner
	if scanner == nil {
		scanner = rejectAllScanner{}
	}
	return &Service{registry: registry, minConfidence: options.MinConfidence, scanner: scanner, actions: options.Actions, observer: options.Observer}, nil
}

func ConfidenceV1(evidence int) float64 {
	if evidence < 1 {
		return 0.50
	}
	return min(0.50+0.10*float64(evidence), 0.95)
}

func ValueDigest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func FactID(ownerID, category, key, digest string) string {
	raw := strings.Join([]string{ownerID, category, key, digest}, "\x00")
	sum := sha256.Sum256([]byte(raw))
	return "pf-" + hex.EncodeToString(sum[:])[:16]
}

func checkProposal(ownerID, category, key, value string) error {
	if strings.TrimSpace(ownerID) == "" {
		return fault.New(ErrorCodeInvalidArgument, "owner must not be empty")
	}
	valid := false
	for _, categoryID := range Categories {
		if category == categoryID {
			valid = true
			break
		}
	}
	if !valid {
		return fault.New(ErrorCodeInvalidArgument, "fact category is not valid")
	}
	if utf8.RuneCountInString(key) < 1 || utf8.RuneCountInString(key) > maxKeyChars {
		return fault.New(ErrorCodeInvalidArgument, "fact key length is out of range")
	}
	if utf8.RuneCountInString(value) < 1 || utf8.RuneCountInString(value) > maxValueChars {
		return fault.New(ErrorCodeInvalidArgument, "fact value length is out of range")
	}
	return nil
}

func (s *Service) ProposeFact(ctx stdcontext.Context, ownerID, category, key, value string, evidence *Evidence, now time.Time) (Fact, error) {
	if ctx == nil {
		return Fact{}, fault.New(ErrorCodeInvalidArgument, "context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return Fact{}, err
	}
	if err := checkProposal(ownerID, category, key, value); err != nil {
		return Fact{}, err
	}
	if evidence == nil {
		return Fact{}, fault.New(ErrorCodeInvalidArgument, "evidence must not be nil")
	}
	if now.IsZero() {
		return Fact{}, fault.New(ErrorCodeInvalidArgument, "timestamp must not be zero")
	}
	digest := ValueDigest(value)
	id := FactID(ownerID, category, key, digest)
	existing, found, err := s.registry.Get(ctx, id)
	if err != nil {
		return Fact{}, err
	}
	if found {
		return s.repropose(ctx, &existing, evidence, now)
	}
	actives, err := s.registry.Active(ctx, ownerID, category, key)
	if err != nil {
		return Fact{}, err
	}
	fact := Fact{
		ID: id, OwnerID: ownerID, Category: category, Key: key, Value: value,
		ValueDigest: digest, Status: StatusCandidate, Origin: OriginDerived,
		TaintSource: evidence.TaintSource,
		Confidence:  ConfidenceV1(1), ConfidencePolicy: ConfidencePolicyV1,
		CreatedAt: now, UpdatedAt: now,
	}
	for i := range actives {
		if actives[i].ValueDigest != digest {
			fact.ConflictsWithID = actives[i].ID
		}
	}
	return s.registry.InsertCandidate(ctx, &fact, evidence, s.minConfidence)
}

func (s *Service) repropose(ctx stdcontext.Context, existing *Fact, evidence *Evidence, now time.Time) (Fact, error) {
	switch existing.Status {
	case StatusActive, StatusCandidate:
		return s.registry.Reinforce(ctx, existing.ID, evidence, s.minConfidence)
	case StatusRejected, StatusDeleted:
		known, err := s.registry.Evidence(ctx, existing.ID)
		if err != nil {
			return Fact{}, err
		}
		for i := range known {
			if known[i].SourceDigest == evidence.SourceDigest {
				return Fact{}, fault.New(ErrorCodeProfileConflict, "decided value cannot be resurrected from the same source")
			}
		}
		if err := s.registry.ReviveCandidate(ctx, existing.ID, now); err != nil {
			return Fact{}, err
		}
		return s.registry.Reinforce(ctx, existing.ID, evidence, s.minConfidence)
	default:
		return Fact{}, fault.New(ErrorCodeProfileConflict, "fact is in a terminal state")
	}
}

func (s *Service) ReinforceFact(ctx stdcontext.Context, factID string, evidence *Evidence) (Fact, error) {
	if ctx == nil {
		return Fact{}, fault.New(ErrorCodeInvalidArgument, "context must not be nil")
	}
	if strings.TrimSpace(factID) == "" {
		return Fact{}, fault.New(ErrorCodeInvalidArgument, "fact id must not be empty")
	}
	if evidence == nil {
		return Fact{}, fault.New(ErrorCodeInvalidArgument, "evidence must not be nil")
	}
	return s.registry.Reinforce(ctx, factID, evidence, s.minConfidence)
}

func (s *Service) DeleteFact(ctx stdcontext.Context, ownerID, factID string, now time.Time) error {
	if err := s.ownerArgs(ctx, ownerID, factID, now); err != nil {
		return err
	}
	fact, found, err := s.registry.Get(ctx, factID)
	if err != nil {
		return err
	}
	if !found || fact.OwnerID != ownerID {
		return fault.New(ErrorCodeProfileNotFound, "fact does not exist")
	}
	if fact.Status == StatusDeleted {
		return nil
	}
	if err := s.registry.SetState(ctx, factID, StatusDeleted, fact.ConflictsWithID, fact.Confidence, fact.OwnerVerified, now); err != nil {
		return err
	}
	return s.recordAction(ctx, ownerID, factID, "delete", &fact, now)
}

func (s *Service) ExpireFacts(ctx stdcontext.Context, now time.Time) ([]Fact, error) {
	if ctx == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "context must not be nil")
	}
	if now.IsZero() {
		return nil, fault.New(ErrorCodeInvalidArgument, "timestamp must not be zero")
	}
	expired, err := s.registry.Expire(ctx, now)
	if err != nil {
		return nil, err
	}
	for i := range expired {
		lag := time.Duration(0)
		if expired[i].ExpiresAt != nil {
			lag = now.Sub(*expired[i].ExpiresAt)
			if lag < 0 {
				lag = 0
			}
		}
		observeWith(ctx, s.observer, &Observation{Kind: ObserveExpiry, Lag: lag})
	}
	return expired, nil
}

func (s *Service) GetFact(ctx stdcontext.Context, ownerID, factID string) (Fact, error) {
	if ctx == nil {
		return Fact{}, fault.New(ErrorCodeInvalidArgument, "context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return Fact{}, err
	}
	if strings.TrimSpace(ownerID) == "" || strings.TrimSpace(factID) == "" {
		return Fact{}, fault.New(ErrorCodeInvalidArgument, "owner and fact id must not be empty")
	}
	fact, found, err := s.registry.Get(ctx, factID)
	if err != nil {
		return Fact{}, err
	}
	if !found || fact.OwnerID != ownerID {
		return Fact{}, fault.New(ErrorCodeProfileNotFound, "fact does not exist")
	}
	return fact, nil
}

type rejectAllScanner struct{}

func (rejectAllScanner) Contains(string) bool { return false }

func (s *Service) ListFacts(ctx stdcontext.Context, ownerID, status, category string, limit int) ([]Fact, error) {
	if ctx == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "context must not be nil")
	}
	if limit <= 0 {
		return nil, fault.New(ErrorCodeInvalidArgument, "fact list limit must be positive")
	}
	return s.registry.List(ctx, ownerID, status, category, limit)
}

func (s *Service) EvidenceFor(ctx stdcontext.Context, factID string) ([]Evidence, error) {
	if ctx == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "context must not be nil")
	}
	return s.registry.Evidence(ctx, factID)
}
