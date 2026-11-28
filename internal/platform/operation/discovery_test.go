package operation_test

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/operation"
	"github.com/anggasct/aura/internal/platform/operation/operationtest"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/tools"
	"github.com/anggasct/aura/internal/platform/turn"
)

var stubBehaviors = []operation.Behavior{
	{ID: "recall", Group: operationtest.Group, Summary: "past conversations are recalled into your context when they match"},
	{ID: "alerts", Summary: "important events reach the owner on their own"},
}

func discoveryRig(t *testing.T, behaviors []operation.Behavior) *operationtest.Rig {
	t.Helper()
	return operationtest.NewRig(t, &operationtest.Options{WithDiscovery: true, Behaviors: behaviors})
}

func describe(t *testing.T, rig *operationtest.Rig, kind operation.TurnKind, tracked, search bool, withheld ...string) string {
	t.Helper()
	text, err := rig.Discovery.Describe(&operation.DescriptionInput{
		Turn:     operation.Turn{Origin: turn.OriginTerminal, Kind: kind, ContentTaintTracked: tracked},
		Search:   search,
		Withheld: withheld,
	})
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	return text
}

func TestOperationDescriptionGolden(t *testing.T) {
	rig := discoveryRig(t, stubBehaviors)
	cases := []struct {
		name     string
		kind     operation.TurnKind
		tracked  bool
		search   bool
		withheld []string
	}{
		{name: "attended", kind: operation.TurnAttended, search: true},
		{name: "unattended", kind: operation.TurnUnattended, search: true},
		{name: "attended-tracked", kind: operation.TurnAttended, tracked: true, search: true},
		{name: "unattended-tracked", kind: operation.TurnUnattended, tracked: true, search: true},
		{name: "attended-listed-tools", kind: operation.TurnAttended},
		{name: "attended-withheld", kind: operation.TurnAttended, search: true, withheld: []string{"web_search", "email_send", "email_send"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			compareGolden(t, "description-"+tc.name+".txt", describe(t, rig, tc.kind, tc.tracked, tc.search, tc.withheld...)+"\n")
		})
	}
}

func TestOperationDescriptionIsDeterministic(t *testing.T) {
	rig := discoveryRig(t, stubBehaviors)
	first := describe(t, rig, operation.TurnAttended, false, true, "b", "a")
	for range 20 {
		if got := describe(t, rig, operation.TurnAttended, false, true, "a", "b"); got != first {
			t.Fatalf("the description changed between calls:\n%s\n---\n%s", first, got)
		}
	}
}

func TestOperationDescriptionNamesNoInternalIdentifiers(t *testing.T) {
	rig := discoveryRig(t, stubBehaviors)
	for _, kind := range []operation.TurnKind{operation.TurnAttended, operation.TurnUnattended} {
		text := describe(t, rig, kind, false, true)
		for _, banned := range []string{"AC-", "SR-", "Decision", "spec", "step "} {
			if strings.Contains(text, banned) {
				t.Errorf("the %s description contains %q", kind, banned)
			}
		}
	}
}

func TestOperationDescriptionRejectsAnUnknownTurnKind(t *testing.T) {
	rig := discoveryRig(t, nil)
	if _, err := rig.Discovery.Describe(&operation.DescriptionInput{Turn: operation.Turn{Origin: turn.OriginTerminal, Kind: "sometimes"}}); !fault.HasCode(err, operation.ErrorCodeInvalidArgument) {
		t.Fatalf("Describe = %v, want invalid_argument", err)
	}
	if _, err := rig.Discovery.Describe(nil); !fault.HasCode(err, operation.ErrorCodeInvalidArgument) {
		t.Fatalf("Describe(nil) = %v, want invalid_argument", err)
	}
}

