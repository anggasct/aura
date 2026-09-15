package profile

import (
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	stdcontext "context"
)

type fakeRegistry struct {
	mu       sync.Mutex
	facts    map[string]*Fact
	evidence map[string][]Evidence
}

func newFakeRegistry() *fakeRegistry {
	return &fakeRegistry{facts: make(map[string]*Fact), evidence: make(map[string][]Evidence)}
}

func (f *fakeRegistry) InsertCandidate(ctx stdcontext.Context, fact *Fact, evidence *Evidence, minConfidence float64) (Fact, error) {
	if err := ctx.Err(); err != nil {
		return Fact{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, exists := f.facts[fact.ID]; exists {
		return Fact{}, Errorf(ErrorCodeProfileConflict, "fact already exists")
	}
	stored := *fact
	for _, other := range f.facts {
		if other.OwnerID == fact.OwnerID && other.Category == fact.Category && other.Key == fact.Key && other.Status == StatusActive && other.ValueDigest != fact.ValueDigest {
			stored.ConflictsWithID = other.ID
		}
	}
	if stored.ConflictsWithID == "" && stored.Confidence >= minConfidence {
		stored.Status = StatusActive
	}
	f.facts[fact.ID] = &stored
	f.addEvidenceLocked(fact.ID, evidence)
	return stored, nil
}

func (f *fakeRegistry) addEvidenceLocked(factID string, evidence *Evidence) {
	for i := range f.evidence[factID] {
		prior := &f.evidence[factID][i]
		if prior.SourceEventID == evidence.SourceEventID && prior.SourceDigest == evidence.SourceDigest {
			return
		}
	}
	f.evidence[factID] = append(f.evidence[factID], *evidence)
}

func (f *fakeRegistry) Reinforce(ctx stdcontext.Context, factID string, evidence *Evidence, minConfidence float64) (Fact, error) {
	if err := ctx.Err(); err != nil {
		return Fact{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	stored, exists := f.facts[factID]
	if !exists {
		return Fact{}, Errorf(ErrorCodeProfileNotFound, "fact does not exist")
	}
	if stored.Status != StatusCandidate && stored.Status != StatusActive {
		return Fact{}, Errorf(ErrorCodeProfileConflict, "fact is not reinforceable")
	}
	f.addEvidenceLocked(factID, evidence)
	stored.Confidence = ConfidenceV1(len(f.evidence[factID]))
	if stored.Status == StatusCandidate && stored.Confidence >= minConfidence {
		blocked := false
		for _, other := range f.facts {
			if other.ID != stored.ID && other.OwnerID == stored.OwnerID && other.Category == stored.Category && other.Key == stored.Key && other.Status == StatusActive && other.ValueDigest != stored.ValueDigest {
				blocked = true
				stored.ConflictsWithID = other.ID
			}
		}
		if !blocked {
			stored.Status = StatusActive
		}
	}
	return *stored, nil
}

func (f *fakeRegistry) SetState(ctx stdcontext.Context, factID, status, conflictsWith string, confidence float64, verified bool, at time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	stored, exists := f.facts[factID]
	if !exists {
		return Errorf(ErrorCodeProfileNotFound, "fact does not exist")
	}
	stored.Status = status
	stored.ConflictsWithID = conflictsWith
	stored.Confidence = confidence
	stored.OwnerVerified = verified
	stored.UpdatedAt = at
	return nil
}

func (f *fakeRegistry) ReviveCandidate(ctx stdcontext.Context, factID string, at time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	stored, exists := f.facts[factID]
	if !exists {
		return Errorf(ErrorCodeProfileNotFound, "fact does not exist")
	}
	stored.Status = StatusCandidate
	stored.UpdatedAt = at
	return nil
}

func (f *fakeRegistry) Get(_ stdcontext.Context, id string) (Fact, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	stored, exists := f.facts[id]
	if !exists {
		return Fact{}, false, nil
	}
	return *stored, true, nil
}

func (f *fakeRegistry) Active(_ stdcontext.Context, ownerID, category, key string) ([]Fact, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Fact
	for _, stored := range f.facts {
		if stored.OwnerID == ownerID && stored.Category == category && stored.Key == key && stored.Status == StatusActive {
			out = append(out, *stored)
		}
	}
	return out, nil
}

func (f *fakeRegistry) Evidence(_ stdcontext.Context, factID string) ([]Evidence, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Evidence(nil), f.evidence[factID]...), nil
}

func (f *fakeRegistry) Expire(_ stdcontext.Context, now time.Time) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	count := 0
	for _, stored := range f.facts {
		if stored.ExpiresAt == nil || stored.ExpiresAt.After(now) {
			continue
		}
		if stored.Status != StatusCandidate && stored.Status != StatusActive {
			continue
		}
		stored.Status = StatusExpired
		stored.UpdatedAt = now
		count++
	}
	return count, nil
}

func testService() *Service {
	service, err := NewService(newFakeRegistry(), Config{MinConfidence: 0.70})
	if err != nil {
		panic(err)
	}
	return service
}

func testEvidence(digest string) *Evidence {
	return &Evidence{SourceEventID: "evt-" + digest, SourceDigest: digest, Provider: "p", Model: "m", PromptVersion: "v1", ObservedAt: time.Now().UTC()}
}

func TestConfidenceV1(t *testing.T) {
	cases := map[int]float64{0: 0.50, 1: 0.60, 2: 0.70, 3: 0.80, 5: 0.95, 10: 0.95}
	for count, want := range cases {
		if got := ConfidenceV1(count); got != want {
			t.Errorf("count %d = %v, want %v", count, got, want)
		}
	}
}

func TestProposeActivatesUncontested(t *testing.T) {
	service := testService()
	now := time.Now().UTC()
	first, err := service.ProposeFact(t.Context(), "owner-1", "language", "backend", "Go", testEvidence("d1"), now)
	if err != nil {
		t.Fatalf("ProposeFact: %v", err)
	}
	if first.Status != StatusCandidate || first.Confidence != 0.60 {
		t.Errorf("fact = %+v", first)
	}
	second, err := service.ProposeFact(t.Context(), "owner-1", "language", "backend", "Go", testEvidence("d2"), now)
	if err != nil {
		t.Fatalf("ProposeFact: %v", err)
	}
	if second.ID != first.ID || second.Status != StatusActive || second.Confidence != 0.70 {
		t.Errorf("fact = %+v", second)
	}
}

func TestConflictingValuesCoexist(t *testing.T) {
	service := testService()
	now := time.Now().UTC()
	goFact, err := service.ProposeFact(t.Context(), "owner-1", "language", "backend", "Go", testEvidence("d1"), now)
	if err != nil {
		t.Fatalf("ProposeFact: %v", err)
	}
	if _, err := service.ProposeFact(t.Context(), "owner-1", "language", "backend", "Go", testEvidence("d2"), now); err != nil {
		t.Fatalf("ProposeFact: %v", err)
	}
	rustFact, err := service.ProposeFact(t.Context(), "owner-1", "language", "backend", "Rust", testEvidence("d3"), now)
	if err != nil {
		t.Fatalf("ProposeFact: %v", err)
	}
	if rustFact.Status != StatusCandidate || rustFact.ConflictsWithID != goFact.ID {
		t.Errorf("fact = %+v", rustFact)
	}
	for _, digest := range []string{"d4", "d5", "d6"} {
		rustFact, err = service.ReinforceFact(t.Context(), rustFact.ID, testEvidence(digest))
		if err != nil {
			t.Fatalf("ReinforceFact: %v", err)
		}
	}
	if rustFact.Status != StatusCandidate || rustFact.Confidence != 0.90 {
		t.Errorf("blocked candidate activated: %+v", rustFact)
	}
	active, err := service.GetFact(t.Context(), goFact.ID)
	if err != nil || active.Status != StatusActive {
		t.Errorf("winner displaced: %+v, %v", active, err)
	}
}

func TestVerifiedActiveBlocksDerived(t *testing.T) {
	registry := newFakeRegistry()
	service, err := NewService(registry, Config{MinConfidence: 0.70})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	now := time.Now().UTC()
	derived, err := service.ProposeFact(t.Context(), "owner-1", "tool", "editor", "vim", testEvidence("d1"), now)
	if err != nil {
		t.Fatalf("ProposeFact: %v", err)
	}
	if err := registry.SetState(t.Context(), derived.ID, StatusActive, "", derived.Confidence, true, now); err != nil {
		t.Fatalf("SetState: %v", err)
	}
	rival, err := service.ProposeFact(t.Context(), "owner-1", "tool", "editor", "neovim", testEvidence("d2"), now)
	if err != nil {
		t.Fatalf("ProposeFact: %v", err)
	}
	if rival.Status != StatusCandidate {
		t.Errorf("rival displaced verified fact: %+v", rival)
	}
}

func TestResurrectionGuard(t *testing.T) {
	service := testService()
	now := time.Now().UTC()
	fact, err := service.ProposeFact(t.Context(), "owner-1", "habit", "standup", "async", testEvidence("d1"), now)
	if err != nil {
		t.Fatalf("ProposeFact: %v", err)
	}
	if err := service.DeleteFact(t.Context(), fact.ID, now); err != nil {
		t.Fatalf("DeleteFact: %v", err)
	}
	if _, err := service.ProposeFact(t.Context(), "owner-1", "habit", "standup", "async", testEvidence("d1"), now); err == nil {
		t.Fatalf("expected resurrection refusal")
	} else if code, ok := CodeOf(err); !ok || code != ErrorCodeProfileConflict {
		t.Fatalf("code = %v, %v (%v)", code, ok, err)
	}
	revived, err := service.ProposeFact(t.Context(), "owner-1", "habit", "standup", "async", testEvidence("d9"), now)
	if err != nil {
		t.Fatalf("ProposeFact: %v", err)
	}
	if revived.Status != StatusActive || revived.Confidence != 0.70 {
		t.Errorf("revived = %+v", revived)
	}
}

func TestDeleteTombstoneIdempotent(t *testing.T) {
	service := testService()
	now := time.Now().UTC()
	fact, err := service.ProposeFact(t.Context(), "owner-1", "project", "branching", "trunk", testEvidence("d1"), now)
	if err != nil {
		t.Fatalf("ProposeFact: %v", err)
	}
	if err := service.DeleteFact(t.Context(), fact.ID, now); err != nil {
		t.Fatalf("DeleteFact: %v", err)
	}
	if err := service.DeleteFact(t.Context(), fact.ID, now); err != nil {
		t.Fatalf("second DeleteFact: %v", err)
	}
	if err := service.DeleteFact(t.Context(), "pf-missing", now); err == nil {
		t.Errorf("expected not-found")
	} else if code, ok := CodeOf(err); !ok || code != ErrorCodeProfileNotFound {
		t.Errorf("code = %v, %v (%v)", code, ok, err)
	}
}

func TestExpireFacts(t *testing.T) {
	registry := newFakeRegistry()
	service, err := NewService(registry, Config{MinConfidence: 0.70})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	now := time.Now().UTC()
	past := now.Add(-time.Hour)
	fact, err := service.ProposeFact(t.Context(), "owner-1", "timezone", "home", "WIB", testEvidence("d1"), now)
	if err != nil {
		t.Fatalf("ProposeFact: %v", err)
	}
	if err := registry.SetState(t.Context(), fact.ID, StatusActive, "", fact.Confidence, false, now); err != nil {
		t.Fatalf("SetState: %v", err)
	}
	stored, _, _ := registry.Get(t.Context(), fact.ID)
	stored.ExpiresAt = &past
	registry.mu.Lock()
	registry.facts[fact.ID] = &stored
	registry.mu.Unlock()
	count, err := service.ExpireFacts(t.Context(), now)
	if err != nil || count != 1 {
		t.Fatalf("ExpireFacts: %d, %v", count, err)
	}
	expired, err := service.GetFact(t.Context(), fact.ID)
	if err != nil || expired.Status != StatusExpired {
		t.Errorf("fact = %+v, %v", expired, err)
	}
}

func TestProposeValidation(t *testing.T) {
	service := testService()
	now := time.Now().UTC()
	longKey := strings.Repeat("k", 129)
	for name, args := range map[string][4]string{
		"empty owner":  {"", "language", "backend", "Go"},
		"bad category": {"owner-1", "mood", "backend", "Go"},
		"empty key":    {"owner-1", "language", "", "Go"},
		"long key":     {"owner-1", "language", longKey, "Go"},
		"empty value":  {"owner-1", "language", "backend", ""},
		"bad evidence": {"owner-1", "language", "backend", "Go"},
	} {
		t.Run(name, func(t *testing.T) {
			var evidence *Evidence
			if name != "bad evidence" {
				evidence = testEvidence("d1")
			}
			if _, err := service.ProposeFact(t.Context(), args[0], args[1], args[2], args[3], evidence, now); err == nil {
				t.Errorf("expected rejection")
			}
		})
	}
}

func TestConcurrentProposeConverges(t *testing.T) {
	service := testService()
	now := time.Now().UTC()
	const workers = 8
	ids := make([]string, workers)
	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			fact, err := service.ProposeFact(stdcontext.Background(), "owner-1", "language", "backend", "Go", testEvidence("converge-"+strconv.Itoa(w)), now)
			if err == nil {
				ids[w] = fact.ID
			}
		}()
	}
	wg.Wait()
	seen := make(map[string]bool)
	for _, id := range ids {
		if id == "" {
			continue
		}
		seen[id] = true
	}
	if len(seen) != 1 {
		t.Fatalf("ids = %v", ids)
	}
	for id := range seen {
		fact, err := service.GetFact(stdcontext.Background(), id)
		if err != nil {
			t.Fatalf("GetFact: %v", err)
		}
		if fact.Status != StatusActive || fact.Confidence != 0.95 {
			t.Errorf("converged = %+v", fact)
		}
	}
}

func TestConcurrentConflictsSingleActive(t *testing.T) {
	registry := newFakeRegistry()
	service, err := NewService(registry, Config{MinConfidence: 0.70})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	now := time.Now().UTC()
	const workers = 8
	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			value := "Go"
			if w%2 == 1 {
				value = "Rust"
			}
			_, _ = service.ProposeFact(stdcontext.Background(), "owner-1", "language", "backend", value, testEvidence("worker-"+strconv.Itoa(w)), now)
		}()
	}
	wg.Wait()
	actives, err := registry.Active(stdcontext.Background(), "owner-1", "language", "backend")
	if err != nil {
		t.Fatalf("Active: %v", err)
	}
	if len(actives) > 1 {
		t.Fatalf("dual active facts: %+v", actives)
	}
}
