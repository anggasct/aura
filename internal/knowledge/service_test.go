package knowledge

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeStore struct {
	writes    []SourceWrite
	sources   map[string][]SourceRecord
	report    IndexReport
	writeErr  error
	removed   []string
	removeErr error
	applied   bool
}

func (s *fakeStore) ReplaceSource(_ context.Context, write *SourceWrite) (WriteResult, error) {
	if s.writeErr != nil {
		return WriteResult{}, s.writeErr
	}
	s.writes = append(s.writes, *write)
	return WriteResult{Status: StatusAdded, SourceID: "kbs_1", Chunks: len(write.Chunks), ChunksAdded: len(write.Chunks)}, nil
}

func (s *fakeStore) ListCollections(context.Context) ([]CollectionSummary, error) {
	return []CollectionSummary{{ID: "support", Sources: 1, Chunks: 2}}, nil
}

func (s *fakeStore) ListSources(_ context.Context, collection string) ([]SourceRecord, error) {
	return s.sources[collection], nil
}

func (s *fakeStore) RemoveSource(_ context.Context, collection, uri string, apply bool) (RemoveOutcome, error) {
	if s.removeErr != nil {
		return RemoveOutcome{Applied: s.applied}, s.removeErr
	}
	if apply {
		s.removed = append(s.removed, collection+"|"+uri)
	}
	return RemoveOutcome{SourceID: "kbs_1", Chunks: 2, Applied: apply, WALTruncated: true}, nil
}

func (s *fakeStore) Check(context.Context) (IndexReport, error) { return s.report, nil }

type fakeSecrets struct {
	screen SecretScreen
	err    error
	calls  int
}

func (s *fakeSecrets) Snapshot(context.Context) (SecretScreen, error) {
	s.calls++
	return s.screen, s.err
}

type recordedActions struct {
	actions []OwnerAction
	err     error
}

func (r *recordedActions) RecordOwnerAction(_ context.Context, action *OwnerAction) error {
	r.actions = append(r.actions, *action)
	return r.err
}

type serviceFixture struct {
	service *Service
	store   *fakeStore
	secrets *fakeSecrets
	actions *recordedActions
	doer    *fakeDoer
}

func newFixture(t *testing.T) *serviceFixture {
	t.Helper()
	fixture := &serviceFixture{
		store:   &fakeStore{sources: map[string][]SourceRecord{}},
		secrets: &fakeSecrets{screen: fakeVault{values: []string{"vault-secret-value"}}},
		actions: &recordedActions{},
		doer:    &fakeDoer{header: contentType("text/html"), body: []byte("<p>refund page</p>")},
	}
	service, err := NewService(&Deps{
		Store:   fixture.store,
		Secrets: fixture.secrets,
		Reducer: func(_ context.Context, source []byte, _ int64) (string, bool, error) {
			return strings.NewReplacer("<p>", "", "</p>", "").Replace(string(source)), false, nil
		},
		HTTP:    fixture.doer,
		Actions: fixture.actions,
		Now:     func() time.Time { return time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	fixture.service = service
	return fixture
}

func TestNewServiceRequiresEveryDependency(t *testing.T) {
	full := func() Deps {
		fixture := newFixture(t)
		return Deps{
			Store: fixture.store, Secrets: fixture.secrets, Reducer: func(context.Context, []byte, int64) (string, bool, error) { return "", false, nil },
			HTTP: fixture.doer, Actions: fixture.actions,
		}
	}
	cases := []struct {
		name   string
		mutate func(*Deps)
	}{
		{"store", func(d *Deps) { d.Store = nil }},
		{"secrets", func(d *Deps) { d.Secrets = nil }},
		{"reducer", func(d *Deps) { d.Reducer = nil }},
		{"http", func(d *Deps) { d.HTTP = nil }},
		{"actions", func(d *Deps) { d.Actions = nil }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deps := full()
			tc.mutate(&deps)
			_, err := NewService(&deps)
			requireCode(t, err, ErrorCodeInvalidArgument)
		})
	}
	if _, err := NewService(nil); err == nil {
		t.Fatal("NewService accepted nil dependencies")
	} else {
		requireCode(t, err, ErrorCodeInvalidArgument)
	}
}

func TestCollectionIDValidation(t *testing.T) {
	cases := []struct {
		id    string
		valid bool
	}{
		{"support", true},
		{"hr-docs", true},
		{"a1", true},
		{"a" + strings.Repeat("b", 47), true},
		{"a", false},
		{"", false},
		{"1abc", false},
		{"Support", false},
		{"hr_docs", false},
		{"-docs", false},
		{"docs/x", false},
		{"a" + strings.Repeat("b", 48), false},
	}
	fixture := newFixture(t)
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			if got := ValidCollectionID(tc.id); got != tc.valid {
				t.Errorf("ValidCollectionID(%q) = %v, want %v", tc.id, got, tc.valid)
			}
			if tc.valid {
				return
			}
			_, err := fixture.service.Add(t.Context(), tc.id, "irrelevant.md")
			requireCode(t, err, ErrorCodeCollectionInvalid)
			_, err = fixture.service.Show(t.Context(), tc.id)
			requireCode(t, err, ErrorCodeCollectionInvalid)
			_, err = fixture.service.Remove(t.Context(), tc.id, "x", true)
			requireCode(t, err, ErrorCodeCollectionInvalid)
			_, err = fixture.service.Refresh(t.Context(), tc.id, "")
			requireCode(t, err, ErrorCodeCollectionInvalid)
		})
	}
}

