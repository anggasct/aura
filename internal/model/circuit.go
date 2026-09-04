package model

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/anggasct/aura/internal/config"
)

type CircuitState string

const (
	CircuitStateClosed   CircuitState = "closed"
	CircuitStateOpen     CircuitState = "open"
	CircuitStateHalfOpen CircuitState = "half_open"
)

type CircuitStatus struct {
	Key                 string       `json:"key"`
	DefinitionID        string       `json:"definition_id"`
	Endpoint            string       `json:"endpoint"`
	State               CircuitState `json:"state"`
	ConsecutiveFailures int          `json:"consecutive_failures"`
	OpenUntil           *time.Time   `json:"open_until,omitempty"`
	AuthFailed          bool         `json:"auth_failed"`
	ConfigDigest        string       `json:"config_digest"`
	UpdatedAt           time.Time    `json:"updated_at"`
}

type CircuitPolicy struct {
	FailureThreshold int
	OpenDuration     time.Duration
	MaxOpenDuration  time.Duration
}

func DefaultCircuitPolicy() CircuitPolicy {
	return CircuitPolicy{
		FailureThreshold: 3,
		OpenDuration:     5 * time.Minute,
		MaxOpenDuration:  time.Hour,
	}
}

func FormatCircuitKey(definitionID, endpoint string) string {
	cleaned := strings.TrimRight(endpoint, "/")
	if cleaned == "" {
		return definitionID
	}
	return definitionID + "|" + cleaned
}

func ComputeConfigDigest(def *config.ModelDefinition) string {
	if def == nil {
		return ""
	}
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "%s:%s:%s:%d:%s",
		def.Protocol,
		def.Model,
		strings.TrimRight(def.BaseURL, "/"),
		def.Capabilities.ContextTokens,
		def.Capabilities.Tokenizer,
	)
	return hex.EncodeToString(h.Sum(nil))[:16]
}

type CircuitCheckpoint struct {
	CircuitKey          string
	ConfigDigest        string
	State               CircuitState
	ConsecutiveFailures int
	OpenUntil           *time.Time
	UpdatedAt           time.Time
}

type CircuitCheckpointStore interface {
	Save(ctx context.Context, cp *CircuitCheckpoint) error
	Load(ctx context.Context) ([]CircuitCheckpoint, error)
	Delete(ctx context.Context, circuitKey string) error
}

type circuitEntry struct {
	key                 string
	definitionID        string
	endpoint            string
	configDigest        string
	policy              CircuitPolicy
	state               CircuitState
	consecutiveFailures int
	openUntil           time.Time
	currentOpenDuration time.Duration
	authFailed          bool
	probeActive         bool
	updatedAt           time.Time
}

type CircuitManager struct {
	mu      sync.Mutex
	now     func() time.Time
	store   CircuitCheckpointStore
	entries map[string]*circuitEntry
	logger  *slog.Logger
}

func NewCircuitManager(nowFn func() time.Time, store CircuitCheckpointStore) *CircuitManager {
	if nowFn == nil {
		nowFn = time.Now
	}
	return &CircuitManager{
		now:     nowFn,
		store:   store,
		entries: make(map[string]*circuitEntry),
	}
}

func (m *CircuitManager) WithLogger(logger *slog.Logger) *CircuitManager {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.logger = logger
	return m
}

func (m *CircuitManager) Register(definitionID, endpoint, configDigest string, policy CircuitPolicy) {
	m.mu.Lock()
	defer m.mu.Unlock()

	key := FormatCircuitKey(definitionID, endpoint)
	if policy.FailureThreshold <= 0 {
		policy.FailureThreshold = 3
	}
	if policy.OpenDuration <= 0 {
		policy.OpenDuration = 5 * time.Minute
	}
	if policy.MaxOpenDuration <= 0 {
		policy.MaxOpenDuration = time.Hour
	}
	if policy.MaxOpenDuration < policy.OpenDuration {
		policy.MaxOpenDuration = policy.OpenDuration
	}

	entry, exists := m.entries[key]
	if !exists {
		m.entries[key] = &circuitEntry{
			key:                 key,
			definitionID:        definitionID,
			endpoint:            endpoint,
			configDigest:        configDigest,
			policy:              policy,
			state:               CircuitStateClosed,
			currentOpenDuration: policy.OpenDuration,
			updatedAt:           m.now(),
		}
		return
	}

	entry.policy = policy
	if entry.configDigest != configDigest {
		// Config changed: reset circuit state to fresh closed state per spec
		entry.configDigest = configDigest
		entry.state = CircuitStateClosed
		entry.consecutiveFailures = 0
		entry.openUntil = time.Time{}
		entry.currentOpenDuration = policy.OpenDuration
		entry.authFailed = false
		entry.probeActive = false
		entry.updatedAt = m.now()
	}
}

