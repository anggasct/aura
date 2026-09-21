package child

import "testing"

func FuzzParseSpawnArgs(f *testing.F) {
	f.Add(`{"task":"summarize","capabilities":["search"],"budget_tokens":100}`)
	f.Add(`{}`)
	f.Add(`not json`)
	f.Add(`{"task":"","capabilities":["spawn_child"]}`)
	f.Fuzz(func(t *testing.T, text string) {
		args, err := ParseSpawnArgs([]byte(text))
		if err != nil {
			return
		}
		if args.Task == "" {
			t.Fatalf("parsed args carry an empty task: %+v", args)
		}
		for _, capability := range args.Capabilities {
			if capability == "" {
				t.Fatalf("parsed args carry an empty capability: %+v", args)
			}
		}
		if args.BudgetTokens < 0 || args.BudgetCost < 0 || args.BudgetTools < 0 {
			t.Fatalf("parsed args carry a negative budget: %+v", args)
		}
	})
}