func TestOperationDescriptionFitsBound(t *testing.T) {
	rig := discoveryRig(t, stubBehaviors)
	for _, kind := range []operation.TurnKind{operation.TurnAttended, operation.TurnUnattended} {
		if size := len(describe(t, rig, kind, false, true)); size > operation.MaxDescriptionBytes {
			t.Errorf("the %s description is %d bytes, over %d", kind, size, operation.MaxDescriptionBytes)
		}
	}
	t.Run("a registry too large for the bound fails at build", func(t *testing.T) {
		catalog := operationtest.Catalog()
		for i := range 400 {
			descriptor := catalog.Descriptors[9]
			descriptor.ID = fmt.Sprintf("%s.verb-number-%d", operationtest.Group, i)
			descriptor.Params = nil
			catalog.Descriptors = append(catalog.Descriptors, descriptor)
		}
		bindings := func(stubs *operationtest.Stubs) []operation.Binding {
			bound := stubs.Bindings()
			for i := range catalog.Descriptors[10:] {
				bound = append(bound, operation.Binding{ID: catalog.Descriptors[10+i].ID, Handle: bound[0].Handle})
			}
			return bound
		}
		rig := operationtest.NewRig(t, &operationtest.Options{Catalogs: []operation.Catalog{catalog}, Bindings: bindings})
		_, err := operation.NewDiscovery(&operation.DiscoveryDeps{Registry: rig.Registry})
		if !fault.HasCode(err, operation.ErrorCodeInvalid) {
			t.Fatalf("NewDiscovery = %v, want operation_invalid", err)
		}
	})
}

func TestDiscoveryRejectsBadBehaviors(t *testing.T) {
	rig := operationtest.NewRig(t, nil)
	cases := []struct {
		name      string
		behaviors []operation.Behavior
	}{
		{name: "an id outside the pattern", behaviors: []operation.Behavior{{ID: "Recall", Summary: "x"}}},
		{name: "an empty summary", behaviors: []operation.Behavior{{ID: "recall"}}},
		{name: "a summary with a line break", behaviors: []operation.Behavior{{ID: "recall", Summary: "a\nb"}}},
		{name: "a repeated id", behaviors: []operation.Behavior{{ID: "recall", Summary: "a"}, {ID: "recall", Summary: "b"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := operation.NewDiscovery(&operation.DiscoveryDeps{Registry: rig.Registry, Behaviors: tc.behaviors})
			if !fault.HasCode(err, operation.ErrorCodeInvalid) {
				t.Fatalf("NewDiscovery = %v, want operation_invalid", err)
			}
		})
	}
	if _, err := operation.NewDiscovery(nil); !fault.HasCode(err, operation.ErrorCodeInvalidArgument) {
		t.Fatalf("NewDiscovery(nil) = %v, want invalid_argument", err)
	}
	if _, err := operation.NewDiscovery(&operation.DiscoveryDeps{}); !fault.HasCode(err, operation.ErrorCodeInvalidArgument) {
		t.Fatalf("NewDiscovery without a registry = %v, want invalid_argument", err)
	}
}

func markersByVerb(t *testing.T, text, group string) map[string]string {
	t.Helper()
	for _, line := range strings.Split(text, "\n") {
		prefix := "- " + group + ": "
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		_, verbs, found := strings.Cut(strings.TrimPrefix(line, prefix), ": ")
		if !found {
			t.Fatalf("the group line %q has no verb list", line)
		}
		markers := map[string]string{}
		for _, entry := range strings.Split(verbs, ", ") {
			switch {
			case strings.HasSuffix(entry, "^"):
				markers[strings.TrimSuffix(entry, "^")] = "^"
			case strings.HasSuffix(entry, "?"):
				markers[strings.TrimSuffix(entry, "?")] = "?"
			case strings.HasSuffix(entry, " (owner)"), strings.HasSuffix(entry, " (host)"):
				verb, _, _ := strings.Cut(entry, " (")
				markers[verb] = entry[len(verb)+1:]
			default:
				markers[entry] = ""
			}
		}
		return markers
	}
	t.Fatalf("no line for the group %s in %q", group, text)
	return nil
}

func TestOperationDescriptionMarkersMatchBroker(t *testing.T) {
	for _, kind := range []operation.TurnKind{operation.TurnAttended, operation.TurnUnattended} {
		for _, tracked := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s tracked=%t", kind, tracked), func(t *testing.T) {
				rig := discoveryRig(t, nil)
				markers := markersByVerb(t, describe(t, rig, kind, tracked, true), operationtest.Group)
				registrations := rig.Registry.Tools()
				if len(registrations) == 0 {
					t.Fatal("the registry holds no tools")
				}
				for i := range registrations {
					definition := &registrations[i].Definition
					verb := strings.TrimPrefix(definition.Operation, operationtest.Group+".")
					marker, listed := markers[verb]
					if !listed {
						t.Fatalf("the description does not list %s", verb)
					}
					request := operationtest.Request(t, definition.Name, "subject")
					request.Unattended = kind == operation.TurnUnattended
					request.ContentTaintTracked = tracked
					before := len(rig.Prompts())

					got := rig.Execute(t.Context(), request)
					asked := len(rig.Prompts()) - before

					switch {
					case marker == "" && (got.Class != toolbroker.ResultOK || asked != 0):
						t.Errorf("%s is unmarked and the broker answered %+v after %d cards", verb, got, asked)
					case marker != "" && kind == operation.TurnAttended && (got.Class != toolbroker.ResultOK || asked != 1):
						t.Errorf("%s is marked %s and the broker answered %+v after %d cards, want one card", verb, marker, got, asked)
					case marker != "" && kind == operation.TurnUnattended && (got.Class != toolbroker.ResultOwnerTurnRequired || asked != 0):
						t.Errorf("%s is marked %s on an unattended turn and the broker answered %+v after %d cards, want a refusal", verb, marker, got, asked)
					}
				}
				for verb, marker := range markers {
					if marker == "^" && tracked && kind == operation.TurnAttended && !slices.Contains([]string{"fresh", "grant", "delete"}, verb) {
						t.Errorf("%s is marked ^ without being privileged", verb)
					}
					if marker == "?" && tracked {
						t.Errorf("%s is marked ? although content taint is tracked", verb)
					}
				}
			})
		}
	}
}