func TestAddFileWritesNormalizedChunksAndRecordsTheOwnerAction(t *testing.T) {
	fixture := newFixture(t)
	path := writeSource(t, "Policy.md", []byte("# Refunds\nRefunds take "+runeText(0x200B)+"ten days.<!-- hidden -->\n"))
	result, err := fixture.service.Add(t.Context(), "support", path)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if result.Status != StatusAdded || result.Label != "Policy.md" || result.Kind != SourceKindFile || result.URI != path {
		t.Fatalf("result = %+v", result)
	}
	if len(fixture.store.writes) != 1 {
		t.Fatalf("writes = %d, want 1", len(fixture.store.writes))
	}
	write := fixture.store.writes[0]
	if write.Collection != "support" || write.IngestVersion != IngestVersion || len(write.ContentSHA256) != 64 ||
		write.CollectionCap != MaxChunksPerCollection || write.TotalCap != MaxChunksTotal {
		t.Fatalf("write = %+v", write)
	}
	if len(write.Chunks) != 1 || write.Chunks[0].Heading != "Refunds" || write.Chunks[0].Content != "Refunds take ten days." {
		t.Fatalf("chunks = %+v", write.Chunks)
	}
	if len(fixture.actions.actions) != 1 || fixture.actions.actions[0].Action != ActionAdd || fixture.actions.actions[0].SourceID != "kbs_1" ||
		fixture.actions.actions[0].Collection != "support" {
		t.Fatalf("owner actions = %+v", fixture.actions.actions)
	}
}

func TestAddRefusesASourceWithHeadingsOnly(t *testing.T) {
	fixture := newFixture(t)
	_, err := fixture.service.Add(t.Context(), "support", writeSource(t, "Empty.md", []byte("# Refunds\n## Returns\n")))
	if code, ok := CodeOf(err); !ok || code != ErrorCodeSourceUnsupported {
		t.Fatalf("err = %v, want %s", err, ErrorCodeSourceUnsupported)
	}
	if len(fixture.store.writes) != 0 {
		t.Fatalf("writes = %d, want 0", len(fixture.store.writes))
	}
}

func TestAddDigestIgnoresHiddenCharactersAndFollowsContent(t *testing.T) {
	fixture := newFixture(t)
	plain := writeSource(t, "a.md", []byte("same text"))
	hidden := writeSource(t, "b.md", []byte("same"+runeText(0x200B)+" text"))
	other := writeSource(t, "c.md", []byte("other text"))
	for _, path := range []string{plain, hidden, other} {
		if _, err := fixture.service.Add(t.Context(), "support", path); err != nil {
			t.Fatalf("Add %s: %v", path, err)
		}
	}
	digests := []string{fixture.store.writes[0].ContentSHA256, fixture.store.writes[1].ContentSHA256, fixture.store.writes[2].ContentSHA256}
	if digests[0] != digests[1] {
		t.Error("invisible characters changed the digest")
	}
	if digests[0] == digests[2] {
		t.Error("different content produced the same digest")
	}
}

func TestAddHTMLFileGoesThroughTheReducer(t *testing.T) {
	fixture := newFixture(t)
	path := writeSource(t, "page.html", []byte("<p>refund page</p>"))
	if _, err := fixture.service.Add(t.Context(), "support", path); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if got := fixture.store.writes[0].Chunks[0].Content; got != "refund page" {
		t.Fatalf("content = %q, want the reduced text", got)
	}
}

