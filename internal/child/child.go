package child

import (
	stdcontext "context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	StatusQueued      = "queued"
	StatusRunning     = "running"
	StatusSucceeded   = "succeeded"
	StatusFailed      = "failed"
	StatusCancelled   = "cancelled"
	StatusDeadline    = "deadline_exceeded"
	StatusInterrupted = "interrupted"
)

const (
	maxTaskChars      = 8192
	maxDepthAllowed   = 1
	maxGrants         = 64
	maxGrantChars     = 128
	maxReferences     = 32
	maxReferenceChars = 512
)

type Reference struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

type Grant struct {
	Capability string `json:"capability"`
	Scope      string `json:"scope,omitempty"`
}

type Budget struct {
	MaxTokens int64         `json:"max_tokens"`
	MaxCost   int64         `json:"max_cost"`
	MaxTools  int           `json:"max_tools"`
	Timeout   time.Duration `json:"timeout_ns"`
}

type Spec struct {
	ID               string
	IdempotencyKey   string
	ParentSessionID  string
	ParentTurnID     string
	ParentInvocation string
	OwnerID          string
	ChildSessionID   string
	ParentDepth      int
	ParentGrants     []Grant
	Task             string
	References       []Reference
	RequestedGrants  []Grant
	Provider         string
	Model            string
	Budget           Budget
	Background       bool
	ContextDigest    string
}

type Spawn struct {
	ID               string
	SessionID        string
	Grants           []Grant
	DurableKey       string
	ContextDigest    string
	Deadline         time.Time
	CreatedAt        time.Time
	ParentSessionID  string
	ParentTurnID     string
	ParentInvocation string
	OwnerID          string
}

type Registry interface {
	Spawn(ctx stdcontext.Context, spec *Spec, now time.Time) (Spawn, bool, error)
	Get(ctx stdcontext.Context, id string) (Spawn, bool, error)
}

type Service struct {
	registry Registry
}

func NewService(registry Registry) (*Service, error) {
	if registry == nil {
		return nil, errNilArgument("registry")
	}
	return &Service{registry: registry}, nil
}

func DurableChildKey(childID string) string {
	return "child/" + childID
}

func ContextDigest(task string, references []Reference) string {
	var digest strings.Builder
	digest.WriteString(task)
	for _, reference := range references {
		digest.WriteString("\x00")
		digest.WriteString(reference.Kind)
		digest.WriteString("\x00")
		digest.WriteString(reference.ID)
	}
	sum := sha256.Sum256([]byte(digest.String()))
	return hex.EncodeToString(sum[:])
}

func checkSpec(spec *Spec) error {
	if strings.TrimSpace(spec.ID) == "" {
		return Errorf(ErrorCodeInvalidArgument, "child id must not be empty")
	}
	if strings.TrimSpace(spec.IdempotencyKey) == "" {
		return Errorf(ErrorCodeInvalidArgument, "idempotency key must not be empty")
	}
	if strings.TrimSpace(spec.ParentSessionID) == "" || strings.TrimSpace(spec.ParentTurnID) == "" || strings.TrimSpace(spec.ParentInvocation) == "" {
		return Errorf(ErrorCodeInvalidArgument, "parent lineage must not be empty")
	}
	if strings.TrimSpace(spec.OwnerID) == "" {
		return Errorf(ErrorCodeInvalidArgument, "owner must not be empty")
	}
	if strings.TrimSpace(spec.ChildSessionID) == "" {
		return Errorf(ErrorCodeInvalidArgument, "child session must not be empty")
	}
	if spec.ParentDepth < 0 {
		return Errorf(ErrorCodeInvalidArgument, "parent depth must not be negative")
	}
	if spec.ParentDepth+1 > maxDepthAllowed {
		return Errorf(ErrorCodeChildDepthExceeded, "child depth exceeds the maximum of one")
	}
	if utf8.RuneCountInString(spec.Task) < 1 || utf8.RuneCountInString(spec.Task) > maxTaskChars {
		return Errorf(ErrorCodeInvalidArgument, "child task length is out of range")
	}
	if len(spec.References) > maxReferences {
		return Errorf(ErrorCodeInvalidArgument, "child references exceed the maximum")
	}
	for _, reference := range spec.References {
		if strings.TrimSpace(reference.Kind) == "" || strings.TrimSpace(reference.ID) == "" {
			return Errorf(ErrorCodeInvalidArgument, "child reference must carry kind and id")
		}
		if utf8.RuneCountInString(reference.ID) > maxReferenceChars {
			return Errorf(ErrorCodeInvalidArgument, "child reference id length is out of range")
		}
	}
	if len(spec.RequestedGrants) > maxGrants {
		return Errorf(ErrorCodeInvalidArgument, "child grants exceed the maximum")
	}
	if _, err := attenuateGrants(spec.ParentGrants, spec.RequestedGrants); err != nil {
		return err
	}
	if spec.Budget.MaxTokens < 0 || spec.Budget.MaxCost < 0 || spec.Budget.MaxTools < 0 {
		return Errorf(ErrorCodeInvalidArgument, "child budget must not be negative")
	}
	if spec.Budget.Timeout <= 0 {
		return Errorf(ErrorCodeInvalidArgument, "child timeout must be positive")
	}
	return nil
}

