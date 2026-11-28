package toolbroker

import (
	"encoding/json"
	"net/url"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/anggasct/aura/internal/platform/tools"
)

const (
	maxReliefURLBytes  = 250
	trackedWebOnlyRuns = 256
)

type WebOnlyLimits struct {
	Fetches int
	Queries int
}

type webOnlyRuns struct {
	mu    sync.Mutex
	order []string
	runs  map[string]*webOnlyCount
}

type webOnlyCount struct {
	fetches  int
	queries  int
	admitted map[string]struct{}
}

func (r *webOnlyRuns) admit(turnID, callKey string, kind tools.SinkReliefKind, limit int) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.runs == nil {
		r.runs = make(map[string]*webOnlyCount, trackedWebOnlyRuns)
	}
	count, found := r.runs[turnID]
	if !found {
		count = &webOnlyCount{admitted: map[string]struct{}{}}
		r.runs[turnID] = count
		r.order = append(r.order, turnID)
		if len(r.order) > trackedWebOnlyRuns {
			delete(r.runs, r.order[0])
			r.order = r.order[1:]
		}
	}
	if _, again := count.admitted[callKey]; again {
		return true
	}
	used := &count.fetches
	if kind == tools.SinkReliefBoundedQuery {
		used = &count.queries
	}
	if *used >= limit {
		return false
	}
	*used++
	count.admitted[callKey] = struct{}{}
	return true
}

func (b *Broker) webOnlyRelieved(request *ToolRequest, definition *tools.Definition) bool {
	relief := definition.SinkRelief
	if !request.WebOnlyRelief || !request.Tainted || relief == nil || !definition.IsExternalSink() {
		return false
	}
	value, ok := reliefArgument(request.Arguments, relief.Argument)
	if !ok || containsSecret(value, b.secretValues()) {
		return false
	}
	limit := b.webOnly.Fetches
	switch relief.Kind {
	case tools.SinkReliefLinkProvenance:
		if !plainURL(value) {
			return false
		}
	case tools.SinkReliefBoundedQuery:
		if len(value) > relief.MaxBytes {
			return false
		}
		limit = b.webOnly.Queries
	default:
		return false
	}
	return b.webOnlyUse.admit(request.TurnID, request.IdempotencyKey, relief.Kind, limit)
}

func reliefArgument(arguments json.RawMessage, name string) (string, bool) {
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(arguments, &decoded); err != nil {
		return "", false
	}
	var value string
	if err := json.Unmarshal(decoded[name], &value); err != nil || !utf8.ValidString(value) {
		return "", false
	}
	return value, true
}

func plainURL(raw string) bool {
	if len(raw) > maxReliefURLBytes {
		return false
	}
	parsed, err := url.Parse(raw)
	return err == nil && parsed.User == nil && parsed.Host != "" && !strings.Contains(parsed.Host, "@")
}

func containsSecret(value string, secrets []string) bool {
	for _, secret := range secrets {
		if secret != "" && strings.Contains(value, secret) {
			return true
		}
	}
	return false
}