func TestAddHTMLReducerFailuresFailClosed(t *testing.T) {
	cases := []struct {
		name    string
		reducer HTMLReducer
		want    ErrorCode
	}{
		{"truncated output", func(context.Context, []byte, int64) (string, bool, error) { return "partial", true, nil }, ErrorCodeSourceTooLarge},
		{"reducer error", func(context.Context, []byte, int64) (string, bool, error) {
			return "", false, Errorf(ErrorCodeSourceUnsupported, "html document could not be parsed")
		}, ErrorCodeSourceUnsupported},
		{"empty output", func(context.Context, []byte, int64) (string, bool, error) { return "  \n", false, nil }, ErrorCodeSourceUnsupported},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newFixture(t)
			fixture.service.reducer = tc.reducer
			path := writeSource(t, "page.html", []byte("<p>x</p>"))
			_, err := fixture.service.Add(t.Context(), "support", path)
			requireCode(t, err, tc.want)
			if len(fixture.store.writes) != 0 || len(fixture.actions.actions) != 0 {
				t.Error("a failed ingest wrote data or recorded an action")
			}
		})
	}
}

func TestAddURLUsesTheHTTPClientAndStoresTheQueryInTheURI(t *testing.T) {
	fixture := newFixture(t)
	result, err := fixture.service.Add(t.Context(), "support", "https://Docs.Example.com/refunds?lang=en#top")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if result.Kind != SourceKindURL || result.URI != "https://docs.example.com/refunds?lang=en" || result.Label != "docs.example.com/refunds" {
		t.Fatalf("result = %+v", result)
	}
	if got := fixture.store.writes[0].Chunks[0].Content; got != "refund page" {
		t.Fatalf("content = %q", got)
	}
}

func TestAddRefusesNonHTTPSURLsAndEgressDenials(t *testing.T) {
	fixture := newFixture(t)
	_, err := fixture.service.Add(t.Context(), "support", "http://docs.example.com/a")
	requireCode(t, err, ErrorCodeURLRefused)
	fixture.doer.err = Errorf(ErrorCodeURLRefused, "destination is not permitted")
	_, err = fixture.service.Add(t.Context(), "support", "https://10.0.0.5/a")
	requireCode(t, err, ErrorCodeURLRefused)
	if len(fixture.store.writes) != 0 {
		t.Error("a refused URL wrote data")
	}
}

func TestAddRejectsSecretBearingSourcesWholeWithoutPrintingTheMatch(t *testing.T) {
	githubToken := "gh" + "p_" + strings.Repeat("a1B2", 9)
	cases := []struct {
		name    string
		content string
		secret  string
	}{
		{"vault value", "The admin password is vault-secret-value, keep it safe.", "vault-secret-value"},
		{"vault value split by hidden characters", "The admin password is vault-secret" + runeText(0x200B) + "-value, keep it safe.", "vault-secret-value"},
		{"credential pattern", "Use this token: " + githubToken, githubToken},
		{"private key", "-----BEGIN " + "OPENSSH PRIVATE KEY-----\nabc", "PRIVATE KEY"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newFixture(t)
			path := writeSource(t, "notes.md", []byte("# Intro\nharmless first section\n\n# Later\n"+tc.content+"\n"))
			_, err := fixture.service.Add(t.Context(), "support", path)
			requireCode(t, err, ErrorCodeSecretInvalid)
			if strings.Contains(err.Error(), tc.secret) {
				t.Errorf("error %q prints the match", err.Error())
			}
			if len(fixture.store.writes) != 0 || len(fixture.actions.actions) != 0 {
				t.Error("a secret-bearing source was partially ingested")
			}
		})
	}
}

func TestAddRejectsAURLThatCarriesAVaultValue(t *testing.T) {
	fixture := newFixture(t)
	_, err := fixture.service.Add(t.Context(), "support", "https://docs.example.com/a?key=vault-secret-value")
	requireCode(t, err, ErrorCodeSecretInvalid)
	if len(fixture.store.writes) != 0 {
		t.Error("a URL carrying a secret was ingested")
	}
}

func TestAddFailsClosedWhenASecretReferenceCannotBeResolved(t *testing.T) {
	fixture := newFixture(t)
	fixture.secrets.err = errors.New("unresolved secret references: telegram_bot_token_main")
	path := writeSource(t, "notes.md", []byte("harmless"))
	_, err := fixture.service.Add(t.Context(), "support", path)
	requireCode(t, err, ErrorCodeSecretUnavailable)
	if !strings.Contains(err.Error(), "telegram_bot_token_main") {
		t.Errorf("error %q does not name the unresolved reference", err.Error())
	}
	if len(fixture.store.writes) != 0 {
		t.Error("ingest wrote data although screening was unavailable")
	}
	if fixture.doer.requestedAs != nil {
		t.Error("the network was reached although screening was unavailable")
	}
}