func TestOperationDescriptionMarkersFollowTheApprovalValue(t *testing.T) {
	rig := discoveryRig(t, nil)
	scope := &operation.SearchScope{Kind: operation.TurnAttended}
	markers := markersByVerb(t, describe(t, rig, operation.TurnAttended, false, true), operationtest.Group)
	for verb, marker := range markers {
		id := operationtest.Group + "." + verb
		entries := searchGroup(t, rig, scope, false)
		var found *searchedOperation
		for i := range entries.Operations {
			if entries.Operations[i].ID == id {
				found = &entries.Operations[i]
			}
		}
		if found == nil {
			t.Fatalf("the group search lacks %s", id)
		}
		want := map[string]string{"^": "always", "?": "asks", "": "none"}[marker]
		if marker == "(owner)" || marker == "(host)" {
			want = strings.Trim(marker, "()")
		}
		if found.Approval != want {
			t.Errorf("%s: marker %q but search approval %q, want %q", id, marker, found.Approval, want)
		}
	}
}

type searchedOperation struct {
	ID       string `json:"id"`
	Summary  string `json:"summary"`
	Exposure string `json:"exposure"`
	Risk     string `json:"risk"`
	Approval string `json:"approval"`
	Tool     string `json:"tool"`
	Offered  *bool  `json:"offered"`
	Command  string `json:"command"`
}

type searchResponse struct {
	Operations  []searchedOperation `json:"operations"`
	Loaded      []string            `json:"loaded"`
	SessionFull bool                `json:"session_full"`
}

func searchWith(t *testing.T, rig *operationtest.Rig, scope *operation.SearchScope, arguments string, mutate func(*toolbroker.ToolRequest)) (operationtest.Outcome, searchResponse) {
	t.Helper()
	request := operationtest.RequestFor(tools.OperationSearchToolName, json.RawMessage(arguments), "search-"+arguments)
	if mutate != nil {
		mutate(request)
	}
	ctx := t.Context()
	if scope != nil {
		ctx = operation.WithSearchScope(ctx, scope)
	}
	outcome := rig.Execute(ctx, request)
	var response searchResponse
	if outcome.Class == toolbroker.ResultOK {
		if err := json.Unmarshal([]byte(outcome.Output), &response); err != nil {
			t.Fatalf("decode the search result %q: %v", outcome.Output, err)
		}
	}
	return outcome, response
}

