package context

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	stdcontext "context"
)

const (
	evalProtectedRetention = 1.0
	evalTokenReduction     = 0.20
	evalKeywordRetention   = 0.80
	evalLatencyBudget      = 5 * time.Second
)

type extractiveSummarizer struct {
	producer Producer
}

func (e *extractiveSummarizer) Summarize(_ stdcontext.Context, req *SummarizeRequest) (*SummarizeResult, error) {
	if req == nil {
		return nil, Errorf(ErrorCodeInvalidArgument, "request must not be nil")
	}
	keys := []string{"goals", "decisions", "constraints", "open_work", "facts"}
	buckets := make(map[string][]string, len(keys))
	for i, source := range req.Sources {
		sentence := source
		if at := strings.Index(source, ". "); at > 0 {
			sentence = source[:at]
		}
		sentence = strings.TrimSpace(sentence)
		if sentence == "" {
			continue
		}
		key := keys[i%len(keys)]
		buckets[key] = append(buckets[key], sentence)
	}
	for _, key := range keys {
		if buckets[key] == nil {
			buckets[key] = []string{}
		}
	}
	encoded, err := json.Marshal(buckets)
	if err != nil {
		return nil, Errorf(ErrorCodeSummaryUnavailable, "extractive summary is not encodable")
	}
	return &SummarizeResult{Text: string(encoded), Producer: e.producer}, nil
}

type evalSession struct {
	name  string
	turns int
}

func evalEvents(session evalSession) (events []Event, keywords []string) {
	var sequence uint64
	for turn := range session.turns {
		sequence++
		keyword := "KW" + session.name + "-" + string(rune('A'+turn%26)) + string(rune('0'+turn/26%10))
		keywords = append(keywords, keyword)
		text := keyword + " anchors turn facts. " + strings.Repeat("session history payload ", 8)
		events = append(events, Event{Sequence: sequence, TurnID: "t-" + session.name + "-" + string(rune('a'+turn%26)) + string(rune('0'+turn/26%10)), InvocationID: "inv-old", Kind: "message.completed", Text: text})
	}
	sequence++
	events = append(events, Event{Sequence: sequence, TurnID: "t-live", InvocationID: "inv-new", Kind: "model.delta", Text: "live reasoning"})
	return events, keywords
}

