package workflow

import (
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/sqlite/sqlitetest"
	turnstore "github.com/anggasct/aura/internal/platform/turn/store"
)

func replyTargetFixture(t *testing.T) (*Store, *Spec) {
	t.Helper()
	db := sqlitetest.OpenMigrated(t, turnstore.Schema(), Schema())
	store := mustNewStore(t, db)
	event := "done"
	spec := &Spec{ID: "book_visit", Goal: "Book a visit", Version: 1, Source: SourceDefined, Steps: []StepSpec{{ID: "hold", Executor: ExecutorSpec{Kind: KindWait, Event: &event}, Timeout: time.Minute}}}
	if err := store.SaveDefinition(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	seedMigrationSession(t, db, "ops/session", "local-owner")
	return store, spec
}

func TestStoreRunsOnTheBaselinePlusItsSteps(t *testing.T) {
	db := sqlitetest.OpenMigrated(t, turnstore.Schema(), Schema())
	var versions []int
	rows, err := db.QueryContext(t.Context(), `SELECT version FROM schema_migration WHERE owner = 'workflow' ORDER BY version`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var version int
		if err := rows.Scan(&version); err != nil {
			t.Fatal(err)
		}
		versions = append(versions, version)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(versions) != 3 || versions[1] != replyTargetStepVersion || versions[2] != personaPinsStepVersion {
		t.Fatalf("workflow ledger versions = %v, want its baseline then %d and %d", versions, replyTargetStepVersion, personaPinsStepVersion)
	}

	store, spec := replyTargetFixture(t)
	target := ReplyTarget{Channel: "telegram", Account: "pocket", ChannelID: "-1001234567890", ThreadID: "7"}
	origin := &RunOrigin{PrincipalID: "telegram:5", PrincipalClass: "public", AgentID: "lobby", SessionID: "lobby/telegram:pocket/chat:5", Reply: target}
	created, err := store.newRun(t.Context(), spec, &RunInput{}, origin)
	if err != nil {
		t.Fatal(err)
	}
	if run, err := store.Run(t.Context(), created.ID); err != nil || run.Origin.Reply != target {
		t.Fatalf("run after a round trip = %+v, %v", run, err)
	}
}

func TestRunAndProposalReplyTargetsRoundTripAllOrNone(t *testing.T) {
	store, spec := replyTargetFixture(t)
	valid := []struct {
		name   string
		target ReplyTarget
	}{
		{"discord channel", ReplyTarget{Channel: "discord", Account: "front", ChannelID: "333"}},
		{"telegram topic", ReplyTarget{Channel: "telegram", Account: "pocket", ChannelID: "-1001234567890", ThreadID: "7"}},
		{"no target", ReplyTarget{}},
	}
	for _, tc := range valid {
		t.Run(tc.name, func(t *testing.T) {
			origin := &RunOrigin{PrincipalID: "discord:111", PrincipalClass: "public", AgentID: "lobby", SessionID: "ops/session", Reply: tc.target}
			created, err := store.newRun(t.Context(), spec, &RunInput{}, origin)
			if err != nil {
				t.Fatalf("newRun: %v", err)
			}
			if run, err := store.Run(t.Context(), created.ID); err != nil || run.Origin.Reply != tc.target {
				t.Fatalf("run target = %+v, %v; want %+v", run, err, tc.target)
			}
			if pending, err := store.PendingCompletions(t.Context(), 10); err != nil || len(pending) != 0 {
				t.Fatalf("pending = %+v, %v", pending, err)
			}
			proposal := &Proposal{
				ID: NewProposalID(), SessionID: "ops/session", AgentID: "ops", PrincipalID: "local-owner", Spec: spec,
				PlanText: "plan", Digest: "digest", Reply: tc.target, CreatedAt: time.Now().UTC(),
			}
			if err := store.CreateProposal(t.Context(), proposal); err != nil {
				t.Fatalf("CreateProposal: %v", err)
			}
			if stored, err := store.Proposal(t.Context(), proposal.ID, time.Now()); err != nil || stored.Reply != tc.target {
				t.Fatalf("proposal target = %+v, %v; want %+v", stored, err, tc.target)
			}
		})
	}

	partial := []struct {
		name   string
		target ReplyTarget
	}{
		{"channel without account", ReplyTarget{Channel: "discord", ChannelID: "333"}},
		{"channel without channel id", ReplyTarget{Channel: "telegram", Account: "pocket"}},
		{"account without channel", ReplyTarget{Account: "pocket", ChannelID: "5"}},
		{"thread without channel", ReplyTarget{ThreadID: "7"}},
		{"unsupported channel", ReplyTarget{Channel: "email", Account: "mail", ChannelID: "guest"}},
	}
	for _, tc := range partial {
		t.Run("refuses "+tc.name, func(t *testing.T) {
			runsBefore, err := store.Runs(t.Context(), "")
			if err != nil {
				t.Fatal(err)
			}
			proposalsBefore, err := store.Proposals(t.Context(), "", time.Now())
			if err != nil {
				t.Fatal(err)
			}
			origin := &RunOrigin{PrincipalID: "discord:111", PrincipalClass: "public", AgentID: "lobby", SessionID: "ops/session", Reply: tc.target}
			if _, err := store.newRun(t.Context(), spec, &RunInput{}, origin); !fault.HasCode(err, ErrorCodeInvalidArgument) {
				t.Fatalf("newRun = %v, want %s", err, ErrorCodeInvalidArgument)
			}
			proposal := &Proposal{ID: NewProposalID(), SessionID: "ops/session", AgentID: "ops", PrincipalID: "local-owner", Spec: spec, PlanText: "plan", Digest: "digest", Reply: tc.target, CreatedAt: time.Now().UTC()}
			if err := store.CreateProposal(t.Context(), proposal); !fault.HasCode(err, ErrorCodeInvalidArgument) {
				t.Fatalf("CreateProposal = %v, want %s", err, ErrorCodeInvalidArgument)
			}
			runsAfter, err := store.Runs(t.Context(), "")
			if err != nil {
				t.Fatal(err)
			}
			proposalsAfter, err := store.Proposals(t.Context(), "", time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if len(runsAfter) != len(runsBefore) || len(proposalsAfter) != len(proposalsBefore) {
				t.Fatalf("a refused target wrote rows: runs %d to %d, proposals %d to %d", len(runsBefore), len(runsAfter), len(proposalsBefore), len(proposalsAfter))
			}
		})
	}
}