func searchGroup(t *testing.T, rig *operationtest.Rig, scope *operation.SearchScope, tainted bool) searchResponse {
	t.Helper()
	outcome, response := searchWith(t, rig, scope, `{"group":"`+operationtest.Group+`"}`, func(r *toolbroker.ToolRequest) { r.Tainted = tainted })
	if outcome.Class != toolbroker.ResultOK {
		t.Fatalf("search = %+v, want ok", outcome)
	}
	return response
}

func approvals(response *searchResponse) map[string]string {
	byVerb := map[string]string{}
	for i := range response.Operations {
		byVerb[strings.TrimPrefix(response.Operations[i].ID, operationtest.Group+".")] = response.Operations[i].Approval
	}
	return byVerb
}

func TestOperationSearch(t *testing.T) {
	rig := discoveryRig(t, nil)
	attended := &operation.SearchScope{Kind: operation.TurnAttended}

	t.Run("a group loads every callable operation and reports the rest without loading them", func(t *testing.T) {
		response := searchGroup(t, rig, attended, false)
		want := []string{"aura_stub_change", "aura_stub_delete", "aura_stub_external", "aura_stub_fresh", "aura_stub_grant", "aura_stub_idempotent", "aura_stub_notice", "aura_stub_read"}
		if !slices.Equal(response.Loaded, want) {
			t.Fatalf("loaded = %v, want %v", response.Loaded, want)
		}
		got := approvals(&response)
		if got["host"] != "host" || got["owner-only"] != "owner" {
			t.Fatalf("approvals = %v, want the host and owner-only verbs reported as such", got)
		}
		if slices.Contains(response.Loaded, "aura_stub_host") || slices.Contains(response.Loaded, "aura_stub_owner_only") {
			t.Fatalf("loaded = %v, an owner or host operation was loaded", response.Loaded)
		}
		if len(response.Operations) != 10 {
			t.Fatalf("operations = %d, want every operation of the group", len(response.Operations))
		}
	})
	t.Run("approval is what the offered tool does on the turn", func(t *testing.T) {
		cases := []struct {
			name    string
			scope   operation.SearchScope
			tainted bool
			want    map[string]string
		}{
			{name: "attended", scope: operation.SearchScope{Kind: operation.TurnAttended}, want: map[string]string{"read": "none", "change": "asks", "notice": "none", "fresh": "always", "grant": "always", "delete": "always", "external": "asks", "idempotent": "asks"}},
			{name: "unattended", scope: operation.SearchScope{Kind: operation.TurnUnattended}, want: map[string]string{"read": "none", "change": "refused_here", "notice": "none", "fresh": "refused_here", "grant": "refused_here", "delete": "refused_here", "external": "refused_here", "idempotent": "refused_here"}},
			{name: "attended with content taint tracked", scope: operation.SearchScope{Kind: operation.TurnAttended, ContentTaintTracked: true}, want: map[string]string{"read": "none", "change": "untrusted_input", "notice": "none", "fresh": "always", "grant": "always", "delete": "always", "external": "untrusted_input", "idempotent": "untrusted_input"}},
			{name: "attended and tainted", scope: operation.SearchScope{Kind: operation.TurnAttended}, tainted: true, want: map[string]string{"read": "none", "change": "asks", "notice": "none", "fresh": "always", "grant": "refused_here", "delete": "always"}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				got := approvals(&[]searchResponse{searchGroup(t, rig, &tc.scope, tc.tainted)}[0])
				for verb, want := range tc.want {
					if got[verb] != want {
						t.Errorf("approval of %s = %q, want %q", verb, got[verb], want)
					}
				}
			})
		}
	})
	t.Run("ranking is deterministic, weighted and ties break by id", func(t *testing.T) {
		_, first := searchWith(t, rig, attended, `{"query":"stub","limit":3}`, nil)
		if len(first.Operations) != 3 || first.Operations[0].ID != "stub.change" || first.Operations[1].ID != "stub.delete" || first.Operations[2].ID != "stub.external" {
			t.Fatalf("operations = %+v, want the first three ids of equal score", first.Operations)
		}
		_, narrow := searchWith(t, rig, attended, `{"query":"read"}`, nil)
		if len(narrow.Operations) != 1 || narrow.Operations[0].ID != "stub.read" {
			t.Fatalf("operations = %+v, want only stub.read", narrow.Operations)
		}
		for range 5 {
			again, _ := searchWith(t, rig, attended, `{"query":"stub","limit":3}`, nil)
			base, _ := searchWith(t, rig, attended, `{"query":"stub","limit":3}`, nil)
			if again.Output != base.Output {
				t.Fatalf("two searches differ:\n%s\n%s", again.Output, base.Output)
			}
		}
	})
	t.Run("a search with no match answers with empty lists", func(t *testing.T) {
		outcome, response := searchWith(t, rig, attended, `{"query":"zzzz"}`, nil)
		if outcome.Class != toolbroker.ResultOK || !strings.Contains(outcome.Output, `"operations":[]`) || !strings.Contains(outcome.Output, `"loaded":[]`) {
			t.Fatalf("search = %+v, want empty operations and loaded lists", outcome)
		}
		if response.SessionFull {
			t.Fatal("session_full is set on an empty result")
		}
	})
	t.Run("a result names the command and the tool", func(t *testing.T) {
		response := searchGroup(t, rig, attended, false)
		for i := range response.Operations {
			entry := &response.Operations[i]
			if !strings.HasPrefix(entry.Command, "aura stub ") || entry.Summary == "" {
				t.Errorf("entry %+v has no command or summary", entry)
			}
		}
	})
	t.Run("bad arguments and callers are refused", func(t *testing.T) {
		cases := []struct {
			name      string
			scope     *operation.SearchScope
			arguments string
			mutate    func(*toolbroker.ToolRequest)
			want      toolbroker.ResultClass
		}{
			{name: "neither a query nor a group", scope: attended, arguments: `{"limit":2}`, want: toolbroker.ResultInvalidArgument},
			{name: "an unknown group", scope: attended, arguments: `{"group":"nowhere"}`, want: toolbroker.ResultInvalidArgument},
			{name: "a limit over the maximum", scope: attended, arguments: `{"query":"stub","limit":11}`, want: toolbroker.ResultInvalidArgument},
			{name: "an unknown key", scope: attended, arguments: `{"query":"stub","extra":1}`, want: toolbroker.ResultInvalidArgument},
			{name: "a query over its length", scope: attended, arguments: `{"query":"` + strings.Repeat("a", 201) + `"}`, want: toolbroker.ResultInvalidArgument},
			{name: "no turn scope", arguments: `{"query":"stub"}`, want: toolbroker.ResultToolTierDenied},
			{name: "a trusted principal", scope: attended, arguments: `{"query":"stub"}`, mutate: func(r *toolbroker.ToolRequest) { r.PrincipalClass = principal.ClassTrusted }, want: toolbroker.ResultToolTierDenied},
			{name: "a child origin", scope: attended, arguments: `{"query":"stub"}`, mutate: func(r *toolbroker.ToolRequest) { r.Origin = turn.OriginChild }, want: toolbroker.ResultToolTierDenied},
			{name: "a workflow origin", scope: attended, arguments: `{"query":"stub"}`, mutate: func(r *toolbroker.ToolRequest) { r.Origin = turn.OriginWorkflow }, want: toolbroker.ResultToolTierDenied},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				got, _ := searchWith(t, rig, tc.scope, tc.arguments, tc.mutate)
				if got.Class != tc.want {
					t.Fatalf("search = %+v, want %s", got, tc.want)
				}
			})
		}
	})
	t.Run("the session fills at 32 loaded tools", func(t *testing.T) {
		already := make([]string, 0, operation.MaxLoaded)
		for i := range operation.MaxLoaded {
			already = append(already, fmt.Sprintf("aura_other_tool_%d", i))
		}
		full := &operation.SearchScope{Kind: operation.TurnAttended, Loaded: already}
		response := searchGroup(t, rig, full, false)
		if !response.SessionFull || len(response.Loaded) != 0 {
			t.Fatalf("response = %+v, want session_full and nothing newly loaded", response)
		}
		partial := &operation.SearchScope{Kind: operation.TurnAttended, Loaded: already[:operation.MaxLoaded-2]}
		response = searchGroup(t, rig, partial, false)
		if !response.SessionFull || len(response.Loaded) != 2 {
			t.Fatalf("response = %+v, want two loaded and session_full", response)
		}
	})
}

