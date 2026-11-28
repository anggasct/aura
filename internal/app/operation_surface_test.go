package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"testing"

	"google.golang.org/genai"

	appconfig "github.com/anggasct/aura/internal/app/config"
	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/capability/capabilitytest"
	auramodel "github.com/anggasct/aura/internal/platform/model"
	"github.com/anggasct/aura/internal/platform/operation"
	"github.com/anggasct/aura/internal/platform/operation/operationtest"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/tools"
	"github.com/anggasct/aura/internal/platform/turn"
	runtimeadk "github.com/anggasct/aura/internal/platform/turn/adk"
	runtimeingress "github.com/anggasct/aura/internal/platform/turn/ingress"
	turnstore "github.com/anggasct/aura/internal/platform/turn/store"
)

func describeRealRegistry(t *testing.T, rig *registryRig, kind operation.TurnKind, tracked bool) string {
	t.Helper()
	text, err := rig.Discovery.Describe(&operation.DescriptionInput{
		Turn:   operation.Turn{Origin: turn.OriginTerminal, Kind: kind, ContentTaintTracked: tracked},
		Search: true,
	})
	if err != nil {
		t.Fatalf("Describe(%s, tracked=%t): %v", kind, tracked, err)
	}
	return text
}

func TestOperationDescriptionOfTheRealRegistryGolden(t *testing.T) {
	rig := newRegistryRig(t, nil)
	compareAppGolden(t, "operation-description-attended.txt", describeRealRegistry(t, rig, operation.TurnAttended, false)+"\n")
	compareAppGolden(t, "operation-description-unattended.txt", describeRealRegistry(t, rig, operation.TurnUnattended, false)+"\n")
}

func TestOperationDescriptionOfTheRealRegistryFitsTheBound(t *testing.T) {
	rig := newRegistryRig(t, nil)
	for _, kind := range []operation.TurnKind{operation.TurnAttended, operation.TurnUnattended} {
		for _, tracked := range []bool{false, true} {
			if size := len(describeRealRegistry(t, rig, kind, tracked)); size > operation.MaxDescriptionBytes {
				t.Errorf("%s tracked=%t: %d bytes, over the bound of %d", kind, tracked, size, operation.MaxDescriptionBytes)
			}
		}
	}
}

var describedGroups = []string{"cron", "kb"}

func groupTools(t *testing.T, rig *registryRig, group string) []toolbroker.ToolRegistration {
	t.Helper()
	var found []toolbroker.ToolRegistration
	registrations := registeredTools(t, rig)
	for i := range registrations {
		if strings.HasPrefix(registrations[i].Definition.Operation, group+".") {
			found = append(found, registrations[i])
		}
	}
	if len(found) == 0 {
		t.Fatalf("the registry holds no %s tool", group)
	}
	return found
}

func groupMarkers(t *testing.T, description, group string) map[string]string {
	t.Helper()
	for line := range strings.SplitSeq(description, "\n") {
		if !strings.HasPrefix(line, "- "+group+": ") {
			continue
		}
		verbs := line[strings.LastIndex(line, ": ")+2:]
		markers := map[string]string{}
		for verb := range strings.SplitSeq(verbs, ", ") {
			name := strings.TrimRight(verb, "?^")
			markers[name] = strings.TrimPrefix(verb, name)
		}
		return markers
	}
	t.Fatalf("the description lists no %s group:\n%s", group, description)
	return nil
}

