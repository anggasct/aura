package model

import (
	"context"
	"fmt"
	"iter"
	"log/slog"
	"strings"
	"time"

	adkmodel "google.golang.org/adk/v2/model"

	"github.com/anggasct/aura/internal/config"
)

type AdapterResolver interface {
	GetAdapter(name string) (adkmodel.LLM, error)
}

type MapAdapterResolver map[string]adkmodel.LLM

func (m MapAdapterResolver) GetAdapter(name string) (adkmodel.LLM, error) {
	a, ok := m[name]
	if !ok || a == nil {
		return nil, newError(ErrorCodeNotFound, name, "", fmt.Sprintf("adapter %q not found", name))
	}
	return a, nil
}

type candidateAttemptError struct {
	Candidate string
	Class     ErrorClass
	Err       error
}

type FallbackAdapter struct {
	name        string
	route       config.ModelRoute
	definitions map[string]config.ModelDefinition
	circuits    *CircuitManager
	resolver    AdapterResolver
	logger      *slog.Logger
}

func NewFallbackAdapter(name string, route config.ModelRoute, definitions map[string]config.ModelDefinition, circuits *CircuitManager, resolver AdapterResolver) *FallbackAdapter {
	return &FallbackAdapter{
		name:        name,
		route:       route,
		definitions: definitions,
		circuits:    circuits,
		resolver:    resolver,
	}
}

func (f *FallbackAdapter) WithLogger(logger *slog.Logger) *FallbackAdapter {
	f.logger = logger
	return f
}

func (f *FallbackAdapter) Name() string {
	return f.name
}

