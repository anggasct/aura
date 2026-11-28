package knowledge_test

import (
	"database/sql"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/modules/knowledge/knowledgetest"
	"github.com/anggasct/aura/internal/platform/operation"
	"github.com/anggasct/aura/internal/platform/toolbroker"
)

var (
	parityTimestamp = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?Z?`)
	paritySourceID  = regexp.MustCompile(`kb[sc]_[0-9a-z]+`)
)

func knowledgeState(t *testing.T, db *sql.DB) string {
	t.Helper()
	var lines []string
	for _, table := range []string{"kb_collection", "kb_source", "kb_chunk", "kb_embedding"} {
		rows, err := db.QueryContext(t.Context(), "SELECT * FROM "+table)
		if err != nil {
			t.Fatalf("read %s: %v", table, err)
		}
		columns, err := rows.Columns()
		if err != nil {
			t.Fatalf("columns of %s: %v", table, err)
		}
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
				text := fmt.Sprint(value)
				if raw, isBytes := value.([]byte); isBytes {
					text = string(raw)
				}
				text = parityTimestamp.ReplaceAllString(text, "<time>")
				cells[i] = columns[i] + "=" + paritySourceID.ReplaceAllString(text, "<id>")
			}
			lines = append(lines, table+": "+strings.Join(cells, " | "))
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("read %s: %v", table, err)
		}
		_ = rows.Close()
	}
	slices.Sort(lines)
	return strings.Join(lines, "\n")
}

func TestKnowledgeOperationsParity(t *testing.T) {
	const page = knowledgetest.PageURL
	steps := []struct {
		name string
		id   string
		tool string
		cli  string
		chat string
	}{
		{name: "add", id: "kb.add", tool: `{"collection":"support","source":"` + page + `"}`, cli: `{"collection":"support","source":"` + page + `"}`, chat: `{"collection":"support","url":"` + page + `"}`},
		{name: "add again", id: "kb.add", tool: `{"collection":"support","source":"` + page + `"}`, cli: `{"collection":"support","source":"` + page + `"}`, chat: `{"collection":"support","url":"` + page + `"}`},
		{name: "embed", id: "kb.embed", tool: `{"collection":"support"}`, cli: `{"collection":"support"}`, chat: `{"collection":"support"}`},
		{name: "refresh", id: "kb.refresh", tool: `{"collection":"support"}`, cli: `{"collection":"support"}`, chat: `{"collection":"support"}`},
		{name: "unembed", id: "kb.unembed", tool: `{"collection":"support"}`, cli: `{"collection":"support"}`, chat: `{"collection":"support"}`},
		{name: "remove", id: "kb.remove", tool: `{"collection":"support","source":"` + page + `"}`, cli: `{"collection":"support","source":"` + page + `"}`, chat: `{"collection":"support","source":"` + page + `"}`},
	}
	agent, cli, chat := newOpsRig(t), newOpsRig(t), newOpsRig(t)
	for _, step := range steps {
		if outcome := agent.tool(step.id, step.tool); outcome.Class != toolbroker.ResultOK {
			t.Fatalf("%s through the broker = %+v, want ok", step.name, outcome)
		}
		if _, err := cli.invoke(t, operation.SurfaceCLI, step.id, step.cli); err != nil {
			t.Fatalf("%s on the CLI surface: %v", step.name, err)
		}
		if _, err := chat.invoke(t, operation.SurfaceChat, step.id, step.chat); err != nil {
			t.Fatalf("%s in chat: %v", step.name, err)
		}
		fromTool, fromCLI, fromChat := knowledgeState(t, agent.DB), knowledgeState(t, cli.DB), knowledgeState(t, chat.DB)
		if fromTool != fromCLI || fromTool != fromChat {
			t.Fatalf("after %s the tool, the CLI and the chat left different state\ntool:\n%s\ncli:\n%s\nchat:\n%s", step.name, fromTool, fromCLI, fromChat)
		}
	}
	if state := knowledgeState(t, agent.DB); strings.Contains(state, "kb_source:") {
		t.Errorf("the source survived the remove:\n%s", state)
	}
}
