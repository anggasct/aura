package profile

import (
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	stdcontext "context"
)

type ModelProducer struct {
	Provider     string
	Model        string
	ModelVersion string
}

type ExtractSource struct {
	Sequence uint64
	EventID  string
	Text     string
}

type ExtractRequest struct {
	Task            string
	Prompt          string
	Sources         []ExtractSource
	MaxOutputTokens int
}

type ExtractResult struct {
	Text     string
	Producer ModelProducer
}

type ModelCaller interface {
	Extract(ctx stdcontext.Context, req *ExtractRequest) (*ExtractResult, error)
}

type JobEvent struct {
	ID       string
	Sequence uint64
	Text     string
}

type Job struct {
	OwnerID   string
	SessionID string
	Events    []JobEvent
}

type queuedJob struct {
	job        Job
	enqueuedAt time.Time
}

type JobOutcome struct {
	Accepted int
	Screened int
	Skipped  int
	Attempt  int
	Dropped  bool
	Failed   bool
}

type Extractor struct {
	service         *Service
	caller          ModelCaller
	scanner         SecretScanner
	task            string
	prompt          string
	promptVersion   string
	promptDigest    string
	maxOutputTokens int
	queue           chan queuedJob
	logger          *slog.Logger
	dropped         atomic.Int64
	observer        Observer
}

type ExtractorOption func(*Extractor)

func WithObserver(observer Observer) ExtractorOption {
	return func(e *Extractor) {
		e.observer = observer
	}
}

func NewExtractor(service *Service, caller ModelCaller, scanner SecretScanner, task, promptVersion string, maxOutputTokens, queueCapacity int, logger *slog.Logger, opts ...ExtractorOption) (*Extractor, error) {
	if service == nil {
		return nil, errNilArgument("service")
	}
	if caller == nil {
		return nil, errNilArgument("caller")
	}
	if scanner == nil {
		return nil, errNilArgument("scanner")
	}
	if strings.TrimSpace(task) == "" {
		return nil, Errorf(ErrorCodeInvalidArgument, "extraction task must not be empty")
	}
	if maxOutputTokens <= 0 {
		return nil, Errorf(ErrorCodeInvalidArgument, "output bound must be positive")
	}
	if queueCapacity <= 0 {
		return nil, Errorf(ErrorCodeInvalidArgument, "queue capacity must be positive")
	}
	if logger == nil {
		logger = slog.Default()
	}
	prompt, err := ExtractPrompt(promptVersion, maxOutputTokens)
	if err != nil {
		return nil, err
	}
	extractor := &Extractor{
		service:         service,
		caller:          caller,
		scanner:         scanner,
		task:            task,
		prompt:          prompt,
		promptVersion:   promptVersion,
		promptDigest:    ValueDigest(prompt),
		maxOutputTokens: maxOutputTokens,
		queue:           make(chan queuedJob, queueCapacity),
		logger:          logger,
	}
	for _, opt := range opts {
		opt(extractor)
	}
	return extractor, nil
}

func (e *Extractor) QueueDepth() int {
	return len(e.queue)
}

func (e *Extractor) DroppedJobs() int64 {
	return e.dropped.Load()
}

func (e *Extractor) Enqueue(job Job) error {
	if strings.TrimSpace(job.OwnerID) == "" || strings.TrimSpace(job.SessionID) == "" || len(job.Events) == 0 {
		return Errorf(ErrorCodeInvalidArgument, "extraction job is not complete")
	}
	seen := make(map[uint64]bool, len(job.Events))
	for _, event := range job.Events {
		if strings.TrimSpace(event.ID) == "" || event.Sequence == 0 {
			return Errorf(ErrorCodeInvalidArgument, "extraction job carries an unidentified event")
		}
		if seen[event.Sequence] {
			return Errorf(ErrorCodeInvalidArgument, "extraction job carries a duplicate sequence")
		}
		seen[event.Sequence] = true
	}
	ticket := queuedJob{job: job, enqueuedAt: time.Now().UTC()}
	select {
	case e.queue <- ticket:
		return nil
	default:
		e.dropped.Add(1)
		observeWith(stdcontext.Background(), e.observer, &Observation{
			Kind:   ObserveExtraction,
			Result: ResultDropped,
		})
		e.logger.WarnContext(stdcontext.Background(), "profile extraction queue is full", "component", "profile")
		return Errorf(ErrorCodeProfileUnavailable, "extraction queue is full")
	}
}

func (e *Extractor) Start(ctx stdcontext.Context) {
	if ctx == nil {
		return
	}
	go e.run(ctx)
}

