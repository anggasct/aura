package effect

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/anggasct/aura/internal/store/storetest"
)

type outcomeLog struct {
	mu       sync.Mutex
	outcomes []string
}

func (l *outcomeLog) Observe(_ context.Context, observation *Observation) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.outcomes = append(l.outcomes, observation.Outcome)
}

func (l *outcomeLog) count(outcome string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, got := range l.outcomes {
		if got == outcome {
			n++
		}
	}
	return n
}

func newObservedExecutor(t *testing.T) (*Executor, *sql.DB, *outcomeLog) {
	t.Helper()
	observed := &outcomeLog{}
	db := storetest.OpenMigrated(t)
	journal, err := NewJournal(db, Options{Logger: slog.New(slog.DiscardHandler), Observer: observed})
	if err != nil {
		t.Fatalf("new journal: %v", err)
	}
	if err := seedSession(t, db); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	executor, err := NewExecutor(journal)
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	return executor, db, observed
}

const recordedToolResult = `{"tool_name":"exec","tool_version":"v1","class":"ok","untrusted":true,"output":{"a":1},"truncated":false}`

func succeedWithOutcome(t *testing.T, executor *Executor, n int, outcome Outcome) *Intent {
	t.Helper()
	outcome.Succeeded = true
	intent, err := executor.Execute(context.Background(), validPrepare(n), &fakeProvider{outcome: outcome})
	if err != nil {
		if stuck, listErr := executor.Journal().ListByState(context.Background(), StateStarted, 1); listErr == nil && len(stuck) == 1 {
			t.Fatalf("intent state = %s, want succeeded: %v", stuck[0].State, err)
		}
		t.Fatalf("execute %d: %v", n, err)
	}
	return intent
}

func wantResultCode(t *testing.T, err error, want ErrorCode) {
	t.Helper()
	if code, ok := CodeOf(err); !ok || code != want {
		t.Fatalf("RecordedResult error = %v, want %s", err, want)
	}
}

func TestExecutorRecordsTheResultOfASucceededEffect(t *testing.T) {
	t.Parallel()
	executor, db, _ := newObservedExecutor(t)
	intent := succeedWithOutcome(t, executor, 1, Outcome{Receipt: json.RawMessage(`{}`), Result: json.RawMessage(`{"ok":true}`)})
	if intent.State != StateSucceeded {
		t.Fatalf("intent state = %s, want succeeded", intent.State)
	}
	record, err := executor.Journal().RecordedResult(context.Background(), intent)
	if err != nil {
		t.Fatalf("RecordedResult: %v", err)
	}
	if string(record) != `{"ok":true}` {
		t.Fatalf("recorded result = %s, want %s", record, `{"ok":true}`)
	}
	var gotDigest string
	if err := db.QueryRowContext(context.Background(), `SELECT result_digest FROM effect_intent WHERE id = ?`, intent.ID).Scan(&gotDigest); err != nil {
		t.Fatalf("read result digest: %v", err)
	}
	sum := sha256.Sum256([]byte("effect-result/v1\n" + intent.ID + "\n" + intent.RequestDigest + "\n" + `{"ok":true}`))
	if wantDigest := hex.EncodeToString(sum[:]); gotDigest != wantDigest {
		t.Fatalf("result digest = %s, want %s", gotDigest, wantDigest)
	}
}

