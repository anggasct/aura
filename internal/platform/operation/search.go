package operation

import (
	"cmp"
	"context"
	"encoding/json"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/telemetry"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/tools"
	"github.com/anggasct/aura/internal/platform/turn"
)

const (
	MaxLoaded          = 32
	MaxLoadedPerSearch = 10

	defaultSearchLimit = 5
	maxSearchLimit     = 10
	maxQueryRunes      = 200
	maxGroupRunes      = 32
	maxGroupMatches    = 50

	weightIdentity    = 3
	weightSummary     = 2
	weightDescription = 1
	weightParameter   = 1

	searchDescriptionChat     = "Find Aura operations by words or by group and load the ones you can call. Results also give the chat and CLI command of operations only the owner or the host can run."
	searchDescriptionTerminal = "Find Aura operations by words or by group and load the ones you can call. Results also give the aura command of operations only the owner or the host can run."
	searchSchema              = `{"$schema":"https://json-schema.org/draft/2020-12/schema","$id":"aura://tools/aura_operation_search/v1","type":"object","properties":{"query":{"type":"string","minLength":1,"maxLength":200},"group":{"type":"string","minLength":1,"maxLength":32},"limit":{"type":"integer","minimum":1,"maximum":10}},"additionalProperties":false}`
)

type SearchScope struct {
	Kind                TurnKind
	ContentTaintTracked bool
	Offered             []string
	Loaded              []string
}

type searchScopeKey struct{}

func WithSearchScope(ctx context.Context, scope *SearchScope) context.Context {
	if scope == nil {
		return ctx
	}
	copied := *scope
	copied.Offered = slices.Clone(scope.Offered)
	copied.Loaded = slices.Clone(scope.Loaded)
	return context.WithValue(ctx, searchScopeKey{}, copied)
}

func searchScopeFrom(ctx context.Context) (SearchScope, bool) {
	scope, ok := ctx.Value(searchScopeKey{}).(SearchScope)
	return scope, ok && scope.Kind.valid()
}

type searchArguments struct {
	query string
	group string
	limit int
}

type searchEntry struct {
	ID       string   `json:"id"`
	Summary  string   `json:"summary"`
	Exposure Exposure `json:"exposure"`
	Risk     Risk     `json:"risk"`
	Approval Approval `json:"approval"`
	Tool     string   `json:"tool,omitempty"`
	Offered  *bool    `json:"offered,omitempty"`
	Chat     string   `json:"chat,omitempty"`
	Command  string   `json:"command"`
}

type searchResult struct {
	Operations  []searchEntry `json:"operations"`
	Loaded      []string      `json:"loaded"`
	SessionFull bool          `json:"session_full,omitempty"`
}

func (d *Discovery) Tools() []toolbroker.ToolRegistration {
	description := searchDescriptionTerminal
	if d.chatCommands {
		description = searchDescriptionChat
	}
	definition := tools.Definition{
		Name:        tools.OperationSearchToolName,
		Version:     tools.Version,
		Schema:      json.RawMessage(searchSchema),
		OptIn:       true,
		OwnerOptIn:  true,
		Description: description,
		Validator:   validateSearchArguments,
	}
	return []toolbroker.ToolRegistration{{
		Definition: definition,
		Adapter:    d.search,
		Rule:       toolbroker.DefaultRule(&definition),
	}}
}

func validateSearchArguments(raw json.RawMessage) (json.RawMessage, error) {
	value, err := tools.DecodeObject(raw)
	if err != nil {
		return nil, err
	}
	if err := tools.RejectUnknown(value, "query", "group", "limit"); err != nil {
		return nil, err
	}
	_, hasQuery := value["query"]
	_, hasGroup := value["group"]
	if !hasQuery && !hasGroup {
		return nil, NewError(ErrorCodeInputInvalid, "operation: a search needs a query or a group")
	}
	for key, bound := range map[string]int{"query": maxQueryRunes, "group": maxGroupRunes} {
		if err := validateBoundedString(value, key, bound); err != nil {
			return nil, err
		}
	}
	if err := tools.ValidateOptionalInteger(value, "limit", 1); err != nil {
		return nil, err
	}
	if limit, ok := value["limit"].(json.Number); ok {
		if parsed, err := limit.Int64(); err != nil || parsed > maxSearchLimit {
			return nil, NewError(ErrorCodeInputInvalid, "operation: limit is over its maximum")
		}
	}
	return json.Marshal(value)
}

