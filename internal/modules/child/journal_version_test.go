package child

import (
	"regexp"
	"strings"
	"testing"
	"time"
)

var freshChildClock = regexp.MustCompile(`^\{"now":"[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9:.]+Z","journal":1\}$`)

func TestChildJournalOfAnotherVersionIsRefused(t *testing.T) {
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	cases := []struct {
		name   string
		record string
	}{
		{name: "a bare instant", record: `"` + stamp + `"`},
		{name: "another journal version", record: `{"now":"` + stamp + `","journal":0}`},
		{name: "an unknown field", record: `{"now":"` + stamp + `","journal":1,"extra":true}`},
		{name: "trailing data", record: `{"now":"` + stamp + `","journal":1} {}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			handler, runs, exec := testHandler()
			inv := newFakeJournal(startPayload(t))
			inv.journal["child-clock/sess-child"] = []byte(tc.record)

			err := handler.HandleDurable(t.Context(), inv, time.Now().UTC)

			if code, ok := CodeOf(err); !ok || code != ErrorCodeChildUnavailable || !strings.Contains(err.Error(), "child journal was written by another build") {
				t.Errorf("HandleDurable = %v, want %s from another build", err, ErrorCodeChildUnavailable)
			}
			if calls := exec.Calls(); calls != 0 {
				t.Errorf("child executor calls = %d, want 0", calls)
			}
			for _, key := range inv.actionKeys() {
				if key == "child-exec/sess-child" {
					t.Errorf("journaled actions = %v, want child-exec never run", inv.actionKeys())
				}
			}
			if got := runs.runs["ch-1"].State; got != StatusFailed {
				t.Errorf("child state = %s, want %s", got, StatusFailed)
			}
		})
	}
	t.Run("a fresh child run writes the current record", func(t *testing.T) {
		handler, _, _ := testHandler()
		inv := newFakeJournal(startPayload(t))
		if err := handler.HandleDurable(t.Context(), inv, time.Now().UTC); err != nil {
			t.Fatalf("HandleDurable: %v", err)
		}
		if raw := string(inv.journal["child-clock/sess-child"]); !freshChildClock.MatchString(raw) {
			t.Errorf("child-clock record = %s, want {\"now\":<instant>,\"journal\":1}", raw)
		}
	})
}
