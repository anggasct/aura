package schedule

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/platform/capability/capabilitytest"
	"github.com/anggasct/aura/internal/platform/command"
	"github.com/anggasct/aura/internal/platform/operation"
	"github.com/anggasct/aura/internal/platform/operation/operationtest"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/toolbroker"
)

var (
	timestampPattern = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?Z?`)
	instantPattern   = regexp.MustCompile(`\d{8}T\d{6}(\.\d{9})?Z?`)
	manualPattern    = regexp.MustCompile(`manual-[0-9a-f]+`)
	identityPattern  = regexp.MustCompile(`\b(job|occ|run)_[0-9a-f]+`)
)

func scheduleState(t *testing.T, db *sql.DB) string {
	t.Helper()
	var lines []string
	for _, table := range []string{"scheduled_job", "scheduled_occurrence", "durable_run"} {
		lines = append(lines, tableLines(t, db, table)...)
	}
	slices.Sort(lines)
	return strings.Join(lines, "\n")
}

func tableLines(t *testing.T, db *sql.DB, table string) []string {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), "SELECT * FROM "+table)
	if err != nil {
		t.Fatalf("read %s: %v", table, err)
	}
	defer func() { _ = rows.Close() }()
	columns, err := rows.Columns()
	if err != nil {
		t.Fatalf("columns of %s: %v", table, err)
	}
	var lines []string
	for rows.Next() {
		values := make([]any, len(columns))
		targets := make([]any, len(columns))
		for i := range values {
			targets[i] = &values[i]
		}
		if err := rows.Scan(targets...); err != nil {
			t.Fatalf("scan %s: %v", table, err)
		}
		cells := make([]string, len(columns))
		for i, value := range values {
			cells[i] = columns[i] + "=" + normalizedCell(value)
		}
		lines = append(lines, table+": "+strings.Join(cells, " | "))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read %s: %v", table, err)
	}
	return lines
}

func normalizedCell(value any) string {
	text := fmt.Sprint(value)
	if raw, isBytes := value.([]byte); isBytes {
		text = string(raw)
	}
	text = timestampPattern.ReplaceAllString(text, "<time>")
	text = instantPattern.ReplaceAllString(text, "<instant>")
	text = manualPattern.ReplaceAllString(text, "manual-<digest>")
	return identityPattern.ReplaceAllString(text, "<id>")
}

type paritySteps struct {
	agent *scheduleRig
	cli   *scheduleRig
	chat  *scheduleRig
	calls int
}

func (p *paritySteps) viaChat(t *testing.T, text string) string {
	t.Helper()
	p.calls++
	chat, err := operation.NewChat(&operation.ChatDeps{Registry: p.chat.Registry, Capabilities: capabilitytest.Host(t)})
	if err != nil {
		t.Fatalf("NewChat: %v", err)
	}
	tokens, err := command.Tokenize(text)
	if err != nil {
		t.Fatalf("Tokenize(%q): %v", text, err)
	}
	plan := chat.Plan(GroupName, tokens)
	if plan.Refusal != nil || plan.Missing != nil {
		t.Fatalf("Plan(%q) = %+v, want an executable plan", text, plan)
	}
	caller := &command.Caller{Channel: "telegram", PrincipalID: principal.OwnerID, Class: principal.ClassOwner, SessionID: "chat-session", AgentID: "main", Direct: true}
	reply := chat.Run(t.Context(), &command.OperationRun{Caller: caller, Plan: &plan, IdempotencyKey: fmt.Sprintf("telegram:parity-%d", p.calls)})
	if reply.Code != command.ErrorCodeOK {
		t.Fatalf("%q in chat = %+v, want ok", text, reply)
	}
	return strings.Join(reply.Lines, "\n")
}

func (p *paritySteps) viaTool(t *testing.T, id, arguments string) string {
	t.Helper()
	p.calls++
	key := fmt.Sprintf("parity-%d-%s", p.calls, id)
	outcome := p.agent.Execute(t.Context(), operationtest.RequestFor(operation.ToolName(id), json.RawMessage(arguments), key))
	if outcome.Class != toolbroker.ResultOK {
		t.Fatalf("%s through the broker = %+v, want ok", id, outcome)
	}
	return outcome.Output
}

func (p *paritySteps) viaCLI(t *testing.T, id, arguments string) string {
	t.Helper()
	result, err := p.cli.Registry.Invoke(t.Context(), id, &operation.Call{
		Caller: operation.Caller{Surface: operation.SurfaceCLI, PrincipalID: principal.OwnerID, Class: principal.ClassOwner},
		Input:  json.RawMessage(arguments),
	})
	if err != nil {
		t.Fatalf("%s on the CLI surface: %v", id, err)
	}
	return string(result.Data)
}

func jobIDOf(t *testing.T, output string) string {
	t.Helper()
	var added struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(output), &added); err != nil || added.ID == "" {
		t.Fatalf("add output %q has no id: %v", output, err)
	}
	return added.ID
}

func TestCronOperationsParity(t *testing.T) {
	owner := ownerRoute{alias: "me", route: "telegram:owner"}
	cases := []struct {
		name string
		add  string
		cli  string
		chat string
	}{
		{
			name: "a prompt job",
			add:  `{"name":"nightly","schedule":"0 21 * * *","timezone":"UTC","prompt":"summarize","context":"full","overlap":"queue_one","grace":"30m"}`,
			cli:  `{"name":"nightly","schedule":"0 21 * * *","timezone":"UTC","prompt":"summarize","context":"full","overlap":"queue_one","grace":"30m","channel":"telegram","destination":"me"}`,
			chat: `add --name nightly --schedule "0 21 * * *" --timezone UTC --prompt summarize --context full --overlap queue_one --grace 30m --channel telegram --destination me`,
		},
		{
			name: "a workflow job with input",
			add:  `{"name":"weekly","schedule":"30 8 * * 1","timezone":"Asia/Jakarta","workflow":"wf1","input":{"objective":"report"}}`,
			cli:  `{"name":"weekly","schedule":"30 8 * * 1","timezone":"Asia/Jakarta","workflow":"wf1","input":{"objective":"report"},"channel":"telegram","destination":"me"}`,
			chat: `add --name weekly --schedule "30 8 * * 1" --timezone Asia/Jakarta --workflow wf1 --input '{"objective":"report"}' --channel telegram --destination me`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			steps := &paritySteps{
				agent: newConfiguredScheduleRig(t, &rigOptions{owner: owner, durable: true}),
				cli:   newConfiguredScheduleRig(t, &rigOptions{owner: owner, durable: true}),
				chat:  newConfiguredScheduleRig(t, &rigOptions{owner: owner, durable: true}),
			}
			agentID := jobIDOf(t, steps.viaTool(t, "cron.add", tc.add))
			cliID := jobIDOf(t, steps.viaCLI(t, "cron.add", tc.cli))
			chatID := strings.TrimPrefix(steps.viaChat(t, tc.chat), "id: ")
			if !identityPattern.MatchString(chatID) {
				t.Fatalf("the chat add replied %q, want the job id", chatID)
			}
			requireSameState(t, steps, "add")
			for _, verb := range []string{"cron.pause", "cron.resume", "cron.run-now", "cron.delete"} {
				steps.viaTool(t, verb, `{"id":"`+agentID+`"}`)
				steps.viaCLI(t, verb, `{"id":"`+cliID+`"}`)
				steps.viaChat(t, strings.TrimPrefix(verb, "cron.")+" "+chatID)
				requireSameState(t, steps, verb)
			}
			if !strings.Contains(scheduleState(t, steps.agent.db), "state=deleted") {
				t.Fatal("the job did not end deleted")
			}
		})
	}
}

func requireSameState(t *testing.T, steps *paritySteps, after string) {
	t.Helper()
	fromTool, fromCLI, fromChat := scheduleState(t, steps.agent.db), scheduleState(t, steps.cli.db), scheduleState(t, steps.chat.db)
	if fromTool != fromCLI || fromTool != fromChat {
		t.Fatalf("after %s the tool, the CLI and the chat left different state\ntool:\n%s\ncli:\n%s\nchat:\n%s", after, fromTool, fromCLI, fromChat)
	}
}

func TestAgentCronAddCardShowsWhatWillBeScheduled(t *testing.T) {
	cases := []struct {
		name      string
		arguments string
		want      []string
	}{
		{
			name:      "a prompt",
			arguments: `{"name":"nightly","schedule":"0 21 * * *","timezone":"UTC","prompt":"summarize","context":"full"}`,
			want: []string{
				"Schedule: 0 21 * * * (UTC)",
				"Name: nightly",
				"Runs prompt: summarize",
				contextFullLine,
				"Channel: telegram",
				"Destination alias: me",
				"Delivers to: telegram:owner",
			},
		},
		{
			name:      "a workflow with input",
			arguments: `{"name":"weekly","schedule":"30 8 * * 1","timezone":"Asia/Jakarta","workflow":"wf1","input":{"objective":"report"}}`,
			want: []string{
				"Schedule: 30 8 * * 1 (Asia/Jakarta)",
				"Name: weekly",
				"Starts workflow: wf1",
				`Workflow input: {"objective":"report"}`,
				"Channel: telegram",
				"Destination alias: me",
				"Delivers to: telegram:owner",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newScheduleRig(t, ownerRoute{alias: "me", route: "telegram:owner"})

			outcome := rig.Execute(t.Context(), operationtest.RequestFor("aura_cron_add", json.RawMessage(tc.arguments), "card-1"))

			if outcome.Class != toolbroker.ResultOK {
				t.Fatalf("outcome = %+v, want ok", outcome)
			}
			cards := rig.Prompts()
			if len(cards) != 1 {
				t.Fatalf("cards = %d, want 1", len(cards))
			}
			_, previewLines, found := strings.Cut(cards[0].Arguments, "Grants:\n")
			if !found || previewLines != strings.Join(tc.want, "\n") {
				t.Fatalf("preview lines of the card = %q, want %q", previewLines, strings.Join(tc.want, "\n"))
			}
		})
	}
}

func TestCronAddPreviewRefusesAnUnknownWorkflow(t *testing.T) {
	unknown := func(context.Context, string) error {
		return operation.NewError(operation.ErrorCodeInputInvalid, "workflow is not known")
	}
	rig := newWrappedScheduleRig(t, ownerRoute{alias: "me", route: "telegram:owner"}, nil)
	rig.checker.set(unknown)

	outcome := rig.Execute(t.Context(), operationtest.RequestFor("aura_cron_add", json.RawMessage(`{"name":"weekly","schedule":"30 8 * * 1","timezone":"UTC","workflow":"ghost"}`), "ghost-1"))

	if outcome.Err == nil || outcome.Class != toolbroker.ResultInvalidArgument {
		t.Fatalf("outcome = %+v, want invalid_argument", outcome)
	}
	if cards := rig.Prompts(); len(cards) != 0 {
		t.Fatalf("an unknown workflow raised %d cards, want none", len(cards))
	}
}