func validateBoundedString(value map[string]any, key string, maxRunes int) error {
	raw, present := value[key]
	if !present {
		return nil
	}
	text, ok := raw.(string)
	if !ok || text == "" || utf8.RuneCountInString(text) > maxRunes {
		return NewError(ErrorCodeInputInvalid, "operation: "+key+" must be a non-empty string within its bound")
	}
	return nil
}

func decodeSearch(raw json.RawMessage) (searchArguments, error) {
	value, err := tools.DecodeObject(raw)
	if err != nil {
		return searchArguments{}, err
	}
	arguments := searchArguments{limit: defaultSearchLimit}
	arguments.query, _ = value["query"].(string)
	arguments.group, _ = value["group"].(string)
	if limit, ok := value["limit"].(json.Number); ok {
		parsed, err := limit.Int64()
		if err != nil {
			return searchArguments{}, err
		}
		arguments.limit = int(parsed)
	}
	return arguments, nil
}

func (d *Discovery) search(ctx context.Context, request *toolbroker.ToolRequest, _ approval.Constraints) (toolbroker.ToolResult, error) {
	if request == nil {
		return toolbroker.ToolResult{}, nilArgument("tool request")
	}
	scope, scoped := searchScopeFrom(ctx)
	ownerTurn := request.PrincipalClass == principal.ClassOwner && request.Origin != "" && request.Origin != turn.OriginChild && request.Origin != turn.OriginWorkflow
	if !scoped || !ownerTurn {
		return toolbroker.ToolResult{}, toolbroker.Errorf(toolbroker.ResultToolTierDenied, "tool %q is available only on a turn the owner started", tools.OperationSearchToolName)
	}
	arguments, err := decodeSearch(request.Arguments)
	if err != nil {
		return toolbroker.ToolResult{}, toolbroker.Errorf(toolbroker.ResultInvalidArgument, "the search arguments are invalid")
	}
	view := &Turn{Origin: request.Origin, Kind: scope.Kind, Tainted: request.Tainted, ContentTaintTracked: scope.ContentTaintTracked}
	matches, err := d.matches(&arguments)
	if err != nil {
		return toolbroker.ToolResult{}, err
	}
	result, loadedTotal := d.result(matches, view, &scope)
	d.recorder.RecordSearch(ctx, searchOutcome(&result), loadedTotal)
	output, err := json.Marshal(result)
	if err != nil {
		return toolbroker.ToolResult{}, toolbroker.Errorf(toolbroker.ResultExecutionFailed, "the search result cannot be encoded")
	}
	return toolbroker.ToolResult{Class: toolbroker.ResultOK, Output: output}, nil
}

type match struct {
	found *entry
	score int
}

func (d *Discovery) matches(arguments *searchArguments) ([]match, error) {
	tokens := queryTokens(arguments.query)
	var matches []match
	if arguments.group != "" {
		ids, known := d.registry.members[arguments.group]
		if !known {
			return nil, toolbroker.Errorf(toolbroker.ResultInvalidArgument, "the group is not known")
		}
		for _, id := range ids {
			found := d.registry.entries[id]
			if d.available(found.descriptor) {
				matches = append(matches, match{found: found, score: scoreOf(found.descriptor, tokens)})
			}
		}
		sortMatches(matches)
		return matches[:min(len(matches), maxGroupMatches)], nil
	}
	for _, id := range d.registry.ids {
		found := d.registry.entries[id]
		if !d.available(found.descriptor) {
			continue
		}
		if score := scoreOf(found.descriptor, tokens); score > 0 {
			matches = append(matches, match{found: found, score: score})
		}
	}
	sortMatches(matches)
	return matches[:min(len(matches), arguments.limit)], nil
}

