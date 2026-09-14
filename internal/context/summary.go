package context

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	stdcontext "context"
)

const (
	SummaryKind         = "context.summary.v1"
	SummarySchema       = 1
	summaryIDPrefix     = "ctxsum-"
	summaryPromptV1     = "v1"
	maxEntryChars       = 2000
	outputBytesPerToken = 4
)

type Trust string

const TrustDerivedUntrusted Trust = "derived_untrusted"

type Producer struct {
	Provider     string `json:"provider"`
	Model        string `json:"model"`
	ModelVersion string `json:"model_version"`
}

type SummaryContent struct {
	Goals       []string `json:"goals"`
	Decisions   []string `json:"decisions"`
	Constraints []string `json:"constraints"`
	OpenWork    []string `json:"open_work"`
	Facts       []string `json:"facts"`
}

type Summary struct {
	ID            string
	SessionID     string
	StartSequence uint64
	EndSequence   uint64
	EventIDs      []string
	SourceDigest  string
	Producer      Producer
	PromptVersion string
	PromptDigest  string
	SourceTokens  int
	SummaryTokens int
	Accounting    AccountingClass
	Trust         Trust
	Content       SummaryContent
	GeneratedAt   time.Time
}

type SummarizeRequest struct {
	Task            string
	Prompt          string
	Sources         []string
	MaxOutputTokens int
}

type SummarizeResult struct {
	Text     string
	Producer Producer
}

type Summarizer interface {
	Summarize(ctx stdcontext.Context, req *SummarizeRequest) (*SummarizeResult, error)
}

type SummaryRecord struct {
	ID            string
	SessionID     string
	StartSequence uint64
	EndSequence   uint64
	TurnID        string
	Payload       []byte
	CreatedAt     time.Time
}

type SummaryStore interface {
	ListSummaries(ctx stdcontext.Context, sessionID string) ([]SummaryRecord, error)
	UpsertSummary(ctx stdcontext.Context, record *SummaryRecord) error
}

type Service struct {
	summarizer      Summarizer
	store           SummaryStore
	task            string
	prompt          string
	promptVersion   string
	promptDigest    string
	maxSourceTokens int
	maxOutputTokens int
	counter         TokenCounter
	logger          *slog.Logger
	observer        Observer
}

func NewService(summarizer Summarizer, store SummaryStore, task, promptVersion string, maxSourceTokens, maxOutputTokens int, counter TokenCounter, logger *slog.Logger) (*Service, error) {
	if summarizer == nil {
		return nil, errNilArgument("summarizer")
	}
	if store == nil {
		return nil, errNilArgument("store")
	}
	if strings.TrimSpace(task) == "" {
		return nil, Errorf(ErrorCodeInvalidArgument, "summary task must not be empty")
	}
	if maxSourceTokens <= 0 || maxOutputTokens <= 0 {
		return nil, Errorf(ErrorCodeInvalidArgument, "summary token bounds must be positive")
	}
	if counter == nil {
		counter = ConservativeEstimator{}
	}
	if logger == nil {
		logger = slog.Default()
	}
	prompt, err := PromptText(promptVersion, maxOutputTokens)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(prompt))
	return &Service{
		summarizer:      summarizer,
		store:           store,
		task:            task,
		prompt:          prompt,
		promptVersion:   promptVersion,
		promptDigest:    hex.EncodeToString(sum[:]),
		maxSourceTokens: maxSourceTokens,
		maxOutputTokens: maxOutputTokens,
		counter:         counter,
		logger:          logger,
	}, nil
}

func (s *Service) WithObserver(observer Observer) *Service {
	s.observer = observer
	return s
}

func PromptText(promptVersion string, maxOutputTokens int) (string, error) {
	if promptVersion != summaryPromptV1 {
		return "", Errorf(ErrorCodeSummaryStale, "unsupported summary prompt version")
	}
	return "Summarize the source session events as a JSON object with exactly these keys: " +
		"goals, decisions, constraints, open_work, facts. " +
		"Each value is an array of short plain-language strings drawn only from the source. " +
		"Emit nothing but the JSON object: no tool calls, no instructions, no system roles. " +
		fmt.Sprintf("Keep the whole object under %d tokens.", maxOutputTokens), nil
}