func TestAddEnforcesTheChunkCapPerSource(t *testing.T) {
	fixture := newFixture(t)
	var builder strings.Builder
	for range MaxChunksPerSource + 1 {
		builder.WriteString("# H\nx\n")
	}
	path := writeSource(t, "many.md", []byte(builder.String()))
	_, err := fixture.service.Add(t.Context(), "support", path)
	requireCode(t, err, ErrorCodeCapExceeded)
	if len(fixture.store.writes) != 0 {
		t.Error("an over-cap source was written")
	}
}

func TestAddEmptyAndWhitespaceOnlySourcesAreRefused(t *testing.T) {
	for name, content := range map[string]string{"empty": "", "blank": " \n\t\n", "invisible only": runeText(0x200B, 0xFEFF)} {
		t.Run(name, func(t *testing.T) {
			fixture := newFixture(t)
			path := writeSource(t, "empty.txt", []byte(content))
			_, err := fixture.service.Add(t.Context(), "support", path)
			requireCode(t, err, ErrorCodeSourceUnsupported)
		})
	}
}

func TestAddStoreErrorsPassThroughWithoutAnOwnerAction(t *testing.T) {
	fixture := newFixture(t)
	fixture.store.writeErr = Errorf(ErrorCodeCapExceeded, "chunk cap would be exceeded")
	path := writeSource(t, "notes.md", []byte("harmless"))
	_, err := fixture.service.Add(t.Context(), "support", path)
	requireCode(t, err, ErrorCodeCapExceeded)
	if len(fixture.actions.actions) != 0 {
		t.Error("an action was recorded for a failed write")
	}
}

func TestAddReportsAFailedOwnerActionButKeepsTheResult(t *testing.T) {
	fixture := newFixture(t)
	fixture.actions.err = errors.New("event store unavailable")
	path := writeSource(t, "notes.md", []byte("harmless"))
	result, err := fixture.service.Add(t.Context(), "support", path)
	if err == nil || result.SourceID != "kbs_1" {
		t.Fatalf("Add = %+v, %v; want the written result and the audit error", result, err)
	}
}

func TestRefreshKeepsOldChunksWhenASourceFailsAndStillRefreshesTheOthers(t *testing.T) {
	fixture := newFixture(t)
	good := writeSource(t, "good.md", []byte("fresh content"))
	missing := filepath.Join(t.TempDir(), "gone.md")
	fixture.store.sources["support"] = []SourceRecord{
		{ID: "kbs_a", Collection: "support", Kind: SourceKindFile, URI: missing, Label: "gone.md"},
		{ID: "kbs_b", Collection: "support", Kind: SourceKindFile, URI: good, Label: "good.md"},
	}
	results, err := fixture.service.Refresh(t.Context(), "support", "")
	requireCode(t, err, ErrorCodeSourceNotFound)
	if !strings.Contains(err.Error(), "gone.md") {
		t.Errorf("error %q does not name the failing source", err.Error())
	}
	if len(results) != 1 || results[0].Label != "good.md" {
		t.Fatalf("results = %+v, want only the readable source", results)
	}
	if len(fixture.store.writes) != 1 || fixture.store.writes[0].URI != good {
		t.Fatalf("writes = %+v, the failing source must write nothing", fixture.store.writes)
	}
	if len(fixture.actions.actions) != 1 || fixture.actions.actions[0].Action != ActionRefresh {
		t.Fatalf("owner actions = %+v", fixture.actions.actions)
	}
}

func TestRefreshOfAURLSourceUsesTheStoredKindAndURI(t *testing.T) {
	fixture := newFixture(t)
	fixture.store.sources["support"] = []SourceRecord{{ID: "kbs_a", Collection: "support", Kind: SourceKindURL, URI: "https://docs.example.com/refunds?lang=en", Label: "docs.example.com/refunds"}}
	results, err := fixture.service.Refresh(t.Context(), "support", "https://docs.example.com/refunds?lang=en")
	if err != nil || len(results) != 1 {
		t.Fatalf("Refresh = %+v, %v", results, err)
	}
	if fixture.doer.requestedAs.RawQuery != "lang=en" {
		t.Errorf("requested %v, want the stored query kept", fixture.doer.requestedAs)
	}
}

