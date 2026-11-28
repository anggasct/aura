package cli

import (
	"slices"
	"testing"

	"github.com/spf13/cobra"
)

func TestRootCommandMountsEverySubcommand(t *testing.T) {
	root := NewRootCommand()

	wantTop := []string{
		"agents", "apikey", "bindings", "broadcast", "chat", "children", "config", "cron", "doctor",
		"effects", "exec", "grants", "handoff", "interpreter", "kb", "mcp", "memory",
		"models", "owner", "pairing", "persona", "plugin", "principal", "profile", "reply",
		"server", "skills", "status", "storage", "sync", "usage", "version", "workflow",
	}

	gotByName := map[string]*cobra.Command{}
	var gotTop []string
	for _, sub := range root.Commands() {
		if sub.Hidden {
			continue
		}
		gotByName[sub.Name()] = sub
		gotTop = append(gotTop, sub.Name())
	}
	slices.Sort(gotTop)
	if !slices.Equal(gotTop, wantTop) {
		t.Fatalf("top-level commands = %v, want %v", gotTop, wantTop)
	}

	wantSubs := map[string][]string{
		"agents":      {"list", "show"},
		"apikey":      {"create", "list", "revoke"},
		"bindings":    {"explain"},
		"broadcast":   {"reconcile", "status"},
		"children":    {"cancel", "list", "show"},
		"config":      {"check", "schema"},
		"cron":        {"add", "confirm", "delete", "list", "pause", "resume", "run-now", "runs"},
		"effects":     {"approve", "list", "mark", "prune", "reconcile", "retry"},
		"grants":      {"list", "revoke"},
		"handoff":     {"list", "release"},
		"interpreter": {"install", "list", "remove"},
		"kb":          {"add", "check", "embed", "list", "refresh", "remove", "show", "unembed"},
		"mcp":         {"list", "trust"},
		"memory":      {"forget", "inspect", "purge"},
		"models":      {"circuit-reset", "circuits", "routes"},
		"owner":       {"rekey", "whoami"},
		"pairing":     {"approve", "clear-pending", "list", "revoke"},
		"persona":     {"eval", "eval-results", "list"},
		"plugin":      {"approve", "disable", "install", "list", "login", "logout", "remove", "review"},
		"principal":   {"forget", "list", "show"},
		"profile":     {"accept", "delete", "list", "reject", "review", "set", "show"},
		"skills":      {"accept", "create", "disable", "doctor", "list", "reject", "review", "show", "validate"},
		"storage":     {"backup", "gc", "reconcile", "restore", "upgrade", "verify"},
		"sync":        {"doctor", "reconcile", "run", "status"},
		"usage":       {"entries", "prices", "reconcile", "status"},
		"workflow":    {"approve", "cancel", "definitions", "inspect", "proposal", "proposals", "runs", "signal", "start", "validate"},
	}

	for parentName, wantChildren := range wantSubs {
		parent := gotByName[parentName]
		if parent == nil {
			t.Fatalf("missing top-level command %q", parentName)
		}
		var gotChildren []string
		for _, child := range parent.Commands() {
			if child.Hidden {
				continue
			}
			gotChildren = append(gotChildren, child.Name())
		}
		slices.Sort(gotChildren)
		if !slices.Equal(gotChildren, wantChildren) {
			t.Errorf("%s subcommands = %v, want %v", parentName, gotChildren, wantChildren)
		}
	}
}