func TestOperationDescriptionMarkersMatchTheBrokerForTheRealRegistry(t *testing.T) {
	for _, group := range describedGroups {
		for _, kind := range []operation.TurnKind{operation.TurnAttended, operation.TurnUnattended} {
			for _, tracked := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s %s tracked=%t", group, kind, tracked), func(t *testing.T) {
					rig := newRegistryRig(t, nil)
					markers := groupMarkers(t, describeRealRegistry(t, rig, kind, tracked), group)
					registrations := groupTools(t, rig, group)
					if len(markers) != len(registrations) {
						t.Fatalf("the description lists %d %s verbs, the registry holds %d tools", len(markers), group, len(registrations))
					}
					for i := range registrations {
						name := registrations[i].Definition.Name
						verb := strings.TrimPrefix(registrations[i].Definition.Operation, group+".")
						marker, listed := markers[strings.ReplaceAll(verb, "_", "-")]
						if !listed {
							t.Fatalf("the description does not list %s", verb)
						}
						request := rig.request(name, rig.sampleInput(t, name))
						request.Unattended = kind == operation.TurnUnattended
						request.ContentTaintTracked = tracked
						before := len(rig.Prompts())

						got := rig.Execute(t.Context(), request)
						asked := len(rig.Prompts()) - before

						switch {
						case marker == "" && (got.Class == toolbroker.ResultOwnerTurnRequired || asked != 0):
							t.Errorf("%s is unmarked and the broker answered %s after %d cards", verb, got.Class, asked)
						case marker != "" && kind == operation.TurnAttended && asked != 1:
							t.Errorf("%s is marked %q and the broker raised %d cards, want one", verb, marker, asked)
						case marker != "" && kind == operation.TurnUnattended && (got.Class != toolbroker.ResultOwnerTurnRequired || asked != 0):
							t.Errorf("%s is marked %q on an unattended turn and the broker answered %s after %d cards, want a refusal", verb, marker, got.Class, asked)
						}
					}
				})
			}
		}
	}
}

func operationSurfaceExecutor(t *testing.T, rig *registryRig, model *operationtest.ScriptedModel) *runtimeadk.ADKExecutor {
	t.Helper()
	cfg := appconfig.Default()
	registry, err := auraagent.Build(nil, personaToolNames(&cfg), appconfig.BuiltinToolDefinitions(), []string{"primary"}, "en")
	if err != nil {
		t.Fatalf("agent registry: %v", err)
	}
	builtin, err := runtimeadk.NewBrokerExecutor(rig.Broker, capabilitytest.Host(t))
	if err != nil {
		t.Fatalf("NewBrokerExecutor: %v", err)
	}
	port := runtimeadk.NewOperationPort()
	executor, err := runtimeadk.NewADKExecutor(
		"aura", "operation-model", auramodel.Set{"operation-model": model}, turnstore.NewSessionService(rig.DB), builtin, nil, slog.New(slog.DiscardHandler),
		runtimeadk.WithAgentResolver(registry, func(string) (string, error) { return "operation-model", nil }),
		runtimeadk.WithBuiltinToolExecutor(builtin),
		runtimeadk.WithOperations(port),
	)
	if err != nil {
		t.Fatalf("NewADKExecutor: %v", err)
	}
	if err := port.Bind(rig.Discovery, builtin); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	return executor
}

type surfaceSession struct {
	t        *testing.T
	rig      *registryRig
	executor *runtimeadk.ADKExecutor
	model    *operationtest.ScriptedModel
	events   turnstore.EventStore
	turns    int
}

func (s *surfaceSession) turn(origin turn.Origin, mutate func(*turn.TurnRequest), script ...[]*genai.Part) []operationtest.ModelCall {
	s.t.Helper()
	before := len(s.model.Calls())
	s.model.Play(script...)
	s.turns++
	req := &turn.TurnRequest{
		TurnID: fmt.Sprintf("turn-%d", s.turns), SessionID: operationtest.SessionID, PrincipalID: operationtest.OwnerID,
		PrincipalClass: principal.ClassOwner, Origin: origin,
		Parts: []runtimeingress.InputPart{{Text: "set up a nightly summary"}},
	}
	if mutate != nil {
		mutate(req)
	}
	for ev, err := range s.executor.Execute(s.t.Context(), req) {
		if err != nil {
			s.t.Fatalf("Execute: %v", err)
		}
		if ev.Kind != turnstore.EventKindADK {
			continue
		}
		if _, err := s.events.AppendSequenced(s.t.Context(), ev.SessionID, &ev); err != nil {
			s.t.Fatalf("persist the event: %v", err)
		}
	}
	return s.model.Calls()[before:]
}

