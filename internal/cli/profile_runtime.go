package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/metric"
	adkmodel "google.golang.org/adk/v2/model"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/health"
	"github.com/anggasct/aura/internal/principal"
	"github.com/anggasct/aura/internal/profile"
	"github.com/anggasct/aura/internal/runtime"
	"github.com/anggasct/aura/internal/store"
	"github.com/anggasct/aura/internal/telemetry"
)

const (
	profileExtractMaxOutputTokens = 1024
	profileFailingThreshold       = 3
	profileUserAuthor             = "user"
	profileUserEventKind          = "adk_event"

	remediationRouteProfiling  = "set models.routing.profiling to a configured route (for example primary) or define models.definitions.auxiliary, then restart"
	remediationReviewProfiling = "check the profiling model route and provider health; extraction resumes on the next completed job"
)

type postTurnHook func(ctx context.Context, req *runtime.TurnRequest, events []store.RuntimeEvent) error

func composePostTurnHooks(hooks ...postTurnHook) postTurnHook {
	active := slices.DeleteFunc(slices.Clone(hooks), func(hook postTurnHook) bool { return hook == nil })
	switch len(active) {
	case 0:
		return nil
	case 1:
		return active[0]
	}
	return func(ctx context.Context, req *runtime.TurnRequest, events []store.RuntimeEvent) error {
		errs := make([]error, 0, len(active))
		for _, hook := range active {
			errs = append(errs, hook(ctx, req, events))
		}
		return errors.Join(errs...)
	}
}

type profileRuntime struct {
	service   *profile.Service
	extractor *profile.Extractor
	health    *profileHealth
	maxFacts  int
	maxTokens int
	stated    []string
}

func buildProfileRuntime(ctx context.Context, cfg *config.Config, db *sql.DB, mp metric.MeterProvider, logger *slog.Logger) (*profileRuntime, error) {
	if logger == nil {
		logger = slog.Default()
	}
	llm, producer, err := profilingLLM(ctx, cfg)
	if err != nil {
		logger.WarnContext(ctx, "profile extraction unavailable", "component", "profile", "error", err)
	}
	return newProfileRuntime(cfg, db, llm, producer, mp, buildSecretVault(cfg), logger)
}

func profilingLLM(ctx context.Context, cfg *config.Config) (adkmodel.LLM, profile.ModelProducer, error) {
	producer, err := profilingProducer(cfg)
	if err != nil {
		return nil, profile.ModelProducer{}, err
	}
	llm, err := adkmodel.NewLLM(ctx, profilingRole(cfg))
	if err != nil {
		return nil, profile.ModelProducer{}, err
	}
	return llm, producer, nil
}

// A nil llm leaves extraction off while verified facts are still served.
func newProfileRuntime(cfg *config.Config, db *sql.DB, llm adkmodel.LLM, producer profile.ModelProducer, mp metric.MeterProvider, scanner profile.SecretScanner, logger *slog.Logger) (*profileRuntime, error) {
	if cfg == nil || cfg.Profile == nil {
		return nil, errors.New("cli: profile configuration must not be nil")
	}
	if db == nil {
		return nil, errors.New("cli: profile database must not be nil")
	}
	if scanner == nil {
		return nil, errors.New("cli: profile scanner must not be nil")
	}
	rt := &profileRuntime{
		health:    &profileHealth{},
		maxFacts:  cfg.Profile.MaxContextFacts,
		maxTokens: cfg.Profile.MaxContextTokens,
		stated:    statedOwnerKeys(cfg.Owner),
	}
	recorder, err := telemetry.NewProfileRecorder(mp, profileTelemetryCallbacks(db, func() int64 {
		if rt.extractor == nil {
			return 0
		}
		return int64(rt.extractor.QueueDepth())
	}))
	if err != nil {
		return nil, err
	}
	record := profileRecorderObserver(recorder)
	observer := func(ctx context.Context, observation *profile.Observation) {
		record(ctx, observation)
		rt.health.observe(observation)
	}
	rt.service, err = profile.NewService(newProfileRegistry(db), profile.Config{
		MinConfidence: cfg.Profile.CandidateMinConfidence,
		Scanner:       scanner,
		Observer:      observer,
	})
	if err != nil {
		return nil, err
	}
	if llm == nil {
		rt.health.unavailable.Store(true)
		return rt, nil
	}
	rt.extractor, err = profile.NewExtractor(rt.service, newProfileModelCaller(llm, producer, profileExtractMaxOutputTokens), scanner,
		"profiling", cfg.Profile.PromptVersion, profileExtractMaxOutputTokens, cfg.Profile.ExtractionQueueCapacity, logger,
		profile.WithObserver(observer))
	if err != nil {
		return nil, err
	}
	return rt, nil
}

func (r *profileRuntime) start(ctx context.Context) {
	if r != nil && r.extractor != nil {
		r.extractor.Start(ctx)
	}
}

