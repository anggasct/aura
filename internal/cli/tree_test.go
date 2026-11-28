package cli

import (
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/app"
)

var updateGolden = flag.Bool("update", false, "rewrite golden files")

const legacyCommandsGolden = "legacy-cli-commands.txt"

func commandLeaves(root *cobra.Command) (generated, handWritten []string) {
	var walk func(cmd *cobra.Command)
	walk = func(cmd *cobra.Command) {
		children := cmd.Commands()
		if len(children) == 0 && cmd != root {
			path := strings.TrimPrefix(cmd.CommandPath(), root.Name()+" ")
			if app.IsOperationCommand(cmd) {
				generated = append(generated, path)
			} else {
				handWritten = append(handWritten, path)
			}
			return
		}
		for _, child := range children {
			if child.Name() == "help" || child.Name() == "completion" {
				continue
			}
			walk(child)
		}
	}
	walk(root)
	slices.Sort(generated)
	slices.Sort(handWritten)
	return generated, handWritten
}

func TestCLICommandsComeFromTheRegistry(t *testing.T) {
	generated, handWritten := commandLeaves(NewRootCommand())
	path := filepath.Join("..", "..", "testdata", "golden", legacyCommandsGolden)
	rendered := strings.Join(handWritten, "\n") + "\n"
	if *updateGolden {
		if err := os.WriteFile(path, []byte(rendered), 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	listed := strings.Split(strings.TrimSpace(string(raw)), "\n")
	for _, name := range handWritten {
		if !slices.Contains(listed, name) {
			t.Errorf("the hand-written command %q is not generated from an operation and is not listed in %s", name, legacyCommandsGolden)
		}
	}
	for _, name := range listed {
		switch {
		case slices.Contains(generated, name):
			t.Errorf("the command %q is generated from an operation and must leave %s", name, legacyCommandsGolden)
		case !slices.Contains(handWritten, name):
			t.Errorf("%s lists %q, which is no command of the root", legacyCommandsGolden, name)
		}
	}
	if len(generated) == 0 {
		t.Error("no leaf command of the root is generated from an operation")
	}
}

func TestCronCommandsAreAllGenerated(t *testing.T) {
	generated, _ := commandLeaves(NewRootCommand())
	want := []string{"cron add", "cron delete", "cron list", "cron pause", "cron resume", "cron run-now", "cron runs"}
	var got []string
	for _, name := range generated {
		if strings.HasPrefix(name, "cron ") {
			got = append(got, name)
		}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("generated cron commands = %v, want %v", got, want)
	}
}