func (e *Extractor) run(ctx stdcontext.Context) {
	for {
		select {
		case <-ctx.Done():
			pending := len(e.queue)
			if pending > 0 {
				e.logger.WarnContext(ctx, "profile extraction stops with pending jobs", "component", "profile", "pending", pending)
			}
			return
		case ticket := <-e.queue:
			queueAge := time.Since(ticket.enqueuedAt)
			if queueAge < 0 {
				queueAge = 0
			}
			e.handle(ctx, ticket.job, queueAge)
		}
	}
}

func (e *Extractor) handle(ctx stdcontext.Context, job Job, queueAge time.Duration) {
	outcome := JobOutcome{}
	sources, bySequence := boundSources(job.Events)
	result, attempts, err := e.callWithRetry(ctx, sources)
	outcome.Attempt = attempts
	if err != nil {
		outcome.Failed = true
		e.logger.WarnContext(ctx, "profile extraction failed", "component", "profile")
		e.finish(ctx, outcome, ResultFailed, queueAge)
		return
	}
	if strings.TrimSpace(result.Producer.Model) == "" {
		outcome.Failed = true
		e.finish(ctx, outcome, ResultFailed, queueAge)
		return
	}
	candidates, err := ParseCandidates(result.Text)
	if err != nil {
		outcome.Failed = true
		e.finish(ctx, outcome, ResultParseFailed, queueAge)
		return
	}
	now := time.Now().UTC()
	for _, candidate := range candidates {
		if reason := SensitiveReason(candidate.Category, candidate.Key, candidate.Value, e.scanner); reason != "" {
			outcome.Screened++
			continue
		}
		if err := checkProposal(job.OwnerID, candidate.Category, candidate.Key, candidate.Value); err != nil {
			outcome.Skipped++
			continue
		}
		evidence := 0
		for _, sequence := range candidate.Sources {
			event, ok := bySequence[sequence]
			if !ok {
				continue
			}
			_, err := e.service.ProposeFact(ctx, job.OwnerID, candidate.Category, candidate.Key, candidate.Value, &Evidence{
				SourceEventID: event.ID,
				SourceDigest:  ValueDigest(event.Text),
				Provider:      result.Producer.Provider,
				Model:         result.Producer.Model,
				ModelVersion:  result.Producer.ModelVersion,
				PromptVersion: e.promptVersion,
				PromptDigest:  e.promptDigest,
				ObservedAt:    now,
			}, now)
			if err != nil {
				if code, ok := CodeOf(err); ok && code == ErrorCodeProfileConflict {
					continue
				}
				break
			}
			evidence++
		}
		if evidence > 0 {
			outcome.Accepted++
		} else {
			outcome.Skipped++
		}
	}
	e.finish(ctx, outcome, ResultCompleted, queueAge)
}

func (e *Extractor) callWithRetry(ctx stdcontext.Context, sources []ExtractSource) (*ExtractResult, int, error) {
	var err error
	var result *ExtractResult
	attempts := 0
	for attempt := 1; attempt <= extractorMaxAttempts; attempt++ {
		attempts = attempt
		result, err = e.caller.Extract(ctx, &ExtractRequest{
			Task:            e.task,
			Prompt:          e.prompt,
			Sources:         sources,
			MaxOutputTokens: e.maxOutputTokens,
		})
		if err == nil && result != nil {
			return result, attempts, nil
		}
		if ctx.Err() != nil {
			return nil, attempts, ctx.Err()
		}
		if attempt < extractorMaxAttempts {
			timer := time.NewTimer(time.Duration(attempt) * 50 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, attempts, ctx.Err()
			case <-timer.C:
			}
		}
	}
	if err == nil {
		return nil, attempts, Errorf(ErrorCodeProfileUnavailable, "extractor returned no result")
	}
	return nil, attempts, err
}

func boundSources(events []JobEvent) (sources []ExtractSource, bySequence map[uint64]JobEvent) {
	bySequence = make(map[uint64]JobEvent, len(events))
	sources = make([]ExtractSource, 0, len(events))
	total := 0
	for _, event := range events {
		text := truncateSource(event.Text, maxSourceEventChars)
		if total+len(text) > maxSourceTotalChars {
			continue
		}
		total += len(text)
		bySequence[event.Sequence] = event
		sources = append(sources, ExtractSource{Sequence: event.Sequence, EventID: event.ID, Text: text})
	}
	return sources, bySequence
}

func (e *Extractor) finish(ctx stdcontext.Context, outcome JobOutcome, result string, queueAge time.Duration) {
	e.logger.DebugContext(ctx, "profile extraction job done",
		"component", "profile",
		"accepted", outcome.Accepted,
		"screened", outcome.Screened,
		"skipped", outcome.Skipped,
		"attempt", outcome.Attempt,
		"failed", outcome.Failed,
	)
	observeWith(ctx, e.observer, &Observation{
		Kind:     ObserveExtraction,
		Result:   result,
		QueueAge: queueAge,
		Accepted: outcome.Accepted,
		Screened: outcome.Screened,
		Skipped:  outcome.Skipped,
	})
}