func (s *Service) SpawnChild(ctx stdcontext.Context, spec *Spec, now time.Time) (Spawn, bool, error) {
	if ctx == nil {
		return Spawn{}, false, Errorf(ErrorCodeInvalidArgument, "context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return Spawn{}, false, err
	}
	if spec == nil {
		return Spawn{}, false, errNilArgument("spec")
	}
	if now.IsZero() {
		return Spawn{}, false, Errorf(ErrorCodeInvalidArgument, "timestamp must not be zero")
	}
	if err := checkSpec(spec); err != nil {
		return Spawn{}, false, err
	}
	childGrants, err := attenuateGrants(spec.ParentGrants, spec.RequestedGrants)
	if err != nil {
		return Spawn{}, false, err
	}
	spawned := *spec
	spawned.ContextDigest = ContextDigest(spawned.Task, spawned.References)
	spawned.RequestedGrants = childGrants
	spawn, created, err := s.registry.Spawn(ctx, &spawned, now)
	if err != nil {
		return Spawn{}, false, err
	}
	if created {
		spawn.Grants = childGrants
	}
	if err := bindSpawnLineage(&spawn, spec); err != nil {
		return Spawn{}, false, err
	}
	return spawn, created, nil
}

func bindSpawnLineage(spawn *Spawn, spec *Spec) error {
	if spawn.ParentSessionID == "" {
		spawn.ParentSessionID = spec.ParentSessionID
	} else if spawn.ParentSessionID != spec.ParentSessionID {
		return Errorf(ErrorCodeChildConflict, "child run conflicts")
	}
	if spawn.ParentTurnID == "" {
		spawn.ParentTurnID = spec.ParentTurnID
	} else if spawn.ParentTurnID != spec.ParentTurnID {
		return Errorf(ErrorCodeChildConflict, "child run conflicts")
	}
	if spawn.ParentInvocation == "" {
		spawn.ParentInvocation = spec.ParentInvocation
	} else if spawn.ParentInvocation != spec.ParentInvocation {
		return Errorf(ErrorCodeChildConflict, "child run conflicts")
	}
	if spawn.OwnerID == "" {
		spawn.OwnerID = spec.OwnerID
	} else if spawn.OwnerID != spec.OwnerID {
		return Errorf(ErrorCodeChildConflict, "child run conflicts")
	}
	return nil
}

func ValidState(state string) bool {
	switch state {
	case StatusQueued, StatusRunning, StatusSucceeded, StatusFailed, StatusCancelled, StatusDeadline, StatusInterrupted:
		return true
	default:
		return false
	}
}