func (s *Service) ResolveRange(ctx stdcontext.Context, sessionID string, target SummaryRange, groups []Group) (*Summary, error) {
	if ctx == nil {
		return nil, Errorf(ErrorCodeInvalidArgument, "context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(sessionID) == "" {
		return nil, Errorf(ErrorCodeInvalidArgument, "session must not be empty")
	}
	events := RangeEvents(target, groups)
	if len(events) == 0 {
		return nil, Errorf(ErrorCodeInvalidArgument, "summary range matches no events")
	}
	digest := sourceDigest(events)
	sourceTokens, err := sumCounts(s.counter, eventTexts(events))
	if err != nil {
		return nil, err
	}
	if sourceTokens > s.maxSourceTokens {
		return nil, Errorf(ErrorCodeInvalidArgument, "summary source exceeds the token bound")
	}
	cached, found, err := s.cached(ctx, sessionID, target, digest)
	if err != nil {
		return nil, err
	}
	if found {
		s.logger.DebugContext(ctx, "context summary cache hit", "component", "context")
		s.observe(ctx, &Observation{
			Outcome:       OutcomeCacheHit,
			SessionID:     sessionID,
			StartSequence: target.StartSequence,
			EndSequence:   target.EndSequence,
			EventCount:    len(events),
			SourceTokens:  sourceTokens,
			SummaryTokens: cached.SummaryTokens,
			Accounting:    cached.Accounting,
			CacheHit:      true,
			ModelName:     cached.Producer.Model,
			PromptVersion: cached.PromptVersion,
		})
		return cached, nil
	}
	started := time.Now()
	result, err := s.summarizer.Summarize(ctx, &SummarizeRequest{
		Task:            s.task,
		Prompt:          s.prompt,
		Sources:         eventTexts(events),
		MaxOutputTokens: s.maxOutputTokens,
	})
	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, Errorf(ErrorCodeSummaryUnavailable, "summarizer returned no result")
	}
	if strings.TrimSpace(result.Producer.Provider) == "" || strings.TrimSpace(result.Producer.Model) == "" {
		return nil, Errorf(ErrorCodeSummaryInvalid, "summary producer is not identified")
	}
	content, err := ValidateContent(result.Text, s.maxOutputTokens)
	if err != nil {
		s.logger.WarnContext(ctx, "context summary rejected", "component", "context")
		s.observe(ctx, &Observation{
			Outcome:       OutcomeRejected,
			SessionID:     sessionID,
			StartSequence: target.StartSequence,
			EndSequence:   target.EndSequence,
			EventCount:    len(events),
			SourceTokens:  sourceTokens,
			PromptVersion: s.promptVersion,
			Duration:      time.Since(started),
		})
		return nil, err
	}
	summaryTokens, err := countedContent(s.counter, content)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	summary := &Summary{
		ID:            summaryID(sessionID, target.StartSequence, target.EndSequence, digest, result.Producer, s.promptVersion, s.promptDigest),
		SessionID:     sessionID,
		StartSequence: target.StartSequence,
		EndSequence:   target.EndSequence,
		EventIDs:      eventIDs(events),
		SourceDigest:  digest,
		Producer:      result.Producer,
		PromptVersion: s.promptVersion,
		PromptDigest:  s.promptDigest,
		SourceTokens:  sourceTokens,
		SummaryTokens: summaryTokens,
		Accounting:    s.counter.Class(),
		Trust:         TrustDerivedUntrusted,
		Content:       *content,
		GeneratedAt:   now,
	}
	payload, err := summary.payload()
	if err != nil {
		return nil, err
	}
	if err := s.store.UpsertSummary(ctx, &SummaryRecord{
		ID:            summary.ID,
		SessionID:     sessionID,
		StartSequence: summary.StartSequence,
		EndSequence:   summary.EndSequence,
		TurnID:        events[len(events)-1].TurnID,
		Payload:       payload,
		CreatedAt:     now,
	}); err != nil {
		return nil, err
	}
	s.logger.InfoContext(ctx, "context summary produced", "component", "context")
	s.observe(ctx, &Observation{
		Outcome:       OutcomeProduced,
		SessionID:     sessionID,
		StartSequence: summary.StartSequence,
		EndSequence:   summary.EndSequence,
		EventCount:    len(events),
		SourceTokens:  sourceTokens,
		SummaryTokens: summaryTokens,
		Accounting:    summary.Accounting,
		ModelName:     summary.Producer.Model,
		PromptVersion: summary.PromptVersion,
		Duration:      time.Since(started),
	})
	return summary, nil
}