func (f *FallbackAdapter) GenerateContent(ctx context.Context, req *adkmodel.LLMRequest, stream bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		reqCtx := ctx
		budget := BudgetFromContext(ctx)
		if budget == nil {
			var bErr error
			maxAttempts := f.route.MaxProviderAttempts
			if maxAttempts <= 0 {
				maxAttempts = 4
			}
			delayBudget := time.Duration(f.route.RetryDelayBudget)
			if delayBudget <= 0 {
				delayBudget = 20 * time.Second
			}
			budget, bErr = NewInvocationBudget(BudgetParams{
				MaxAttempts:      maxAttempts,
				RetryDelayBudget: delayBudget,
				CostCeilingUSD:   f.route.CostBudgetUSD,
			})
			if bErr != nil {
				yield(nil, bErr)
				return
			}
			reqCtx = WithInvocationBudget(ctx, budget)
		}

		reqPredicate := PredicateForRequest(req)
		var candidateErrors []candidateAttemptError

		for _, candidate := range f.route.Candidates {
			if err := budget.CheckActive(reqCtx); err != nil {
				yield(nil, err)
				return
			}
			if err := budget.CanAttempt(reqCtx); err != nil {
				yield(nil, err)
				return
			}

			def, ok := f.definitions[candidate]
			if !ok {
				if f.logger != nil {
					f.logger.WarnContext(reqCtx, "model candidate not found in definitions",
						"route", f.name,
						"candidate", candidate,
						"decision", "unknown_definition",
					)
				}
				candidateErrors = append(candidateErrors, candidateAttemptError{
					Candidate: candidate,
					Class:     ErrorClassInvalidRequest,
					Err:       newError(ErrorCodeNotFound, candidate, "", fmt.Sprintf("unknown model definition %q", candidate)),
				})
				continue
			}

			if err := ValidateCandidate(candidate, &def, &reqPredicate); err != nil {
				if f.logger != nil {
					f.logger.WarnContext(reqCtx, "model candidate capability mismatch",
						"route", f.name,
						"candidate", candidate,
						"decision", "capability_unsupported",
					)
				}
				candidateErrors = append(candidateErrors, candidateAttemptError{
					Candidate: candidate,
					Class:     ErrorClassUnsupported,
					Err:       err,
				})
				continue
			}

			circuitKey := FormatCircuitKey(candidate, def.BaseURL)
			if f.circuits != nil {
				allowed, _ := f.circuits.Allow(circuitKey)
				if !allowed {
					if f.logger != nil {
						f.logger.WarnContext(reqCtx, "model candidate circuit open",
							"route", f.name,
							"candidate", candidate,
							"decision", "circuit_open",
						)
					}
					candidateErrors = append(candidateErrors, candidateAttemptError{
						Candidate: candidate,
						Class:     ErrorClassOverloaded,
						Err:       newError(ErrorCodeOverloaded, candidate, "", fmt.Sprintf("circuit open for candidate %q", candidate)),
					})
					continue
				}
			}

			adapter, err := f.resolver.GetAdapter(candidate)
			if err != nil {
				if f.logger != nil {
					f.logger.WarnContext(reqCtx, "model adapter resolution failed",
						"route", f.name,
						"candidate", candidate,
						"decision", "adapter_unresolved",
					)
				}
				candidateErrors = append(candidateErrors, candidateAttemptError{
					Candidate: candidate,
					Class:     ErrorClassInvalidRequest,
					Err:       err,
				})
				continue
			}

			clonedReq := CloneCanonicalRequest(req)
			if clonedReq != nil && def.Model != "" {
				clonedReq.Model = def.Model
			}

			if !stream {
				var response *adkmodel.LLMResponse
				var attemptErr error
				for resp, err := range adapter.GenerateContent(reqCtx, clonedReq, false) {
					if err != nil {
						attemptErr = err
						break
					}
					response = resp
				}

				if attemptErr != nil {
					class := ClassifyError(attemptErr)
					if f.circuits != nil {
						f.circuits.RecordFailure(reqCtx, circuitKey, class)
					}
					if f.logger != nil {
						f.logger.WarnContext(reqCtx, "model candidate attempt failed",
							"route", f.name,
							"candidate", candidate,
							"error_class", string(class),
						)
					}
					candidateErrors = append(candidateErrors, candidateAttemptError{
						Candidate: candidate,
						Class:     class,
						Err:       attemptErr,
					})
					if class.FallbackEligible() {
						continue
					}
					// Terminal error (policy rejection, auth, unsupported, caller deadline):
					yield(nil, attemptErr)
					return
				}

				if f.circuits != nil {
					f.circuits.RecordSuccess(reqCtx, circuitKey)
				}
				yield(response, nil)
				return
			}

			// Streaming mode with observable boundary tracking
			boundaryCrossed := false
			var streamErr error
			aborted := false

			for resp, err := range adapter.GenerateContent(reqCtx, clonedReq, true) {
				if err != nil {
					streamErr = err
					break
				}
				boundaryCrossed = true
				if !yield(resp, nil) {
					aborted = true
					break
				}
			}

			if aborted {
				return
			}

			if streamErr != nil {
				if boundaryCrossed {
					// Emitted output already visible to caller: boundary crossed!
					// Must return typed error without restarting on another candidate.
					if f.logger != nil {
						f.logger.ErrorContext(reqCtx, "model stream interrupted after observable output",
							"route", f.name,
							"candidate", candidate,
							"decision", "fallback_boundary",
						)
					}
					boundaryErr := newError(
						ErrorCodeFallbackBoundary,
						candidate,
						"",
						fmt.Sprintf("model stream interrupted after observable output emitted: %v", streamErr),
					)
					yield(nil, boundaryErr)
					return
				}

				// Failed before emitting any output: boundary not crossed
				class := ClassifyError(streamErr)
				if f.circuits != nil {
					f.circuits.RecordFailure(reqCtx, circuitKey, class)
				}
				if f.logger != nil {
					f.logger.WarnContext(reqCtx, "model candidate stream attempt failed",
						"route", f.name,
						"candidate", candidate,
						"error_class", string(class),
					)
				}
				candidateErrors = append(candidateErrors, candidateAttemptError{
					Candidate: candidate,
					Class:     class,
					Err:       streamErr,
				})
				if class.FallbackEligible() {
					continue
				}
				yield(nil, streamErr)
				return
			}

			if f.circuits != nil {
				f.circuits.RecordSuccess(reqCtx, circuitKey)
			}
			return
		}

		// All candidates exhausted: return safe aliases and normalized classes only
		var parts []string
		for _, ce := range candidateErrors {
			parts = append(parts, fmt.Sprintf("%s (%s)", ce.Candidate, ce.Class))
		}
		summary := strings.Join(parts, ", ")
		if summary == "" {
			summary = "no candidates available"
		}
		if f.logger != nil {
			f.logger.ErrorContext(reqCtx, "model candidate chain exhausted",
				"route", f.name,
				"decision", "exhausted",
				"summary", summary,
			)
		}
		exhaustedErr := newError(
			ErrorCodeFallbackExhausted,
			f.name,
			"",
			"all candidate models exhausted: "+summary,
		)
		yield(nil, exhaustedErr)
	}
}