func (r *profileRuntime) check(cfg *config.Config) health.RegisteredCheck {
	return health.RegisteredCheck{
		ID:          "profile",
		Checker:     r.health,
		Timeout:     time.Duration(cfg.Health.CheckTimeout),
		Remediation: remediationReviewProfiling,
	}
}

func (r *profileRuntime) hook(ctx context.Context, req *runtime.TurnRequest, events []store.RuntimeEvent) error {
	if r == nil || r.extractor == nil || req == nil {
		return nil
	}
	if req.PrincipalClass != principal.ClassOwner || !directOwnerOrigin(req.Origin) {
		return nil
	}
	job, ok := profileJob(req.SessionID, events)
	if !ok {
		return nil
	}
	if err := r.extractor.Enqueue(ctx, job); err != nil {
		if code, ok := profile.CodeOf(err); ok && code == profile.ErrorCodeProfileUnavailable {
			return nil
		}
		return err
	}
	return nil
}

func (r *profileRuntime) ProfileContext(ctx context.Context, req *runtime.TurnRequest) (string, error) {
	if r == nil || req == nil || req.PrincipalClass != principal.ClassOwner {
		return "", nil
	}
	part, err := r.service.Retrieve(ctx, &profile.RetrieveQuery{
		OwnerID:      principal.OwnerID,
		MaxFacts:     r.maxFacts,
		MaxTokens:    r.maxTokens,
		VerifiedOnly: true,
		StatedKeys:   r.stated,
	}, time.Now().UTC())
	if err != nil {
		return "", err
	}
	return part.Render(), nil
}

// Cron and child turns resolve to the owner class but carry repeated or model-written prompts, not new owner signal.
func directOwnerOrigin(origin runtime.Origin) bool {
	return origin == runtime.OriginTerminal || strings.HasPrefix(string(origin), principal.ChannelDiscord+":")
}

func profileJob(sessionID string, events []store.RuntimeEvent) (profile.Job, bool) {
	job := profile.Job{OwnerID: principal.OwnerID, SessionID: sessionID}
	for i := range events {
		event := &events[i]
		if event.Kind != profileUserEventKind || event.Author != profileUserAuthor || event.Sequence == 0 {
			continue
		}
		if text := ownerInputText(event.Payload); text != "" {
			job.Events = append(job.Events, profile.JobEvent{ID: event.ID, Sequence: event.Sequence, Text: text})
		}
	}
	return job, strings.TrimSpace(sessionID) != "" && len(job.Events) > 0
}

// Injected evidence travels as separate parts of the owner's message; learning from it would let the bot reinforce its own context.
func ownerInputText(payload json.RawMessage) string {
	var envelope struct {
		Content *struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"content"`
		Partial bool `json:"partial"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil || envelope.Partial || envelope.Content == nil {
		return ""
	}
	var texts []string
	for _, part := range envelope.Content.Parts {
		text := strings.TrimSpace(part.Text)
		if text == "" || strings.HasPrefix(text, runtime.RecallEvidenceStart) || strings.HasPrefix(text, runtime.ProfileEvidenceStart) {
			continue
		}
		texts = append(texts, text)
	}
	return strings.Join(texts, "\n")
}

func statedOwnerKeys(owner *config.Owner) []string {
	if owner == nil {
		return nil
	}
	var keys []string
	for key, value := range map[string]string{"name": owner.Name, "language": owner.Language, "timezone": owner.Timezone} {
		if strings.TrimSpace(value) != "" {
			keys = append(keys, key)
		}
	}
	for key, value := range owner.Preferences {
		if strings.TrimSpace(key) != "" && strings.TrimSpace(value) != "" {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	return keys
}

type profileHealth struct {
	unavailable atomic.Bool
	failures    atomic.Int64
}

func (h *profileHealth) observe(observation *profile.Observation) {
	if observation == nil || observation.Kind != profile.ObserveExtraction {
		return
	}
	switch observation.Result {
	case profile.ResultCompleted:
		h.failures.Store(0)
	case profile.ResultFailed, profile.ResultParseFailed:
		h.failures.Add(1)
	}
}

func (h *profileHealth) Check(context.Context) []health.Finding {
	if h.unavailable.Load() {
		return []health.Finding{{
			Component:   "profile",
			Code:        "profile_extractor_unavailable",
			Status:      health.StatusDegraded,
			Severity:    health.SeverityWarning,
			Detail:      "the profiling model route is not configured; the bot does not learn new preferences, verified facts are still served",
			Remediation: remediationRouteProfiling,
		}}
	}
	if h.failures.Load() >= profileFailingThreshold {
		return []health.Finding{{
			Component:   "profile",
			Code:        "profile_extraction_failing",
			Status:      health.StatusDegraded,
			Severity:    health.SeverityWarning,
			Detail:      "recent profile extraction jobs failed; turns are unaffected",
			Remediation: remediationReviewProfiling,
		}}
	}
	return nil
}
