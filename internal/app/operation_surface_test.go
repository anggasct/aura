package app

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"google.golang.org/genai"

	appconfig "github.com/anggasct/aura/internal/app/config"
	auraagent "github.com/anggasct/aura/internal/platform/agent"
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

func cronMarkers(t *testing.T, description string) map[string]string {
	t.Helper()
	for line := range strings.SplitSeq(description, "\n") {
		if !strings.HasPrefix(line, "- cron: ") {
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
	t.Fatalf("the description lists no cron group:\n%s", description)
	return nil
}

func TestOperationDescriptionMarkersMatchTheBrokerForTheRealRegistry(t *testing.T) {
	for _, kind := range []operation.TurnKind{operation.TurnAttended, operation.TurnUnattended} {
		for _, tracked := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s tracked=%t", kind, tracked), func(t *testing.T) {
				rig := newRegistryRig(t, nil)
				markers := cronMarkers(t, describeRealRegistry(t, rig, kind, tracked))
				registrations := registeredTools(t, rig)
				if len(markers) != len(registrations) {
					t.Fatalf("the description lists %d cron verbs, the registry holds %d tools", len(markers), len(registrations))
				}
				for i := range registrations {
					name := registrations[i].Definition.Name
					verb := strings.TrimPrefix(registrations[i].Definition.Operation, "cron.")
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
	executor, err := runtimeadk.NewADKExecutor(
		"aura", "operation-model", auramodel.Set{"operation-model": model}, turnstore.NewSessionService(rig.DB), builtin, nil, slog.New(slog.DiscardHandler),
		runtimeadk.WithAgentResolver(registry, func(string) (string, error) { return "operation-model", nil }),
		runtimeadk.WithBuiltinToolExecutor(builtin),
	)
	if err != nil {
		t.Fatalf("NewADKExecutor: %v", err)
	}
	if err := executor.BindOperations(rig.Discovery); err != nil {
		t.Fatalf("BindOperations: %v", err)
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

func (s *surfaceSession) turn(origin turn.Origin, script ...[]*genai.Part) []operationtest.ModelCall {
	s.t.Helper()
	before := len(s.model.Calls())
	s.model.Play(script...)
	s.turns++
	req := &turn.TurnRequest{
		TurnID: fmt.Sprintf("turn-%d", s.turns), SessionID: operationtest.SessionID, PrincipalID: operationtest.OwnerID,
		PrincipalClass: principal.ClassOwner, Origin: origin,
		Parts: []runtimeingress.InputPart{{Text: "set up a nightly summary"}},
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

func TestOperationDiscoveryEndToEnd(t *testing.T) {
	rig := newRegistryRig(t, func(context.Context, *toolbroker.ApprovalPrompt) (bool, error) { return true, nil })
	model := &operationtest.ScriptedModel{}
	session := &surfaceSession{t: t, rig: rig, executor: operationSurfaceExecutor(t, rig, model), model: model, events: turnstore.NewEventStore(rig.DB)}
	add := operationtest.CallOf("aura_cron_add", map[string]any{"name": "nightly", "schedule": "0 21 * * *", "timezone": "UTC", "prompt": "summarize"})

	attended := session.turn(turn.OriginTerminal,
		operationtest.CallOf(tools.OperationSearchToolName, map[string]any{"group": "cron"}),
		add,
	)
	if len(attended) != 3 {
		t.Fatalf("model calls = %d, want 3", len(attended))
	}
	if !slices.Contains(attended[0].Tools, tools.OperationSearchToolName) || len(cronToolsOf(attended[0])) != 0 {
		t.Errorf("the first call declares %v, want the discovery tool and no cron tool", attended[0].Tools)
	}
	if got := cronToolsOf(attended[1]); len(got) != len(registeredTools(t, rig)) {
		t.Errorf("the call after the search declares %v, want every cron tool loaded", got)
	}
	if !strings.Contains(attended[0].System, "# What you can operate") || !strings.Contains(attended[0].System, "- cron: ") {
		t.Errorf("the instruction lacks the description of the cron group:\n%s", attended[0].System)
	}
	if prompts := rig.Prompts(); len(prompts) != 1 || prompts[0].ToolName != "aura_cron_add" {
		t.Fatalf("approval cards = %+v, want one for aura_cron_add", prompts)
	}
	if n := countRows(t, rig.DB, `SELECT COUNT(*) FROM scheduled_job`); n != 1 {
		t.Fatalf("jobs after the approved add = %d, want 1", n)
	}

	unattended := session.turn(turn.OriginSchedule, add)
	if got := cronToolsOf(unattended[0]); len(got) != len(registeredTools(t, rig)) {
		t.Errorf("a later scheduled turn declares %v, want the loaded cron tools kept", got)
	}
	if !strings.Contains(unattended[0].System, "Nobody can approve on this turn") {
		t.Errorf("a scheduled turn lacks the unattended description:\n%s", unattended[0].System)
	}
	if prompts := rig.Prompts(); len(prompts) != 1 {
		t.Errorf("approval cards = %d, want the scheduled turn to raise none", len(prompts))
	}
	if n := countRows(t, rig.DB, `SELECT COUNT(*) FROM scheduled_job`); n != 1 {
		t.Errorf("jobs after the scheduled turn = %d, want 1: an unattended add must be refused", n)
	}
	last := unattended[len(unattended)-1]
	if !slices.ContainsFunc(last.Answers, func(answer string) bool { return strings.Contains(answer, string(toolbroker.ResultOwnerTurnRequired)) }) {
		t.Errorf("the model saw %q after the scheduled add, want an %s answer", last.Answers, toolbroker.ResultOwnerTurnRequired)
	}
}