func cronToolsOf(call operationtest.ModelCall) []string {
	var names []string
	for _, name := range call.Tools {
		if strings.HasPrefix(name, "aura_cron_") {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}

func answersMentioning(call operationtest.ModelCall, text string) int {
	count := 0
	for _, answer := range call.Answers {
		if strings.Contains(answer, text) {
			count++
		}
	}
	return count
}

func jobStates(t *testing.T, rig *registryRig) map[string]string {
	t.Helper()
	rows, err := rig.DB.QueryContext(t.Context(), `SELECT id, state FROM scheduled_job ORDER BY id`)
	if err != nil {
		t.Fatalf("read the jobs: %v", err)
	}
	defer func() { _ = rows.Close() }()
	states := map[string]string{}
	for rows.Next() {
		var id, state string
		if err := rows.Scan(&id, &state); err != nil {
			t.Fatalf("scan a job: %v", err)
		}
		states[id] = state
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the jobs: %v", err)
	}
	return states
}

func TestOperationDiscoveryEndToEnd(t *testing.T) {
	rig := newRegistryRig(t, func(context.Context, *toolbroker.ApprovalPrompt) (bool, error) { return true, nil })
	model := &operationtest.ScriptedModel{}
	session := &surfaceSession{t: t, rig: rig, executor: operationSurfaceExecutor(t, rig, model), model: model, events: turnstore.NewEventStore(rig.DB)}
	cron := len(groupTools(t, rig, "cron"))
	add := operationtest.CallOf("aura_cron_add", map[string]any{"name": "nightly", "schedule": "0 21 * * *", "timezone": "UTC", "prompt": "summarize"})
	list := operationtest.CallOf("aura_cron_list", map[string]any{})
	var jobID string
	cards := func() []toolbroker.ApprovalPrompt { return rig.Prompts() }

	t.Run("an attended owner turn searches, loads the group and adds a job after one card with its preview", func(t *testing.T) {
		calls := session.turn(turn.OriginTerminal, nil, operationtest.CallOf(tools.OperationSearchToolName, map[string]any{"group": "cron"}), add)
		if len(calls) != 3 {
			t.Fatalf("model calls = %d, want 3", len(calls))
		}
		if !slices.Contains(calls[0].Tools, tools.OperationSearchToolName) || len(cronToolsOf(calls[0])) != 0 {
			t.Errorf("the first call declares %v, want the discovery tool and no cron tool", calls[0].Tools)
		}
		if got := cronToolsOf(calls[1]); len(got) != cron {
			t.Errorf("the call after the search declares %v, want every cron tool loaded", got)
		}
		if !strings.Contains(calls[0].System, "# What you can operate") || !strings.Contains(calls[0].System, "- cron: ") {
			t.Errorf("the instruction lacks the description of the cron group:\n%s", calls[0].System)
		}
		prompts := cards()
		if len(prompts) != 1 || prompts[0].ToolName != "aura_cron_add" {
			t.Fatalf("approval cards = %+v, want one for aura_cron_add", prompts)
		}
		for _, line := range []string{"Schedule: 0 21 * * * (UTC)", "Name: nightly", "Runs prompt: summarize"} {
			if !strings.Contains(prompts[0].Arguments, line) {
				t.Errorf("the card %q lacks the preview line %q", prompts[0].Arguments, line)
			}
		}
		states := jobStates(t, rig)
		if len(states) != 1 {
			t.Fatalf("jobs after the approved add = %v, want 1", states)
		}
		for id := range states {
			jobID = id
		}
	})

	t.Run("a read runs without a card", func(t *testing.T) {
		before := len(cards())
		calls := session.turn(turn.OriginTerminal, nil, list)
		if got := len(cards()) - before; got != 0 {
			t.Fatalf("approval cards for aura_cron_list = %d, want none", got)
		}
		if answersMentioning(calls[len(calls)-1], jobID) == 0 {
			t.Errorf("the model saw %q after listing, want the job %s", calls[len(calls)-1].Answers, jobID)
		}
	})

	t.Run("a change asks because the content is unverified and applies after approval", func(t *testing.T) {
		before := len(cards())
		session.turn(turn.OriginTerminal, nil, operationtest.CallOf("aura_cron_pause", map[string]any{"id": jobID}))
		prompts := cards()[before:]
		if len(prompts) != 1 || prompts[0].ToolName != "aura_cron_pause" || prompts[0].ReasonCode != approval.ReasonContentUnverified {
			t.Fatalf("approval cards = %+v, want one for aura_cron_pause with reason %s", prompts, approval.ReasonContentUnverified)
		}
		if state := jobStates(t, rig)[jobID]; state != "paused" {
			t.Errorf("job state after the approved pause = %q, want paused", state)
		}
	})

	t.Run("a scheduled turn keeps the loaded tools, sees the unattended description and is refused every change", func(t *testing.T) {
		before, jobs := len(cards()), jobStates(t, rig)
		calls := session.turn(turn.OriginSchedule, nil, add)
		if got := cronToolsOf(calls[0]); len(got) != cron {
			t.Errorf("a later scheduled turn declares %v, want the loaded cron tools kept", got)
		}
		if !strings.Contains(calls[0].System, "Nobody can approve on this turn") {
			t.Errorf("a scheduled turn lacks the unattended description:\n%s", calls[0].System)
		}
		if got := len(cards()) - before; got != 0 {
			t.Errorf("approval cards = %d, want the scheduled turn to raise none", got)
		}
		if !maps.Equal(jobStates(t, rig), jobs) {
			t.Errorf("jobs changed on a scheduled turn: %v", jobStates(t, rig))
		}
		if answersMentioning(calls[len(calls)-1], string(toolbroker.ResultOwnerTurnRequired)) == 0 {
			t.Errorf("the model saw %q after the scheduled add, want an %s answer", calls[len(calls)-1].Answers, toolbroker.ResultOwnerTurnRequired)
		}
	})

	t.Run("an http owner turn runs reads and is refused changes and privileged calls", func(t *testing.T) {
		before, jobs := len(cards()), jobStates(t, rig)
		calls := session.turn(turn.OriginHTTP, nil, list, operationtest.CallOf("aura_cron_resume", map[string]any{"id": jobID}), add)
		last := calls[len(calls)-1]
		if got := answersMentioning(last, jobID); got < 2 {
			t.Errorf("the model saw %d answers naming the job, want the earlier listing and the new one", got)
		}
		if refusals := answersMentioning(last, string(toolbroker.ResultOwnerTurnRequired)); refusals < 3 {
			t.Errorf("refusals seen = %d, want the scheduled add and the http resume and add", refusals)
		}
		if got := len(cards()) - before; got != 0 {
			t.Errorf("approval cards = %d, want none on an http turn", got)
		}
		if !maps.Equal(jobStates(t, rig), jobs) {
			t.Errorf("jobs changed on an http turn: %v", jobStates(t, rig))
		}
	})

	t.Run("a trusted principal in a session that holds search results is offered no operation tool", func(t *testing.T) {
		calls := session.turn("telegram:main", func(req *turn.TurnRequest) {
			req.PrincipalClass, req.PrincipalID = principal.ClassTrusted, "trusted-friend"
		})
		for _, name := range calls[0].Tools {
			if strings.HasPrefix(name, "aura_") {
				t.Errorf("a trusted principal is offered %q", name)
			}
		}
		if strings.Contains(calls[0].System, "# What you can operate") {
			t.Errorf("a trusted principal's instruction carries the description:\n%s", calls[0].System)
		}
	})

	t.Run("a tainted attended turn refuses a trust grant without a card and still asks for a change", func(t *testing.T) {
		before := len(cards())
		calls := session.turn(turn.OriginTerminal, func(req *turn.TurnRequest) {
			req.UntrustedContext = &turn.UntrustedRecall{Query: "notes", Summary: "remembered"}
		}, add, operationtest.CallOf("aura_cron_resume", map[string]any{"id": jobID}))
		last := calls[len(calls)-1]
		if answersMentioning(last, string(toolbroker.ResultUntrustedInputTrustGrant)) == 0 {
			t.Errorf("the model saw %q, want the add refused as %s", last.Answers, toolbroker.ResultUntrustedInputTrustGrant)
		}
		prompts := cards()[before:]
		if len(prompts) != 1 || prompts[0].ToolName != "aura_cron_resume" {
			t.Fatalf("approval cards = %+v, want exactly one, for aura_cron_resume", prompts)
		}
		if states := jobStates(t, rig); len(states) != 1 || states[jobID] != "active" {
			t.Errorf("jobs = %v, want the one job resumed and no new one", states)
		}
	})
}

func TestOperationDescriptionNeverClaimsLessFrictionThanTheBrokerOnTaintedTurns(t *testing.T) {
	const clause = "A verb marked ^ that grants trust is refused, with no card, when this conversation carries untrusted input"
	for _, group := range describedGroups {
		for _, kind := range []operation.TurnKind{operation.TurnAttended, operation.TurnUnattended} {
			for _, tracked := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s %s tracked=%t", group, kind, tracked), func(t *testing.T) {
					rig := newRegistryRig(t, nil)
					description := describeRealRegistry(t, rig, kind, tracked)
					markers := groupMarkers(t, description, group)
					registrations := groupTools(t, rig, group)
					for i := range registrations {
						name := registrations[i].Definition.Name
						verb := strings.ReplaceAll(strings.TrimPrefix(registrations[i].Definition.Operation, group+"."), "_", "-")
						request := rig.request(name, rig.sampleInput(t, name))
						request.Tainted = true
						request.Unattended = kind == operation.TurnUnattended
						request.ContentTaintTracked = tracked
						before := len(rig.Prompts())

						got := rig.Execute(t.Context(), request)
						asked := len(rig.Prompts()) - before

						refusedWithoutCard := got.Class == toolbroker.ResultUntrustedInputTrustGrant
						if refusedWithoutCard && (markers[verb] != "^" || !strings.Contains(description, clause)) {
							t.Errorf("%s is refused as a trust grant on a tainted turn but the description marks it %q (clause present: %t)", verb, markers[verb], strings.Contains(description, clause))
						}
						if asked > 0 && markers[verb] == "" && !strings.Contains(description, "unless this conversation carries untrusted input") && kind == operation.TurnAttended {
							t.Errorf("%s asks on a tainted turn but the description promises no card for it", verb)
						}
						if got.Class == toolbroker.ResultOwnerTurnRequired && markers[verb] == "" {
							t.Errorf("%s is refused on a tainted %s turn but the description marks it as running", verb, kind)
						}
					}
				})
			}
		}
	}
}