func sortMatches(matches []match) {
	slices.SortFunc(matches, func(a, b match) int {
		return cmp.Or(cmp.Compare(b.score, a.score), compareStrings(a.found.descriptor.ID, b.found.descriptor.ID))
	})
}

func queryTokens(query string) []string {
	tokens := strings.FieldsFunc(strings.ToLower(query), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	slices.Sort(tokens)
	return slices.Compact(tokens)
}

func scoreOf(descriptor *Descriptor, tokens []string) int {
	group, _, _ := strings.Cut(descriptor.ID, ".")
	identity := []string{strings.ToLower(descriptor.ID), strings.ToLower(group), strings.ToLower(verbOf(group, descriptor.ID))}
	summary := strings.ToLower(descriptor.Summary)
	description := strings.ToLower(descriptor.Description)
	score := 0
	for _, token := range tokens {
		for _, field := range identity {
			if strings.Contains(field, token) {
				score += weightIdentity
			}
		}
		if strings.Contains(summary, token) {
			score += weightSummary
		}
		if strings.Contains(description, token) {
			score += weightDescription
		}
		if slices.ContainsFunc(descriptor.Params, func(p Param) bool { return strings.Contains(strings.ToLower(p.Name), token) }) {
			score += weightParameter
		}
	}
	return score
}

func searchOutcome(result *searchResult) string {
	switch {
	case result.SessionFull:
		return telemetry.SearchOutcomeSessionFull
	case len(result.Operations) == 0:
		return telemetry.SearchOutcomeEmpty
	}
	return telemetry.SearchOutcomeMatched
}

func (d *Discovery) result(matches []match, view *Turn, scope *SearchScope) (result searchResult, loadedTotal int) {
	loaded := slices.Clone(scope.Loaded)
	result = searchResult{Operations: make([]searchEntry, 0, len(matches)), Loaded: []string{}}
	full := len(loaded) >= MaxLoaded
	for i := range matches {
		found := matches[i].found
		descriptor := found.descriptor
		entry := searchEntry{
			ID:       descriptor.ID,
			Summary:  descriptor.Summary,
			Exposure: descriptor.Exposure,
			Risk:     descriptor.Risk,
			Approval: found.approval(view),
			Command:  synopsis(descriptor, SurfaceCLI),
		}
		if d.chatCommands && descriptor.Projects(SurfaceChat) {
			entry.Chat = synopsis(descriptor, SurfaceChat)
		}
		switch {
		case descriptor.ModuleTool != "":
			entry.Tool = descriptor.ModuleTool
			offered := slices.Contains(scope.Offered, descriptor.ModuleTool)
			entry.Offered = &offered
		case descriptor.GeneratesTool():
			entry.Tool = ToolName(descriptor.ID)
			switch {
			case slices.Contains(loaded, entry.Tool):
				result.Loaded = append(result.Loaded, entry.Tool)
			case len(loaded) < MaxLoaded:
				loaded = append(loaded, entry.Tool)
				result.Loaded = append(result.Loaded, entry.Tool)
			default:
				full = true
			}
		}
		result.Operations = append(result.Operations, entry)
	}
	result.SessionFull = full
	return result, len(loaded)
}

func synopsis(descriptor *Descriptor, surface Surface) string {
	path := commandPath(descriptor.ID)
	parts := append([]string{rootGroup}, path...)
	if surface == SurfaceChat {
		parts = append([]string{"/" + path[0]}, path[1:]...)
	}
	positionals := descriptor.Positionals(surface)
	for i := range positionals {
		positional := &positionals[i]
		label := "<" + positional.Name + ">"
		if !positional.Required {
			label = "[" + label + "]"
		}
		parts = append(parts, label)
	}
	flags := descriptor.Flags(surface)
	for i := range flags {
		flag := &flags[i]
		label := "--" + flag.Name
		if flag.Kind != KindBoolean {
			label += " <" + placeholder(flag) + ">"
		}
		if !flag.Required {
			label = "[" + label + "]"
		}
		parts = append(parts, label)
	}
	return strings.Join(parts, " ")
}

func placeholder(flag *Flag) string {
	if len(flag.Values) > 0 {
		return strings.Join(flag.Values, "|")
	}
	return flag.Name
}