func TestRecordedResultIsServedOnlyWhenRecordedAndIntact(t *testing.T) {
	t.Parallel()
	oversized := json.RawMessage(`"` + strings.Repeat("a", MaxResultBytes) + `"`)
	exactlyAtTheCap := json.RawMessage(`"` + strings.Repeat("a", MaxResultBytes-2) + `"`)
	cases := []struct {
		name            string
		outcome         Outcome
		wantCode        ErrorCode
		wantNotRecorded int
	}{
		{name: "a success without a result", outcome: Outcome{Receipt: json.RawMessage(`{}`)}, wantCode: ErrorCodeResultUnavailable},
		{name: "a result above the cap", outcome: Outcome{Result: oversized}, wantCode: ErrorCodeResultUnavailable, wantNotRecorded: 1},
		{name: "a result at the cap", outcome: Outcome{Result: exactlyAtTheCap}},
		{name: "a result that is not JSON", outcome: Outcome{Result: json.RawMessage(`not json`)}, wantCode: ErrorCodeResultUnavailable, wantNotRecorded: 1},
		{name: "a result that is not UTF-8", outcome: Outcome{Result: json.RawMessage("\"\xff\"")}, wantCode: ErrorCodeResultUnavailable, wantNotRecorded: 1},
		{name: "a provider that could not encode its result", outcome: Outcome{UnrecordedResult: true}, wantCode: ErrorCodeResultUnavailable, wantNotRecorded: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			executor, _, observed := newObservedExecutor(t)
			intent := succeedWithOutcome(t, executor, 1, tc.outcome)
			if intent.State != StateSucceeded {
				t.Fatalf("intent state = %s, want succeeded", intent.State)
			}
			record, err := executor.Journal().RecordedResult(context.Background(), intent)
			if tc.wantCode == "" {
				if err != nil || string(record) != string(tc.outcome.Result) {
					t.Fatalf("RecordedResult error = %v, want the recorded result", err)
				}
			} else {
				wantResultCode(t, err, tc.wantCode)
			}
			if got := observed.count("result_not_recorded"); got != tc.wantNotRecorded {
				t.Fatalf("observed result_not_recorded %d times, want %d", got, tc.wantNotRecorded)
			}
			wantReplayed := 0
			if tc.wantCode == "" {
				wantReplayed = 1
			}
			if got := observed.count("result_replayed"); got != wantReplayed {
				t.Fatalf("observed result_replayed %d times, want %d", got, wantReplayed)
			}
		})
	}

	t.Run("a success recorded through Succeed", func(t *testing.T) {
		t.Parallel()
		executor, _, observed := newObservedExecutor(t)
		journal := executor.Journal()
		prepared := mustPrepare(t, journal, validPrepare(1))
		mustStart(t, journal, prepared.ID)
		intent, err := journal.Succeed(context.Background(), prepared.ID, json.RawMessage(`{}`))
		if err != nil {
			t.Fatalf("Succeed: %v", err)
		}
		_, err = journal.RecordedResult(context.Background(), intent)
		wantResultCode(t, err, ErrorCodeResultUnavailable)
		if got := observed.count("result_not_recorded"); got != 0 {
			t.Fatalf("observed result_not_recorded %d times, want 0", got)
		}
	})

	t.Run("a tampered record", func(t *testing.T) {
		t.Parallel()
		executor, db, _ := newObservedExecutor(t)
		intent := succeedWithOutcome(t, executor, 1, Outcome{Result: json.RawMessage(recordedToolResult)})
		tampered := strings.Replace(recordedToolResult, `"output":{"a":1}`, `"output":{"a":2}`, 1)
		if _, err := db.ExecContext(context.Background(), `UPDATE effect_intent SET result_json = ? WHERE id = ?`, tampered, intent.ID); err != nil {
			t.Fatalf("tamper: %v", err)
		}
		_, err := executor.Journal().RecordedResult(context.Background(), intent)
		wantResultCode(t, err, ErrorCodeResultCorrupt)
	})

	t.Run("the record of another intent with the same request digest", func(t *testing.T) {
		t.Parallel()
		executor, db, _ := newObservedExecutor(t)
		first := succeedWithOutcome(t, executor, 1, Outcome{Result: json.RawMessage(recordedToolResult)})
		second := succeedWithOutcome(t, executor, 2, Outcome{Result: json.RawMessage(recordedToolResult)})
		if first.RequestDigest != second.RequestDigest {
			t.Fatal("the two intents must share a request digest")
		}
		if _, err := db.ExecContext(context.Background(),
			`UPDATE effect_intent SET result_json = (SELECT result_json FROM effect_intent WHERE id = ?1), result_digest = (SELECT result_digest FROM effect_intent WHERE id = ?1) WHERE id = ?2`,
			first.ID, second.ID); err != nil {
			t.Fatalf("copy record: %v", err)
		}
		_, err := executor.Journal().RecordedResult(context.Background(), second)
		wantResultCode(t, err, ErrorCodeResultCorrupt)
	})

	t.Run("an intent that did not succeed", func(t *testing.T) {
		t.Parallel()
		executor, _, _ := newObservedExecutor(t)
		journal := executor.Journal()
		prepared := mustPrepare(t, journal, validPrepare(1))
		_, err := journal.RecordedResult(context.Background(), prepared)
		wantResultCode(t, err, ErrorCodeResultUnavailable)
		unknown := mustPrepare(t, journal, validPrepare(2))
		mustStart(t, journal, unknown.ID)
		settled, err := journal.MarkUnknown(context.Background(), unknown.ID)
		if err != nil {
			t.Fatalf("MarkUnknown: %v", err)
		}
		_, err = journal.RecordedResult(context.Background(), settled)
		wantResultCode(t, err, ErrorCodeResultUnavailable)
	})

	t.Run("a nil intent", func(t *testing.T) {
		t.Parallel()
		executor, _, _ := newObservedExecutor(t)
		_, err := executor.Journal().RecordedResult(context.Background(), nil)
		wantResultCode(t, err, ErrorCodeInvalidArgument)
	})
}