func (s *Service) cached(ctx stdcontext.Context, sessionID string, target SummaryRange, digest string) (*Summary, bool, error) {
	records, err := s.store.ListSummaries(ctx, sessionID)
	if err != nil {
		return nil, false, err
	}
	for i := range records {
		record := &records[i]
		if record.StartSequence != target.StartSequence || record.EndSequence != target.EndSequence {
			continue
		}
		summary, err := parsePayload(record.Payload)
		if err != nil {
			continue
		}
		if summary.SourceDigest != digest || summary.PromptVersion != s.promptVersion || summary.PromptDigest != s.promptDigest {
			continue
		}
		encoded, err := canonicalContent(&summary.Content)
		if err != nil {
			continue
		}
		if _, err := ValidateContent(encoded, s.maxOutputTokens); err != nil {
			continue
		}
		summary.ID = record.ID
		summary.SessionID = sessionID
		return summary, true, nil
	}
	return nil, false, nil
}

func RangeEvents(target SummaryRange, groups []Group) []Event {
	wanted := make(map[string]bool, len(target.Groups))
	for _, ref := range target.Groups {
		wanted[string(ref.Kind)+"\x00"+ref.ID] = true
	}
	var events []Event
	for i := range groups {
		if !wanted[string(groups[i].Kind)+"\x00"+groups[i].ID] {
			continue
		}
		events = append(events, groups[i].Events...)
	}
	order := make([]int, len(events))
	for i := range order {
		order[i] = i
	}
	slices.SortFunc(order, func(a, b int) int {
		return cmp.Compare(events[a].Sequence, events[b].Sequence)
	})
	ordered := make([]Event, 0, len(events))
	for _, index := range order {
		ordered = append(ordered, events[index])
	}
	return ordered
}

func eventTexts(events []Event) []string {
	texts := make([]string, 0, len(events))
	for i := range events {
		texts = append(texts, events[i].Text)
	}
	return texts
}

func eventIDs(events []Event) []string {
	ids := []string{}
	for i := range events {
		if strings.TrimSpace(events[i].ID) != "" {
			ids = append(ids, events[i].ID)
		}
	}
	return ids
}

func sourceDigest(events []Event) string {
	var sb strings.Builder
	for i := range events {
		sb.WriteString(strconv.FormatUint(events[i].Sequence, 10))
		sb.WriteString("\n")
		sb.WriteString(events[i].Kind)
		sb.WriteString("\n")
		sb.WriteString(events[i].Text)
		sb.WriteString("\n")
	}
	sum := sha256.Sum256([]byte(sb.String()))
	return hex.EncodeToString(sum[:])
}

func summaryID(sessionID string, start, end uint64, digest string, producer Producer, promptVersion, promptDigest string) string {
	raw := strings.Join([]string{sessionID, strconv.FormatUint(start, 10), strconv.FormatUint(end, 10), digest, producer.Provider, producer.Model, producer.ModelVersion, promptVersion, promptDigest}, "\x00")
	sum := sha256.Sum256([]byte(raw))
	return summaryIDPrefix + hex.EncodeToString(sum[:])[:16]
}