func TestSearchStatesWhatAppliesOnATaintedTurn(t *testing.T) {
	rig := newRegistryRig(t, nil)
	request := operationtest.RequestFor(tools.OperationSearchToolName, json.RawMessage(`{"group":"cron"}`), "search-tainted")
	request.Tainted = true
	ctx := operation.WithSearchScope(t.Context(), &operation.SearchScope{Kind: operation.TurnAttended})
	outcome := rig.Execute(ctx, request)
	if outcome.Class != toolbroker.ResultOK {
		t.Fatalf("search = %+v, want ok", outcome)
	}
	var response struct {
		Operations []struct {
			ID       string `json:"id"`
			Approval string `json:"approval"`
			Chat     string `json:"chat"`
			Command  string `json:"command"`
		} `json:"operations"`
	}
	if err := json.Unmarshal([]byte(outcome.Output), &response); err != nil {
		t.Fatalf("decode the search result: %v", err)
	}
	approvals := map[string]string{}
	for _, entry := range response.Operations {
		approvals[entry.ID] = entry.Approval
		if entry.Chat != "" || !strings.HasPrefix(entry.Command, "aura cron ") {
			t.Errorf("%s: chat %q, command %q, want the terminal form only while chat commands do not exist", entry.ID, entry.Chat, entry.Command)
		}
	}
	if approvals["cron.add"] != "refused_here" {
		t.Errorf("cron.add approval on a tainted turn = %q, want refused_here", approvals["cron.add"])
	}
}