func TestRecordedResultColumnsRefuseAPartialRecord(t *testing.T) {
	t.Parallel()
	executor, db, _ := newObservedExecutor(t)
	journal := executor.Journal()
	started := mustPrepare(t, journal, validPrepare(1))
	mustStart(t, journal, started.ID)
	unrecorded := succeedWithOutcome(t, executor, 2, Outcome{})
	recorded := succeedWithOutcome(t, executor, 3, Outcome{Result: json.RawMessage(`{"ok":true}`)})
	atTheCap := `"` + strings.Repeat("a", MaxResultBytes-2) + `"`
	aboveTheCap := `"` + strings.Repeat("a", MaxResultBytes-1) + `"`
	cases := []struct {
		name  string
		query string
		args  []any
		want  bool
	}{
		{"a result without a digest", `UPDATE effect_intent SET result_json = '{}' WHERE id = ?`, []any{unrecorded.ID}, false},
		{"a digest without a result", `UPDATE effect_intent SET result_digest = 'x' WHERE id = ?`, []any{unrecorded.ID}, false},
		{"a result on a started intent", `UPDATE effect_intent SET result_json = '{}', result_digest = 'x' WHERE id = ?`, []any{started.ID}, false},
		{"a result that is not JSON", `UPDATE effect_intent SET result_json = 'nope', result_digest = 'x' WHERE id = ?`, []any{unrecorded.ID}, false},
		{"a BLOB result", `UPDATE effect_intent SET result_json = CAST('{}' AS BLOB), result_digest = 'x' WHERE id = ?`, []any{unrecorded.ID}, false},
		{"a record of 1048577 bytes", `UPDATE effect_intent SET result_json = ?, result_digest = 'x' WHERE id = ?`, []any{aboveTheCap, unrecorded.ID}, false},
		{"a state change of a recorded intent", `UPDATE effect_intent SET state = 'failed' WHERE id = ?`, []any{recorded.ID}, false},
		{"a record of exactly 1048576 bytes", `UPDATE effect_intent SET result_json = ?, result_digest = 'x' WHERE id = ?`, []any{atTheCap, unrecorded.ID}, true},
	}
	for _, tc := range cases {
		_, err := db.ExecContext(context.Background(), tc.query, tc.args...)
		if tc.want {
			if err != nil {
				t.Fatalf("UPDATE %s: err = %v, want it accepted", tc.name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), "CHECK constraint failed") {
			t.Fatalf("UPDATE %s: err = %v, want a CHECK constraint failure", tc.name, err)
		}
	}
}