func TestContextEvalCorpus(t *testing.T) {
	sessions := []evalSession{
		{name: "long", turns: 30},
		{name: "medium", turns: 12},
		{name: "short", turns: 4},
	}
	started := time.Now()
	keywordHits, keywordTotal := 0, 0
	for _, session := range sessions {
		events, keywords := evalEvents(session)
		planner, err := NewPlanner(100, 0.80, exactCounter{})
		if err != nil {
			t.Fatalf("%s: %v", session.name, err)
		}
		plan, err := planner.Plan(Capability{ID: "eval-model", ContextTokens: 3000}, Invocation{InvocationID: "inv-new"}, events)
		if err != nil {
			t.Fatalf("%s plan: %v", session.name, err)
		}
		policy, err := ResolveSelectionPolicy(2, 0.90, 64)
		if err != nil {
			t.Fatalf("%s: %v", session.name, err)
		}
		selection, err := Select(policy, plan, exactCounter{})
		if err != nil {
			t.Fatalf("%s select: %v", session.name, err)
		}
		protected := markProtected(plan.Groups, 2)
		missing, total := 0, 0
		for index := range plan.Groups {
			if !protected[index] {
				continue
			}
			total++
			found := false
			for _, part := range selection.Parts {
				if part.Kind == PartVerbatim && part.Group.Kind == plan.Groups[index].Kind && part.Group.ID == plan.Groups[index].ID {
					found = true
					break
				}
			}
			if !found {
				missing++
			}
		}
		if total > 0 {
			if retained := 1 - float64(missing)/float64(total); retained < evalProtectedRetention {
				t.Errorf("%s: protected retention %.2f below %.2f", session.name, retained, evalProtectedRetention)
			}
		}
		raw := plan.HeadTokens + plan.TotalEventTokens
		if reduction := 1 - float64(selection.UsedTokens)/float64(raw); session.name == "long" && reduction < evalTokenReduction {
			t.Errorf("%s: token reduction %.2f below %.2f", session.name, reduction, evalTokenReduction)
		}
		service, err := NewService(&extractiveSummarizer{producer: testProducer()}, newFakeSummaryStore(), "compression", "v1", 32768, 2048, exactCounter{}, nil)
		if err != nil {
			t.Fatalf("%s: %v", session.name, err)
		}
		var reachable strings.Builder
		for _, part := range selection.Parts {
			switch part.Kind {
			case PartVerbatim:
				for _, event := range part.Group.Events {
					reachable.WriteString(event.Text)
					reachable.WriteString("\n")
				}
			case PartSummary:
				summary, err := service.ResolveRange(stdcontext.Background(), "eval-"+session.name, part.Range, plan.Groups)
				if err != nil {
					t.Fatalf("%s resolve: %v", session.name, err)
				}
				if summary.Producer != testProducer() || summary.SourceDigest == "" || summary.PromptDigest == "" {
					t.Errorf("%s: summary misses provenance: %+v", session.name, summary)
				}
				encoded, err := canonicalContent(&summary.Content)
				if err != nil {
					t.Fatalf("%s: %v", session.name, err)
				}
				reachable.WriteString(encoded)
				reachable.WriteString("\n")
			}
		}
		corpus := reachable.String()
		for _, keyword := range keywords {
			keywordTotal++
			if strings.Contains(corpus, keyword) {
				keywordHits++
			} else {
				t.Errorf("%s: keyword %q unreachable", session.name, keyword)
			}
		}
	}
	if retention := float64(keywordHits) / float64(keywordTotal); retention < evalKeywordRetention {
		t.Errorf("keyword retention %.2f below %.2f", retention, evalKeywordRetention)
	}
	if elapsed := time.Since(started); elapsed > evalLatencyBudget {
		t.Errorf("eval latency %v exceeds %v", elapsed, evalLatencyBudget)
	}
	t.Logf("keywords %d/%d reduction checked latency %v", keywordHits, keywordTotal, time.Since(started))
}

func TestObservationCarriesNoContent(t *testing.T) {
	denied := map[string]bool{"text": true, "content": true, "prompt": true, "sources": true, "excerpt": true, "payload": true, "summary": true, "message": true}
	seen := map[string]bool{}
	var observations []*Observation
	service, err := NewService(&fakeSummarizer{text: validSummaryJSON(), producer: testProducer()}, newFakeSummaryStore(), "compression", "v1", 32768, 2048, exactCounter{}, nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	service.WithObserver(func(_ stdcontext.Context, observation *Observation) {
		observations = append(observations, observation)
	})
	if _, err := service.ResolveRange(stdcontext.Background(), "sess-1", summaryRange(), summaryGroups()); err != nil {
		t.Fatalf("produce: %v", err)
	}
	if _, err := service.ResolveRange(stdcontext.Background(), "sess-1", summaryRange(), summaryGroups()); err != nil {
		t.Fatalf("hit: %v", err)
	}
	if len(observations) != 2 {
		t.Fatalf("observations = %d", len(observations))
	}
	if observations[0].Outcome != OutcomeProduced || observations[1].Outcome != OutcomeCacheHit {
		t.Errorf("outcomes = %q, %q", observations[0].Outcome, observations[1].Outcome)
	}
	typ := reflect.TypeOf(Observation{})
	for i := range typ.NumField() {
		field := typ.Field(i)
		if denied[strings.ToLower(field.Name)] {
			t.Errorf("observation carries content field %q", field.Name)
		}
		seen[field.Name] = true
	}
	if !seen["Outcome"] || !seen["Duration"] {
		t.Errorf("observation misses telemetry fields")
	}
}