func TestOperationSearchReportsModuleToolsWithoutLoadingThem(t *testing.T) {
	cases := []struct {
		name    string
		sink    bool
		offered []string
		want    string
	}{
		{name: "a projection the persona offers", offered: []string{"legacy_start"}, want: "none"},
		{name: "a sink projection the persona does not offer", sink: true, want: "untrusted_input"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			shipped := shippedTool(shippedSchema, false)
			rig := operationtest.NewRig(t, &operationtest.Options{
				Tools:         operationtest.LegacyTools(&shipped),
				Catalogs:      []operation.Catalog{{Group: operation.Group{Name: "wf", Summary: "workflows"}, Descriptors: []operation.Descriptor{withSink(projectingDescriptor(), tc.sink)}}},
				WithDiscovery: true,
				Bindings: func(*operationtest.Stubs) []operation.Binding {
					return []operation.Binding{{ID: "wf.start", Handle: func(context.Context, *operation.Call) (operation.Result, error) { return operation.Result{}, nil }}}
				},
			})
			scope := &operation.SearchScope{Kind: operation.TurnAttended, Offered: tc.offered}
			outcome, response := searchWith(t, rig, scope, `{"group":"wf"}`, nil)
			if outcome.Class != toolbroker.ResultOK || len(response.Operations) != 1 {
				t.Fatalf("search = %+v, want one operation", outcome)
			}
			entry := response.Operations[0]
			if entry.Tool != "legacy_start" || entry.Offered == nil || *entry.Offered != (len(tc.offered) > 0) || entry.Approval != tc.want {
				t.Fatalf("entry = %+v, want tool legacy_start, offered %t and approval %s", entry, len(tc.offered) > 0, tc.want)
			}
			if len(response.Loaded) != 0 {
				t.Fatalf("loaded = %v, a module tool is never loaded", response.Loaded)
			}
		})
	}
}