func (m *CircuitManager) Allow(key string) (allowed, isProbe bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	entry, ok := m.findEntryLocked(key)
	if !ok {
		// Unregistered candidate defaults to closed/allowed
		return true, false
	}

	now := m.now()
	switch entry.state {
	case CircuitStateClosed:
		return true, false
	case CircuitStateOpen:
		if entry.authFailed {
			return false, false
		}
		if now.Before(entry.openUntil) {
			return false, false
		}
		// Open duration expired: transition to half-open
		entry.state = CircuitStateHalfOpen
		entry.updatedAt = now
		entry.probeActive = true
		return true, true
	case CircuitStateHalfOpen:
		if entry.probeActive {
			return false, false
		}
		entry.probeActive = true
		return true, true
	default:
		return true, false
	}
}

func (m *CircuitManager) RecordSuccess(ctx context.Context, key string) {
	m.mu.Lock()
	entry, ok := m.findEntryLocked(key)
	if !ok {
		m.mu.Unlock()
		return
	}

	now := m.now()
	prevState := entry.state
	entry.state = CircuitStateClosed
	entry.consecutiveFailures = 0
	entry.openUntil = time.Time{}
	entry.currentOpenDuration = entry.policy.OpenDuration
	entry.probeActive = false
	entry.authFailed = false
	entry.updatedAt = now

	cp := m.checkpointFromEntryLocked(entry)
	logger := m.logger
	key = entry.key
	m.mu.Unlock()

	if logger != nil && prevState == CircuitStateHalfOpen {
		logger.InfoContext(ctx, "circuit closed after successful probe",
			"circuit_key", key,
		)
	}

	m.persistCheckpoint(ctx, &cp)
}

func (m *CircuitManager) RecordFailure(ctx context.Context, key string, class ErrorClass) {
	m.mu.Lock()
	entry, ok := m.findEntryLocked(key)
	if !ok {
		m.mu.Unlock()
		return
	}

	now := m.now()

	if class == ErrorClassAuth {
		entry.state = CircuitStateOpen
		entry.authFailed = true
		entry.probeActive = false
		entry.openUntil = now.Add(365 * 24 * time.Hour)
		entry.updatedAt = now
		cp := m.checkpointFromEntryLocked(entry)
		logger := m.logger
		circuitKey := entry.key
		m.mu.Unlock()
		if logger != nil {
			logger.ErrorContext(ctx, "circuit locked open due to auth failure",
				"circuit_key", circuitKey,
				"decision", "auth_locked",
			)
		}
		m.persistCheckpoint(ctx, &cp)
		return
	}

	entry.consecutiveFailures++
	var transition string

	switch entry.state {
	case CircuitStateHalfOpen:
		// Probe failed: reopen with backoff
		entry.state = CircuitStateOpen
		entry.probeActive = false
		entry.currentOpenDuration *= 2
		if entry.currentOpenDuration > entry.policy.MaxOpenDuration {
			entry.currentOpenDuration = entry.policy.MaxOpenDuration
		}
		entry.openUntil = now.Add(entry.currentOpenDuration)
		entry.updatedAt = now
		transition = "probe_failed"
	case CircuitStateClosed:
		if entry.consecutiveFailures >= entry.policy.FailureThreshold {
			entry.state = CircuitStateOpen
			entry.currentOpenDuration = entry.policy.OpenDuration
			entry.openUntil = now.Add(entry.currentOpenDuration)
			entry.updatedAt = now
			transition = "opened"
		}
	case CircuitStateOpen:
		// Already open; update timestamp
		entry.updatedAt = now
	}

	cp := m.checkpointFromEntryLocked(entry)
	logger := m.logger
	circuitKey := entry.key
	failures := entry.consecutiveFailures
	m.mu.Unlock()

	if logger != nil && transition != "" {
		logger.WarnContext(ctx, "circuit state transition",
			"circuit_key", circuitKey,
			"transition", transition,
			"consecutive_failures", failures,
		)
	}

	m.persistCheckpoint(ctx, &cp)
}

