package sqlite

import (
	"bytes"
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/turn"
	runtimeengine "github.com/anggasct/aura/internal/platform/turn/engine"
	runtimesessions "github.com/anggasct/aura/internal/platform/turn/sessions"
	"github.com/anggasct/aura/internal/store"
)

var _ runtimeengine.SessionStore = (*SessionStore)(nil)

var sessionClockStart = time.Date(2030, 4, 5, 6, 7, 8, 123456789, time.UTC)

const (
	testSessionID     = "session-1"
	testSessionOwner  = "telegram:1"
	testOrigin        = "telegram"
	defaultMaxPending = 8
)

type sessionFixture struct {
	path     string
	db       *sql.DB
	clock    *durable.ManualClock
	sessions *SessionStore
}

func newSessionFixture(t *testing.T) *sessionFixture {
	t.Helper()
	path := newDatabaseFile(t)
	db := openDB(t, path)
	createSession(t, db, testSessionID)
	clock := durable.NewManualClock(sessionClockStart)
	return &sessionFixture{path: path, db: db, clock: clock, sessions: newSessionStore(t, db, clock)}
}

func newSessionStore(t *testing.T, db *sql.DB, clock durable.Clock) *SessionStore {
	t.Helper()
	sessions, err := NewSessionStore(&SessionStoreOptions{DB: db, Clock: clock})
	if err != nil {
		t.Fatalf("NewSessionStore: %v", err)
	}
	return sessions
}

func createSession(t *testing.T, db *sql.DB, id string) {
	t.Helper()
	if err := store.NewSessionService(db).Create(t.Context(), &store.Session{ID: id, OwnerID: testSessionOwner}); err != nil {
		t.Fatalf("create session %s: %v", id, err)
	}
}

type turnSpec struct {
	sessionID  string
	turnID     string
	key        string
	eventID    string
	deadline   time.Time
	maxPending int
}

func admitRequest(spec turnSpec) *runtimesessions.AdmitRequest {
	return &runtimesessions.AdmitRequest{
		Turn: runtimesessions.Descriptor{
			TurnID:         spec.turnID,
			SessionID:      cmp.Or(spec.sessionID, testSessionID),
			PrincipalID:    testSessionOwner,
			Origin:         testOrigin,
			Parts:          []runtimesessions.Part{{Text: "hello from " + spec.turnID}},
			IdempotencyKey: spec.key,
			Deadline:       spec.deadline,
			AgentID:        "main",
		},
		AcceptedEventID: cmp.Or(spec.eventID, "event-"+spec.turnID),
		MaxPending:      cmp.Or(spec.maxPending, defaultMaxPending),
	}
}

func (f *sessionFixture) admit(t *testing.T, spec turnSpec) runtimesessions.AdmitResult {
	t.Helper()
	result, err := f.sessions.Admit(t.Context(), admitRequest(spec))
	if err != nil {
		t.Fatalf("Admit(%s): %v", spec.turnID, err)
	}
	return result
}

func (f *sessionFixture) release(t *testing.T, turnID string) runtimesessions.ReleaseResult {
	t.Helper()
	result, err := f.sessions.Release(t.Context(), testSessionID, turnID)
	if err != nil {
		t.Fatalf("Release(%s): %v", turnID, err)
	}
	return result
}

func (f *sessionFixture) status(t *testing.T) runtimesessions.StatusResult {
	t.Helper()
	result, err := f.sessions.Status(t.Context(), testSessionID)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	return result
}

func queuedRef(q *runtimesessions.QueuedTurn) string {
	if q == nil {
		return ""
	}
	return fmt.Sprintf("%s@%d", q.Descriptor.TurnID, q.Sequence)
}

func queryStrings(t *testing.T, db *sql.DB, query string, args ...any) []string {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), query, args...)
	if err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			t.Fatalf("scan %q: %v", query, err)
		}
		out = append(out, value)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return out
}

func snapshotAdmission(t *testing.T, db *sql.DB) []string {
	t.Helper()
	events := queryStrings(t, db, `SELECT 'event|' || id || '|' || session_id || '|' || sequence || '|' || turn_id || '|' || kind || '|' || payload_json || '|' || created_at
FROM runtime_event ORDER BY session_id, sequence`)
	claims := queryStrings(t, db, `SELECT 'claim|' || source || '|' || external_id || '|' || turn_id || '|' || accepted_at || '|' || expires_at
FROM ingress_dedupe ORDER BY source, external_id`)
	turns := queryStrings(t, db, `SELECT 'turn|' || turn_id || '|' || session_id || '|' || sequence || '|' || state || '|' || descriptor_json || '|' || COALESCE(deadline, '') || '|' || created_at || '|' || updated_at
FROM durable_session_turn ORDER BY session_id, sequence`)
	return slices.Concat(events, claims, turns)
}

func requireUnchanged(t *testing.T, db *sql.DB, before []string) {
	t.Helper()
	if after := snapshotAdmission(t, db); !slices.Equal(after, before) {
		t.Fatalf("stored rows changed:\nbefore %q\nafter  %q", before, after)
	}
}

func turnStates(t *testing.T, db *sql.DB, sessionID string) []string {
	t.Helper()
	return queryStrings(t, db, `SELECT turn_id || ':' || state FROM durable_session_turn WHERE session_id = ? ORDER BY sequence`, sessionID)
}