func TestRefreshSelectsOneSourceByStoredURIAndRejectsUnknownOnes(t *testing.T) {
	fixture := newFixture(t)
	first := writeSource(t, "a.md", []byte("alpha"))
	second := writeSource(t, "b.md", []byte("beta"))
	fixture.store.sources["support"] = []SourceRecord{
		{Collection: "support", Kind: SourceKindFile, URI: first, Label: "a.md"},
		{Collection: "support", Kind: SourceKindFile, URI: second, Label: "b.md"},
	}
	results, err := fixture.service.Refresh(t.Context(), "support", second)
	if err != nil || len(results) != 1 || results[0].URI != second {
		t.Fatalf("Refresh one = %+v, %v", results, err)
	}
	_, err = fixture.service.Refresh(t.Context(), "support", "/not/stored.md")
	requireCode(t, err, ErrorCodeSourceNotFound)
	_, err = fixture.service.Refresh(t.Context(), "empty-one", "")
	requireCode(t, err, ErrorCodeSourceNotFound)
}

func TestRemoveDryRunRecordsNothingAndApplyRecordsTheAction(t *testing.T) {
	fixture := newFixture(t)
	outcome, err := fixture.service.Remove(t.Context(), "support", "/docs/a.md", false)
	if err != nil || outcome.Applied || len(fixture.store.removed) != 0 || len(fixture.actions.actions) != 0 {
		t.Fatalf("dry run = %+v, %v, removed %v, actions %v", outcome, err, fixture.store.removed, fixture.actions.actions)
	}
	outcome, err = fixture.service.Remove(t.Context(), "support", "/docs/a.md", true)
	if err != nil || !outcome.Applied || len(fixture.store.removed) != 1 {
		t.Fatalf("apply = %+v, %v", outcome, err)
	}
	if len(fixture.actions.actions) != 1 || fixture.actions.actions[0].Action != ActionRemove || fixture.actions.actions[0].Chunks != 2 {
		t.Fatalf("owner actions = %+v", fixture.actions.actions)
	}
	if _, err := fixture.service.Remove(t.Context(), "support", "", true); err == nil {
		t.Fatal("Remove accepted an empty source")
	}
}

func TestRemoveStillRecordsTheActionWhenOnlyTheLogTruncationFailed(t *testing.T) {
	fixture := newFixture(t)
	fixture.store.removeErr = errors.New("truncate write-ahead log")
	fixture.store.applied = true
	outcome, err := fixture.service.Remove(t.Context(), "support", "/docs/a.md", true)
	if err == nil || !outcome.Applied {
		t.Fatalf("Remove = %+v, %v; want the applied outcome with the error", outcome, err)
	}
	if len(fixture.actions.actions) != 1 {
		t.Fatalf("owner actions = %+v, want the applied removal recorded", fixture.actions.actions)
	}
}

func TestRemoveNotAppliedErrorsAreReturnedUnchanged(t *testing.T) {
	fixture := newFixture(t)
	fixture.store.removeErr = Errorf(ErrorCodeSourceNotFound, "source does not exist in the collection")
	_, err := fixture.service.Remove(t.Context(), "support", "/docs/a.md", true)
	requireCode(t, err, ErrorCodeSourceNotFound)
	if len(fixture.actions.actions) != 0 {
		t.Error("an action was recorded for a removal that did not happen")
	}
}

func TestShowSummarizesSourcesAndRefusesEmptyCollections(t *testing.T) {
	fixture := newFixture(t)
	fixture.store.sources["support"] = []SourceRecord{{URI: "/a.md", Chunks: 2}, {URI: "/b.md", Chunks: 3}}
	detail, err := fixture.service.Show(t.Context(), "support")
	if err != nil || detail.Chunks != 5 || len(detail.Sources) != 2 {
		t.Fatalf("Show = %+v, %v", detail, err)
	}
	_, err = fixture.service.Show(t.Context(), "nothing")
	requireCode(t, err, ErrorCodeSourceNotFound)
}

func TestCheckReportsAnIndexWithProblemsAsDamaged(t *testing.T) {
	fixture := newFixture(t)
	report, err := fixture.service.Check(t.Context())
	if err != nil || len(report.Problems) != 0 {
		t.Fatalf("healthy Check = %+v, %v", report, err)
	}
	fixture.store.report = IndexReport{Chunks: 3, IndexedChunks: 2, Problems: []string{"chunk count differs from indexed row count"}}
	report, err = fixture.service.Check(t.Context())
	requireCode(t, err, ErrorCodeIndexDamaged)
	if len(report.Problems) != 1 {
		t.Fatalf("report = %+v, want the problems returned with the error", report)
	}
}

func TestIngestedFilesUseTheNoFollowOpen(t *testing.T) {
	fixture := newFixture(t)
	target := writeSource(t, "real.md", []byte("content"))
	link := filepath.Join(t.TempDir(), "link.md")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	_, err := fixture.service.Add(t.Context(), "support", link)
	requireCode(t, err, ErrorCodeSourceNotRegular)
	if len(fixture.store.writes) != 0 {
		t.Error("a symlink was ingested")
	}
}