func (m *CircuitManager) Reset(ctx context.Context, definitionIDOrKey string) bool {
	m.mu.Lock()
	entry, ok := m.findEntryLocked(definitionIDOrKey)
	if !ok {
		m.mu.Unlock()
		return false
	}

	now := m.now()
	entry.state = CircuitStateClosed
	entry.consecutiveFailures = 0
	entry.openUntil = time.Time{}
	entry.currentOpenDuration = entry.policy.OpenDuration
	entry.probeActive = false
	entry.authFailed = false
	entry.updatedAt = now

	key := entry.key
	logger := m.logger
	m.mu.Unlock()

	if logger != nil {
		logger.InfoContext(ctx, "circuit reset",
			"circuit_key", key,
		)
	}

	if m.store != nil {
		_ = m.store.Delete(ctx, key)
	}
	return true
}

func (m *CircuitManager) Inspect() []CircuitStatus {
	m.mu.Lock()
	defer m.mu.Unlock()

	statuses := make([]CircuitStatus, 0, len(m.entries))
	for _, key := range slices.Sorted(maps.Keys(m.entries)) {
		entry := m.entries[key]
		var openUntil *time.Time
		if !entry.openUntil.IsZero() {
			t := entry.openUntil
			openUntil = &t
		}
		statuses = append(statuses, CircuitStatus{
			Key:                 entry.key,
			DefinitionID:        entry.definitionID,
			Endpoint:            entry.endpoint,
			State:               entry.state,
			ConsecutiveFailures: entry.consecutiveFailures,
			OpenUntil:           openUntil,
			AuthFailed:          entry.authFailed,
			ConfigDigest:        entry.configDigest,
			UpdatedAt:           entry.updatedAt,
		})
	}
	return statuses
}

func (m *CircuitManager) LoadCheckpoints(ctx context.Context) error {
	if m.store == nil {
		return nil
	}
	checkpoints, err := m.store.Load(ctx)
	if err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	now := m.now()
	for _, cp := range checkpoints {
		entry, ok := m.entries[cp.CircuitKey]
		if !ok {
			continue
		}
		// Stale checkpoint check: ignore if config digest changed
		if cp.ConfigDigest != entry.configDigest {
			continue
		}

		entry.consecutiveFailures = cp.ConsecutiveFailures
		entry.updatedAt = cp.UpdatedAt

		if cp.State == CircuitStateOpen {
			if cp.OpenUntil != nil && !cp.OpenUntil.IsZero() {
				if now.After(*cp.OpenUntil) {
					entry.state = CircuitStateHalfOpen
					entry.openUntil = time.Time{}
				} else {
					entry.state = CircuitStateOpen
					entry.openUntil = *cp.OpenUntil
				}
			} else {
				entry.state = CircuitStateOpen
			}
		} else {
			entry.state = cp.State
		}
	}
	return nil
}

func (m *CircuitManager) findEntryLocked(key string) (*circuitEntry, bool) {
	if entry, ok := m.entries[key]; ok {
		return entry, true
	}
	for _, entry := range m.entries {
		if entry.definitionID == key {
			return entry, true
		}
	}
	return nil, false
}

func (m *CircuitManager) checkpointFromEntryLocked(entry *circuitEntry) CircuitCheckpoint {
	var openUntil *time.Time
	if !entry.openUntil.IsZero() {
		t := entry.openUntil
		openUntil = &t
	}
	return CircuitCheckpoint{
		CircuitKey:          entry.key,
		ConfigDigest:        entry.configDigest,
		State:               entry.state,
		ConsecutiveFailures: entry.consecutiveFailures,
		OpenUntil:           openUntil,
		UpdatedAt:           entry.updatedAt,
	}
}

func (m *CircuitManager) persistCheckpoint(ctx context.Context, cp *CircuitCheckpoint) {
	if m.store == nil || cp == nil {
		return
	}
	_ = m.store.Save(ctx, cp)
}