type heldRow struct {
	sessionID  string
	turnID     string
	sequence   uint64
	state      string
	deadline   string
	descriptor string
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func insertTurnRow(t *testing.T, x execer, row heldRow) error {
	t.Helper()
	descriptor := row.descriptor
	if descriptor == "" {
		raw, err := json.Marshal(runtimesessions.Descriptor{TurnID: row.turnID, SessionID: row.sessionID, PrincipalID: testSessionOwner, Origin: testOrigin})
		if err != nil {
			t.Fatalf("encode descriptor: %v", err)
		}
		descriptor = string(raw)
	}
	var deadline any
	if row.deadline != "" {
		deadline = row.deadline
	}
	stamp := sessionClockStart.Format(time.RFC3339Nano)
	_, err := x.ExecContext(t.Context(), `INSERT INTO durable_session_turn (turn_id, session_id, sequence, state, descriptor_json, deadline, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, row.turnID, row.sessionID, row.sequence, row.state, descriptor, deadline, stamp, stamp)
	return err
}

func insertHeldTurn(t *testing.T, db *sql.DB, row heldRow) error {
	t.Helper()
	tx, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(t.Context(), `INSERT INTO runtime_event (id, session_id, sequence, turn_id, invocation_id, author, kind, schema_version, payload_json, created_at)
VALUES (?, ?, ?, ?, '', ?, ?, 1, '{}', ?)`, "event-"+row.turnID, row.sessionID, row.sequence, row.turnID, testSessionOwner, turn.EventKindTurnAccepted, sessionClockStart.Format(time.RFC3339Nano)); err != nil {
		return err
	}
	if err := insertTurnRow(t, tx, row); err != nil {
		return err
	}
	return tx.Commit()
}

func mustInsertHeldTurn(t *testing.T, db *sql.DB, row heldRow) {
	t.Helper()
	if err := insertHeldTurn(t, db, row); err != nil {
		t.Fatalf("insert held turn %s: %v", row.turnID, err)
	}
}

func TestNewSessionStoreRejectsMissingOptions(t *testing.T) {
	db := openDB(t, newDatabaseFile(t))
	cases := []struct {
		name string
		opts *SessionStoreOptions
	}{
		{"nil options", nil},
		{"nil database", &SessionStoreOptions{Clock: durable.NewManualClock(sessionClockStart)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sessions, err := NewSessionStore(tc.opts)
			if sessions != nil {
				t.Fatal("NewSessionStore returned a store for invalid options")
			}
			requireCode(t, err, ErrorCodeInvalidArgument)
		})
	}
	sessions, err := NewSessionStore(&SessionStoreOptions{DB: db})
	if err != nil {
		t.Fatalf("NewSessionStore without a clock: %v", err)
	}
	if sessions.clock == nil {
		t.Fatal("a nil clock was not replaced by the real clock")
	}
}

func TestSessionStoreValidatesIds(t *testing.T) {
	f := newSessionFixture(t)
	f.admit(t, turnSpec{turnID: "turn-1", key: "key-1"})
	f.admit(t, turnSpec{turnID: "turn-2"})
	before := snapshotAdmission(t, f.db)
	cases := []struct {
		name string
		call func() error
	}{
		{"release without a session id", func() error {
			_, err := f.sessions.Release(t.Context(), "", "turn-1")
			return err
		}},
		{"release without a turn id", func() error {
			_, err := f.sessions.Release(t.Context(), testSessionID, "")
			return err
		}},
		{"status without a session id", func() error {
			_, err := f.sessions.Status(t.Context(), "")
			return err
		}},
		{"abort without a session id", func() error {
			_, err := f.sessions.Abort(t.Context(), "")
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if code, ok := CodeOf(tc.call()); !ok || code != ErrorCodeInvalidArgument {
				t.Fatalf("code = %q, %v; want %s", code, ok, ErrorCodeInvalidArgument)
			}
			requireUnchanged(t, f.db, before)
		})
	}
}

func TestAdmitGrantsTheFirstTurnAndQueuesTheRest(t *testing.T) {
	f := newSessionFixture(t)
	persona := &turn.PersonaFingerprint{AgentID: "main", SoulSHA256: "soul", ToolsSHA256: "tools", ModelRoute: "default"}
	req := admitRequest(turnSpec{turnID: "turn-1", key: "key-1"})
	req.Turn.Persona = persona
	first, err := f.sessions.Admit(t.Context(), req)
	if err != nil {
		t.Fatalf("first Admit: %v", err)
	}
	if first.ToStart == nil || first.ToStart.Descriptor.TurnID != "turn-1" || first.ToStart.Sequence != first.Sequence || first.Sequence == 0 {
		t.Fatalf("first admit = %+v, want turn-1 granted with a positive sequence", first)
	}
	if first.Replayed || first.Overloaded {
		t.Fatalf("first admit = %+v, want neither replayed nor overloaded", first)
	}
	wantPayload, err := turn.AcceptedPayload(testOrigin, testSessionOwner, "main", persona)
	if err != nil {
		t.Fatalf("AcceptedPayload: %v", err)
	}
	if !bytes.Equal(first.EventPayload, wantPayload) {
		t.Fatalf("event payload = %s, want %s", first.EventPayload, wantPayload)
	}
	stamp := sessionClockStart.Format(time.RFC3339Nano)
	if first.EventCreatedAt != stamp {
		t.Fatalf("event created at = %q, want the store clock %q", first.EventCreatedAt, stamp)
	}
	var payload, eventCreatedAt, kind, author string
	var sequence uint64
	if err := f.db.QueryRowContext(t.Context(), `SELECT payload_json, created_at, kind, author, sequence FROM runtime_event WHERE id = 'event-turn-1'`).
		Scan(&payload, &eventCreatedAt, &kind, &author, &sequence); err != nil {
		t.Fatalf("read accepted event: %v", err)
	}
	if payload != string(wantPayload) || eventCreatedAt != stamp || kind != turn.EventKindTurnAccepted || author != testSessionOwner || sequence != first.Sequence {
		t.Fatalf("stored event = %s %s %s %s %d", payload, eventCreatedAt, kind, author, sequence)
	}
	var rowCreatedAt, rowUpdatedAt string
	if err := f.db.QueryRowContext(t.Context(), `SELECT created_at, updated_at FROM durable_session_turn WHERE turn_id = 'turn-1'`).Scan(&rowCreatedAt, &rowUpdatedAt); err != nil {
		t.Fatalf("read turn row: %v", err)
	}
	if rowCreatedAt != stamp || rowUpdatedAt != stamp {
		t.Fatalf("turn row stamps = %q %q, want %q", rowCreatedAt, rowUpdatedAt, stamp)
	}
	second := f.admit(t, turnSpec{turnID: "turn-2"})
	third := f.admit(t, turnSpec{turnID: "turn-3", key: "key-3"})
	if second.ToStart != nil || third.ToStart != nil {
		t.Fatalf("later admits granted %q and %q, want both queued", queuedRef(second.ToStart), queuedRef(third.ToStart))
	}
	if second.Sequence <= first.Sequence || third.Sequence <= second.Sequence {
		t.Fatalf("sequences = %d %d %d, want increasing", first.Sequence, second.Sequence, third.Sequence)
	}
	status := f.status(t)
	if status.ActiveTurnID != "turn-1" || status.QueueDepth != 2 {
		t.Fatalf("status = %+v, want turn-1 active and a depth of 2", status)
	}
	if got := turnStates(t, f.db, testSessionID); !slices.Equal(got, []string{"turn-1:active", "turn-2:queued", "turn-3:queued"}) {
		t.Fatalf("turn rows = %v", got)
	}
}

func TestAdmitIsOneTransaction(t *testing.T) {
	t.Run("a failed queue entry rolls back the event and the claim", func(t *testing.T) {
		f := newSessionFixture(t)
		f.admit(t, turnSpec{turnID: "turn-1", key: "key-1"})
		execSQL(t, f.db, `CREATE TRIGGER fail_queue BEFORE INSERT ON durable_session_turn WHEN NEW.turn_id = 'turn-fail' BEGIN SELECT RAISE(ABORT, 'injected'); END`)
		before := snapshotAdmission(t, f.db)
		if _, err := f.sessions.Admit(t.Context(), admitRequest(turnSpec{turnID: "turn-fail", key: "key-fail"})); err == nil {
			t.Fatal("admit through the failing trigger succeeded")
		}
		requireUnchanged(t, f.db, before)
		later := f.admit(t, turnSpec{turnID: "turn-2", key: "key-fail"})
		if later.Replayed || later.Sequence == 0 {
			t.Fatalf("admit with the rolled back key = %+v, want a new accepted turn", later)
		}
	})
	t.Run("a held turn id is rejected without writing", func(t *testing.T) {
		f := newSessionFixture(t)
		createSession(t, f.db, "session-2")
		f.admit(t, turnSpec{turnID: "turn-1", key: "key-1"})
		f.admit(t, turnSpec{turnID: "turn-2"})
		before := snapshotAdmission(t, f.db)
		for _, spec := range []turnSpec{
			{turnID: "turn-1", key: "key-new", eventID: "event-turn-1-again"},
			{turnID: "turn-2", key: "key-other", eventID: "event-turn-2-elsewhere", sessionID: "session-2"},
		} {
			_, err := f.sessions.Admit(t.Context(), admitRequest(spec))
			requireCode(t, err, ErrorCodeInvalidArgument)
			requireUnchanged(t, f.db, before)
		}
	})
}

func TestAdmitReplaysAClaimedIdempotencyKey(t *testing.T) {
	f := newSessionFixture(t)
	first := f.admit(t, turnSpec{turnID: "turn-1", key: "key-1"})
	if first.ToStart == nil {
		t.Fatalf("first admit = %+v, want granted", first)
	}
	before := snapshotAdmission(t, f.db)
	for _, spec := range []turnSpec{
		{turnID: "turn-2", key: "key-1"},
		{turnID: "turn-1", key: "key-1", eventID: "event-turn-1-again"},
	} {
		got := f.admit(t, spec)
		if !got.Replayed || got.OriginalTurnID != "turn-1" || got.ToStart != nil || got.Sequence != 0 {
			t.Fatalf("admit %s with a claimed key = %+v, want a replay of turn-1", spec.turnID, got)
		}
		requireUnchanged(t, f.db, before)
	}
	f.release(t, "turn-1")
	released := snapshotAdmission(t, f.db)
	if got := f.admit(t, turnSpec{turnID: "turn-3", key: "key-1"}); !got.Replayed || got.OriginalTurnID != "turn-1" {
		t.Fatalf("admit after the release = %+v, want a replay of turn-1", got)
	}
	requireUnchanged(t, f.db, released)
	f.clock.Advance(idempotencyWindow + time.Millisecond)
	got := f.admit(t, turnSpec{turnID: "turn-4", key: "key-1"})
	if got.Replayed || queuedRef(got.ToStart) != fmt.Sprintf("turn-4@%d", got.Sequence) {
		t.Fatalf("admit after the window = %+v, want turn-4 granted", got)
	}
	if claims := queryStrings(t, f.db, `SELECT turn_id FROM ingress_dedupe WHERE source = ? AND external_id = 'key-1'`, testOrigin); !slices.Equal(claims, []string{"turn-4"}) {
		t.Fatalf("claims of key-1 = %v, want turn-4", claims)
	}
}

func TestAdmitRejectsAFullSession(t *testing.T) {
	f := newSessionFixture(t)
	f.admit(t, turnSpec{turnID: "turn-1", key: "key-1", maxPending: 2})
	f.admit(t, turnSpec{turnID: "turn-2", key: "key-2", maxPending: 2})
	before := snapshotAdmission(t, f.db)
	full := f.admit(t, turnSpec{turnID: "turn-3", key: "key-3", maxPending: 2})
	if !full.Overloaded || full.Replayed || full.ToStart != nil || full.Sequence != 0 {
		t.Fatalf("admit to a full session = %+v, want overloaded", full)
	}
	requireUnchanged(t, f.db, before)
	replayed := f.admit(t, turnSpec{turnID: "turn-4", key: "key-1", maxPending: 2})
	if !replayed.Replayed || replayed.Overloaded || replayed.OriginalTurnID != "turn-1" {
		t.Fatalf("admit with a claimed key to a full session = %+v, want a replay of turn-1", replayed)
	}
	requireUnchanged(t, f.db, before)
	if next := f.release(t, "turn-1"); queuedRef(next.ToStart) == "" || next.ToStart.Descriptor.TurnID != "turn-2" {
		t.Fatalf("release = %+v, want turn-2", next)
	}
	accepted := f.admit(t, turnSpec{turnID: "turn-5", key: "key-3", maxPending: 2})
	if accepted.Replayed || accepted.Overloaded || accepted.Sequence == 0 || accepted.ToStart != nil {
		t.Fatalf("admit with the rejected key after a release = %+v, want queued and not a replay", accepted)
	}
	if got := turnStates(t, f.db, testSessionID); !slices.Equal(got, []string{"turn-2:active", "turn-5:queued"}) {
		t.Fatalf("turn rows = %v", got)
	}
}

func TestAdmitValidatesItsRequest(t *testing.T) {
	f := newSessionFixture(t)
	f.admit(t, turnSpec{turnID: "turn-0", key: "key-0"})
	before := snapshotAdmission(t, f.db)
	cases := []struct {
		name     string
		mutate   func(req *runtimesessions.AdmitRequest) *runtimesessions.AdmitRequest
		code     ErrorCode
		turnCode turn.ErrorCode
	}{
		{name: "nil request", mutate: func(*runtimesessions.AdmitRequest) *runtimesessions.AdmitRequest { return nil }, code: ErrorCodeInvalidArgument},
		{name: "empty turn id", mutate: func(req *runtimesessions.AdmitRequest) *runtimesessions.AdmitRequest {
			req.Turn.TurnID = ""
			return req
		}, code: ErrorCodeInvalidArgument},
		{name: "empty accepted event id", mutate: func(req *runtimesessions.AdmitRequest) *runtimesessions.AdmitRequest {
			req.AcceptedEventID = ""
			return req
		}, code: ErrorCodeInvalidArgument},
		{name: "zero max pending", mutate: func(req *runtimesessions.AdmitRequest) *runtimesessions.AdmitRequest {
			req.MaxPending = 0
			return req
		}, code: ErrorCodeInvalidArgument},
		{name: "negative max pending", mutate: func(req *runtimesessions.AdmitRequest) *runtimesessions.AdmitRequest {
			req.MaxPending = -1
			return req
		}, code: ErrorCodeInvalidArgument},
		{name: "empty session id", mutate: func(req *runtimesessions.AdmitRequest) *runtimesessions.AdmitRequest {
			req.Turn.SessionID = ""
			return req
		}, turnCode: turn.ErrorCodeInvalidArgument},
		{name: "empty origin", mutate: func(req *runtimesessions.AdmitRequest) *runtimesessions.AdmitRequest {
			req.Turn.Origin = ""
			return req
		}, turnCode: turn.ErrorCodeInvalidArgument},
		{name: "image reference that is not json", mutate: func(req *runtimesessions.AdmitRequest) *runtimesessions.AdmitRequest {
			req.Turn.Parts = []runtimesessions.Part{{Image: json.RawMessage(`{not json`)}}
			return req
		}, turnCode: turn.ErrorCodeInvalidArgument},
		{name: "incomplete skill pin", mutate: func(req *runtimesessions.AdmitRequest) *runtimesessions.AdmitRequest {
			req.Turn.Skill = &turn.SkillPin{SkillID: "skill"}
			return req
		}, turnCode: turn.ErrorCodeInvalidArgument},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.sessions.Admit(t.Context(), tc.mutate(admitRequest(turnSpec{turnID: "turn-1", key: "key-1"})))
			if tc.code != "" {
				if code, ok := CodeOf(err); !ok || code != tc.code {
					t.Fatalf("err = %v, want code %s", err, tc.code)
				}
			}
			if tc.turnCode != "" {
				if code, ok := turn.CodeOf(err); !ok || code != tc.turnCode {
					t.Fatalf("err = %v, want turn code %s", err, tc.turnCode)
				}
			}
			requireUnchanged(t, f.db, before)
		})
	}
	t.Run("unknown session", func(t *testing.T) {
		_, err := f.sessions.Admit(t.Context(), admitRequest(turnSpec{turnID: "turn-1", key: "key-1", sessionID: "session-missing"}))
		if code, ok := store.CodeOf(err); !ok || code != store.ErrorCodeSessionNotFound {
			t.Fatalf("err = %v, want store code %s", err, store.ErrorCodeSessionNotFound)
		}
		requireUnchanged(t, f.db, before)
	})
}

func TestAdmitRefusesAQueueWithoutAnActiveTurn(t *testing.T) {
	f := newSessionFixture(t)
	mustInsertHeldTurn(t, f.db, heldRow{sessionID: testSessionID, turnID: "turn-stranded", sequence: 1, state: turnStateQueued})
	before := snapshotAdmission(t, f.db)
	_, err := f.sessions.Admit(t.Context(), admitRequest(turnSpec{turnID: "turn-2", key: "key-2"}))
	requireCode(t, err, ErrorCodeSessionStateInvalid)
	requireUnchanged(t, f.db, before)
}

func TestReleaseHandsTheNextTurnInOrder(t *testing.T) {
	f := newSessionFixture(t)
	first := f.admit(t, turnSpec{turnID: "turn-1"})
	second := f.admit(t, turnSpec{turnID: "turn-2"})
	third := f.admit(t, turnSpec{turnID: "turn-3"})
	steps := []struct {
		release string
		toStart string
		depth   int
	}{
		{"turn-1", fmt.Sprintf("turn-2@%d", second.Sequence), 1},
		{"turn-2", fmt.Sprintf("turn-3@%d", third.Sequence), 0},
		{"turn-3", "", 0},
	}
	if first.ToStart == nil {
		t.Fatalf("first admit = %+v, want granted", first)
	}
	for _, step := range steps {
		got := f.release(t, step.release)
		if queuedRef(got.ToStart) != step.toStart || got.QueueDepth != step.depth {
			t.Fatalf("release %s = %q depth %d, want %q depth %d", step.release, queuedRef(got.ToStart), got.QueueDepth, step.toStart, step.depth)
		}
		if got.ToStart != nil && got.ToStart.Descriptor.Parts[0].Text != "hello from "+got.ToStart.Descriptor.TurnID {
			t.Fatalf("granted descriptor = %+v, want the admitted one", got.ToStart.Descriptor)
		}
	}
	if rows := turnStates(t, f.db, testSessionID); len(rows) != 0 {
		t.Fatalf("turn rows after the last release = %v, want none", rows)
	}
}

func TestReleaseIsIdempotent(t *testing.T) {
	f := newSessionFixture(t)
	createSession(t, f.db, "session-2")
	f.admit(t, turnSpec{turnID: "turn-1"})
	f.admit(t, turnSpec{turnID: "turn-2"})
	f.admit(t, turnSpec{turnID: "turn-3"})
	f.release(t, "turn-1")
	before := snapshotAdmission(t, f.db)
	for _, turnID := range []string{"turn-1", "turn-unknown"} {
		got := f.release(t, turnID)
		if got.ToStart != nil || got.QueueDepth != 1 {
			t.Fatalf("release of %s that is not held = %+v, want nothing to start and a depth of 1", turnID, got)
		}
		requireUnchanged(t, f.db, before)
	}
	refused := []struct {
		name      string
		sessionID string
		turnID    string
	}{
		{"queued turn", testSessionID, "turn-3"},
		{"held turn under another session", "session-2", "turn-2"},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.sessions.Release(t.Context(), tc.sessionID, tc.turnID)
			requireCode(t, err, ErrorCodeInvalidArgument)
			requireUnchanged(t, f.db, before)
		})
	}
}

func TestStatusReportsActiveQueueAndDeadline(t *testing.T) {
	f := newSessionFixture(t)
	createSession(t, f.db, "session-2")
	deadline := time.Date(2030, 4, 5, 14, 0, 0, 500, time.FixedZone("plus7", 7*60*60))
	first := f.admit(t, turnSpec{turnID: "turn-1", deadline: deadline})
	f.admit(t, turnSpec{turnID: "turn-2"})
	third := f.admit(t, turnSpec{turnID: "turn-3", deadline: deadline.Add(time.Hour)})
	status := f.status(t)
	if status.ActiveTurnID != "turn-1" || status.QueueDepth != 2 || status.LastSequence != third.Sequence {
		t.Fatalf("status = %+v, want turn-1 active, a depth of 2 and last sequence %d", status, third.Sequence)
	}
	if status.Active == nil || status.Active.Sequence != first.Sequence || status.Active.Descriptor.TurnID != "turn-1" || !status.Active.Descriptor.Deadline.Equal(deadline) {
		t.Fatalf("active = %+v, want turn-1 at sequence %d", status.Active, first.Sequence)
	}
	if want := deadline.UTC().Format(time.RFC3339Nano); status.Deadline != want {
		t.Fatalf("deadline = %q, want %q", status.Deadline, want)
	}
	empty, err := f.sessions.Status(t.Context(), "session-2")
	if err != nil || empty.QueueDepth != 0 || empty.ActiveTurnID != "" || empty.Active != nil || empty.LastSequence != 0 || empty.Deadline != "" {
		t.Fatalf("status of a session with no turns = %+v, %v; want the zero result", empty, err)
	}
	execSQL(t, f.db, `UPDATE durable_session_turn SET descriptor_json = 'not json' WHERE turn_id = 'turn-1'`)
	if _, err := f.sessions.Status(t.Context(), testSessionID); err == nil {
		t.Fatal("status with an undecodable active descriptor succeeded")
	}
}

func TestAbortDropsEveryTurn(t *testing.T) {
	f := newSessionFixture(t)
	var want []string
	for _, turnID := range []string{"turn-1", "turn-2", "turn-3"} {
		admitted := f.admit(t, turnSpec{turnID: turnID})
		want = append(want, fmt.Sprintf("%s@%d", turnID, admitted.Sequence))
	}
	aborted, err := f.sessions.Abort(t.Context(), testSessionID)
	if err != nil {
		t.Fatalf("Abort: %v", err)
	}
	var dropped []string
	for i := range aborted.Dropped {
		dropped = append(dropped, queuedRef(&aborted.Dropped[i]))
	}
	if !slices.Equal(dropped, want) {
		t.Fatalf("dropped = %v, want %v", dropped, want)
	}
	if rows := turnStates(t, f.db, testSessionID); len(rows) != 0 {
		t.Fatalf("turn rows after abort = %v, want none", rows)
	}
	if n := countRows(t, f.db, `SELECT COUNT(*) FROM runtime_event WHERE session_id = ? AND kind = ?`, testSessionID, turn.EventKindTurnAccepted); n != 3 {
		t.Fatalf("accepted events after abort = %d, want 3", n)
	}
	if next := f.admit(t, turnSpec{turnID: "turn-4"}); queuedRef(next.ToStart) != fmt.Sprintf("turn-4@%d", next.Sequence) {
		t.Fatalf("admit after abort = %+v, want turn-4 granted", next)
	}
}

func TestSessionTurnsSurviveRestart(t *testing.T) {
	f := newSessionFixture(t)
	if first := f.admit(t, turnSpec{turnID: "turn-1", key: "key-1"}); first.ToStart == nil {
		t.Fatalf("first admit = %+v, want turn-1 granted", first)
	}
	for _, spec := range []turnSpec{{turnID: "turn-2", key: "key-2"}, {turnID: "turn-3", key: "key-3"}} {
		if got := f.admit(t, spec); got.Replayed || got.ToStart != nil {
			t.Fatalf("admit %s = %+v, want queued", spec.turnID, got)
		}
	}
	if dup := f.admit(t, turnSpec{turnID: "turn-1-again", key: "key-1"}); !dup.Replayed || dup.OriginalTurnID != "turn-1" {
		t.Fatalf("duplicate key = %+v, want a replay of turn-1", dup)
	}
	if next := f.release(t, "turn-1"); queuedRef(next.ToStart) == "" || next.ToStart.Descriptor.TurnID != "turn-2" {
		t.Fatalf("release = %+v, want turn-2", next)
	}
	if err := f.db.Close(); err != nil {
		t.Fatalf("close database: %v", err)
	}
	restarted := &sessionFixture{path: f.path, db: openDB(t, f.path), clock: f.clock}
	restarted.sessions = newSessionStore(t, restarted.db, restarted.clock)
	if status := restarted.status(t); status.ActiveTurnID != "turn-2" || status.QueueDepth != 1 {
		t.Fatalf("status after restart = %+v, want turn-2 active with a depth of 1", status)
	}
	if next := restarted.release(t, "turn-2"); queuedRef(next.ToStart) == "" || next.ToStart.Descriptor.TurnID != "turn-3" {
		t.Fatalf("release after restart = %+v, want turn-3", next)
	}
	if next := restarted.release(t, "turn-3"); next.ToStart != nil {
		t.Fatalf("final release = %+v, want nothing", next)
	}
	if dup := restarted.admit(t, turnSpec{turnID: "turn-9", key: "key-1"}); !dup.Replayed || dup.OriginalTurnID != "turn-1" {
		t.Fatalf("duplicate key after restart = %+v, want a replay of turn-1", dup)
	}
	sequences := queryStrings(t, restarted.db, `SELECT CAST(sequence AS TEXT) FROM runtime_event WHERE session_id = ? AND kind = ? ORDER BY rowid`, testSessionID, turn.EventKindTurnAccepted)
	if len(sequences) != 3 {
		t.Fatalf("accepted events = %v, want exactly 3", sequences)
	}
	last := 0
	for _, raw := range sequences {
		sequence, err := strconv.Atoi(raw)
		if err != nil || sequence <= last {
			t.Fatalf("accepted sequences = %v, want increasing", sequences)
		}
		last = sequence
	}
}

type admitOutcome struct {
	result runtimesessions.AdmitResult
	err    error
}

func TestConcurrentAdmitsKeepOneActiveTurn(t *testing.T) {
	f := newSessionFixture(t)
	other := newSessionStore(t, openDB(t, f.path), f.clock)
	const admits = 16
	outcomes := make(chan admitOutcome, admits)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range admits {
		sessions := f.sessions
		if i%2 == 1 {
			sessions = other
		}
		req := admitRequest(turnSpec{turnID: fmt.Sprintf("turn-%02d", i), maxPending: 32})
		wg.Go(func() {
			<-start
			result, err := sessions.Admit(t.Context(), req)
			outcomes <- admitOutcome{result: result, err: err}
		})
	}
	close(start)
	wg.Wait()
	close(outcomes)
	granted := 0
	var sequences []string
	for outcome := range outcomes {
		if outcome.err != nil {
			t.Fatalf("concurrent admit: %v", outcome.err)
		}
		if outcome.result.ToStart != nil {
			granted++
		}
		sequences = append(sequences, strconv.FormatUint(outcome.result.Sequence, 10))
	}
	if granted != 1 {
		t.Fatalf("granted turns = %d, want exactly 1", granted)
	}
	slices.Sort(sequences)
	if len(slices.Compact(slices.Clone(sequences))) != admits {
		t.Fatalf("sequences = %v, want %d distinct values", sequences, admits)
	}
	stored := queryStrings(t, f.db, `SELECT CAST(sequence AS TEXT) FROM runtime_event WHERE session_id = ? AND kind = ? ORDER BY CAST(sequence AS TEXT)`, testSessionID, turn.EventKindTurnAccepted)
	if !slices.Equal(stored, sequences) {
		t.Fatalf("stored accepted sequences = %v, want %v", stored, sequences)
	}
	if n := countRows(t, f.db, `SELECT COUNT(*) FROM durable_session_turn WHERE session_id = ? AND state = 'active'`, testSessionID); n != 1 {
		t.Fatalf("active rows = %d, want 1", n)
	}
}

type scriptStep struct {
	op     string
	turnID string
	key    string
}

type machineAdmission struct {
	req      *runtimesessions.AdmitRequest
	sequence uint64
}

func machineAdmit(t *testing.T, state *runtimesessions.State, in machineAdmission) runtimesessions.AdmitResult {
	t.Helper()
	original, err := runtimesessions.CheckReplay(state, in.req.Turn.IdempotencyKey)
	if err != nil {
		t.Fatalf("CheckReplay: %v", err)
	}
	if original != "" {
		return runtimesessions.AdmitResult{Replayed: true, OriginalTurnID: original}
	}
	sequence := in.sequence
	if sequence == 0 {
		sequence = state.LastSequence + 1
	}
	result, err := runtimesessions.Enqueue(state, &in.req.Turn, sequence, in.req.MaxPending)
	if code, ok := turn.CodeOf(err); ok && code == turn.ErrorCodeRuntimeOverloaded {
		return runtimesessions.AdmitResult{Overloaded: true}
	}
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	return result
}

func TestSessionStoreMatchesTheStateMachine(t *testing.T) {
	f := newSessionFixture(t)
	state := runtimesessions.NewState()
	script := []scriptStep{
		{"admit", "turn-1", "key-1"},
		{"admit", "turn-2", ""},
		{"admit", "turn-3", "key-1"},
		{"admit", "turn-4", "key-4"},
		{"admit", "turn-5", ""},
		{"admit", "turn-6", "key-6"},
		{"release", "turn-1", ""},
		{"admit", "turn-7", "key-6"},
		{"release", "turn-2", ""},
		{"admit", "turn-8", "key-4"},
		{"abort", "", ""},
		{"admit", "turn-9", ""},
		{"admit", "turn-10", "key-1"},
		{"release", "turn-9", ""},
		{"admit", "turn-11", "key-11"},
	}
	for i, step := range script {
		label := fmt.Sprintf("step %d %s %s", i, step.op, step.turnID)
		switch step.op {
		case "admit":
			req := admitRequest(turnSpec{turnID: step.turnID, key: step.key, maxPending: 3})
			got, err := f.sessions.Admit(t.Context(), req)
			if err != nil {
				t.Fatalf("%s: store: %v", label, err)
			}
			want := machineAdmit(t, state, machineAdmission{req: req, sequence: got.Sequence})
			if queuedRef(got.ToStart) != queuedRef(want.ToStart) || got.Overloaded != want.Overloaded || got.Replayed != want.Replayed || got.OriginalTurnID != want.OriginalTurnID {
				t.Fatalf("%s: store %+v, machine %+v", label, got, want)
			}
		case "release":
			got := f.release(t, step.turnID)
			want, err := runtimesessions.Release(state, step.turnID)
			if err != nil {
				t.Fatalf("%s: machine: %v", label, err)
			}
			if queuedRef(got.ToStart) != queuedRef(want.ToStart) || got.QueueDepth != want.QueueDepth {
				t.Fatalf("%s: store %q depth %d, machine %q depth %d", label, queuedRef(got.ToStart), got.QueueDepth, queuedRef(want.ToStart), want.QueueDepth)
			}
		case "abort":
			got, err := f.sessions.Abort(t.Context(), testSessionID)
			if err != nil {
				t.Fatalf("%s: store: %v", label, err)
			}
			want, err := runtimesessions.Abort(state)
			if err != nil {
				t.Fatalf("%s: machine: %v", label, err)
			}
			if len(got.Dropped) != len(want) {
				t.Fatalf("%s: store dropped %d, machine %d", label, len(got.Dropped), len(want))
			}
			for j := range want {
				if queuedRef(&got.Dropped[j]) != queuedRef(&want[j]) {
					t.Fatalf("%s: dropped %d store %q, machine %q", label, j, queuedRef(&got.Dropped[j]), queuedRef(&want[j]))
				}
			}
		}
		status := f.status(t)
		wantActive := ""
		if state.Active != nil {
			wantActive = state.Active.Descriptor.TurnID
		}
		if status.QueueDepth != len(state.Queue) || status.ActiveTurnID != wantActive {
			t.Fatalf("%s: store active %q depth %d, machine active %q depth %d", label, status.ActiveTurnID, status.QueueDepth, wantActive, len(state.Queue))
		}
	}
}

func TestErasingASessionDropsItsTurns(t *testing.T) {
	f := newSessionFixture(t)
	f.admit(t, turnSpec{turnID: "turn-1", key: "key-1"})
	f.admit(t, turnSpec{turnID: "turn-2"})
	counts, err := store.ErasePrincipal(t.Context(), f.db, testSessionOwner, true, store.ErasureOptions{})
	if err != nil {
		t.Fatalf("ErasePrincipal: %v", err)
	}
	if counts.Sessions != 1 {
		t.Fatalf("erased sessions = %d, want 1", counts.Sessions)
	}
	if n := countRows(t, f.db, `SELECT COUNT(*) FROM durable_session_turn`); n != 0 {
		t.Fatalf("turn rows after erasure = %d, want 0", n)
	}
	got := f.release(t, "turn-1")
	if got.ToStart != nil || got.QueueDepth != 0 {
		t.Fatalf("release after erasure = %+v, want nothing to start", got)
	}
}

func TestOneActiveTurnPerSessionIsEnforcedBySchema(t *testing.T) {
	f := newSessionFixture(t)
	first := f.admit(t, turnSpec{turnID: "turn-1"})
	if err := insertHeldTurn(t, f.db, heldRow{sessionID: testSessionID, turnID: "turn-2", sequence: first.Sequence + 1, state: turnStateActive}); err == nil {
		t.Fatal("a second active turn for one session was stored")
	}
	if got := turnStates(t, f.db, testSessionID); !slices.Equal(got, []string{"turn-1:active"}) {
		t.Fatalf("turn rows = %v, want only turn-1 active", got)
	}
	mustInsertHeldTurn(t, f.db, heldRow{sessionID: testSessionID, turnID: "turn-2", sequence: first.Sequence + 1, state: turnStateQueued})
}

func TestQueueEntryRequiresItsAcceptedEvent(t *testing.T) {
	f := newSessionFixture(t)
	if err := insertTurnRow(t, f.db, heldRow{sessionID: testSessionID, turnID: "turn-orphan", sequence: 7, state: turnStateActive}); err == nil {
		t.Fatal("a queue entry without its accepted event was stored")
	}
	if n := countRows(t, f.db, `SELECT COUNT(*) FROM durable_session_turn`); n != 0 {
		t.Fatalf("turn rows = %d, want 0", n)
	}
	mustInsertHeldTurn(t, f.db, heldRow{sessionID: testSessionID, turnID: "turn-orphan", sequence: 7, state: turnStateActive})
}

func activeTurnIDs(t *testing.T, sessions *SessionStore) []string {
	t.Helper()
	active, err := sessions.ActiveTurns(t.Context())
	if err != nil {
		t.Fatalf("ActiveTurns: %v", err)
	}
	ids := make([]string, 0, len(active))
	for i := range active {
		ids = append(ids, active[i].Descriptor.SessionID+"/"+queuedRef(&active[i]))
	}
	return ids
}

func TestActiveTurnsListsEachSessionsActiveTurn(t *testing.T) {
	f := newSessionFixture(t)
	if empty, err := f.sessions.ActiveTurns(t.Context()); err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("active turns of an empty store = %#v, %v; want an empty list", empty, err)
	}
	createSession(t, f.db, "session-b")
	createSession(t, f.db, "session-c")
	a1 := f.admit(t, turnSpec{turnID: "a1"})
	b1 := f.admit(t, turnSpec{turnID: "b1", sessionID: "session-b"})
	a2 := f.admit(t, turnSpec{turnID: "a2"})
	c1 := f.admit(t, turnSpec{turnID: "c1", sessionID: "session-c"})
	want := []string{
		fmt.Sprintf("%s/a1@%d", testSessionID, a1.Sequence),
		fmt.Sprintf("session-b/b1@%d", b1.Sequence),
		fmt.Sprintf("session-c/c1@%d", c1.Sequence),
	}
	if got := activeTurnIDs(t, f.sessions); !slices.Equal(got, want) {
		t.Fatalf("active turns = %v, want %v", got, want)
	}
	f.release(t, "a1")
	want = []string{
		fmt.Sprintf("session-b/b1@%d", b1.Sequence),
		fmt.Sprintf("%s/a2@%d", testSessionID, a2.Sequence),
		fmt.Sprintf("session-c/c1@%d", c1.Sequence),
	}
	if got := activeTurnIDs(t, f.sessions); !slices.Equal(got, want) {
		t.Fatalf("active turns after the release = %v, want %v", got, want)
	}
	execSQL(t, f.db, `UPDATE durable_session_turn SET descriptor_json = 'not json' WHERE turn_id = 'c1'`)
	if _, err := f.sessions.ActiveTurns(t.Context()); err == nil {
		t.Fatal("ActiveTurns decoded a descriptor that is not JSON")
	}
}

func TestAdmitRejectsAUsedTurnID(t *testing.T) {
	f := newSessionFixture(t)
	f.admit(t, turnSpec{turnID: "turn-1"})
	f.release(t, "turn-1")
	insertRun(t, f.db, runFixture{key: "turn-run", handler: "turn"})
	before := snapshotAdmission(t, f.db)
	for _, spec := range []turnSpec{
		{turnID: "turn-1", key: "key-released", eventID: "event-turn-1-again"},
		{turnID: "turn-run", key: "key-run"},
	} {
		t.Run(spec.turnID, func(t *testing.T) {
			_, err := f.sessions.Admit(t.Context(), admitRequest(spec))
			requireCode(t, err, ErrorCodeInvalidArgument)
			requireUnchanged(t, f.db, before)
		})
	}
	for _, key := range []string{"key-released", "key-run"} {
		got := f.admit(t, turnSpec{turnID: "turn-after-" + key, key: key})
		if got.Replayed || got.Sequence == 0 {
			t.Fatalf("admit with %s after the rejection = %+v, want a new accepted turn", key, got)
		}
		f.release(t, "turn-after-"+key)
	}
}