func withSink(descriptor operation.Descriptor, sink bool) operation.Descriptor {
	descriptor.Sink = sink
	return descriptor
}

func TestRecordedLoadsReadsOnlyWellFormedResults(t *testing.T) {
	long := make([]any, operation.MaxLoadedPerSearch+1)
	for i := range long {
		long[i] = fmt.Sprintf("aura_tool_%d", i)
	}
	cases := []struct {
		name     string
		response map[string]any
		want     []string
	}{
		{name: "a plain list", response: map[string]any{"loaded": []any{"aura_cron_list", "aura_cron_add"}}, want: []string{"aura_cron_list", "aura_cron_add"}},
		{name: "a repeated name", response: map[string]any{"loaded": []any{"aura_cron_list", "aura_cron_list"}}, want: []string{"aura_cron_list"}},
		{name: "no loaded key", response: map[string]any{"operations": []any{}}},
		{name: "loaded of the wrong type", response: map[string]any{"loaded": "aura_cron_list"}},
		{name: "a name that is not a generated tool shape", response: map[string]any{"loaded": []any{"aura_cron_list", "read_file"}}},
		{name: "a non-string entry", response: map[string]any{"loaded": []any{"aura_cron_list", 7}}},
		{name: "the discovery tool itself", response: map[string]any{"loaded": []any{tools.OperationSearchToolName}}},
		{name: "more names than one search can load", response: map[string]any{"loaded": long}},
		{name: "a name over the tool name bound", response: map[string]any{"loaded": []any{"aura_" + strings.Repeat("a", 64)}}},
		{name: "a nil response"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := operation.RecordedLoads(tc.response); !slices.Equal(got, tc.want) {
				t.Fatalf("RecordedLoads = %v, want %v", got, tc.want)
			}
		})
	}
}
