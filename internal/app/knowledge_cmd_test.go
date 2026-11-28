package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	appconfig "github.com/anggasct/aura/internal/app/config"
	"github.com/anggasct/aura/internal/modules/knowledge"
	"github.com/anggasct/aura/internal/modules/search"
	"github.com/anggasct/aura/internal/platform/capability"
	"github.com/anggasct/aura/internal/platform/clikit"
	"github.com/anggasct/aura/internal/platform/egress"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/model"
	"github.com/anggasct/aura/internal/platform/operation"
	"github.com/anggasct/aura/internal/platform/sandbox"
	"github.com/anggasct/aura/internal/platform/secret/secrettest"
	"github.com/anggasct/aura/internal/platform/sqlite"
	"github.com/anggasct/aura/internal/platform/storage"
	"github.com/anggasct/aura/internal/platform/turn"
	turnstore "github.com/anggasct/aura/internal/platform/turn/store"
	platformwebhook "github.com/anggasct/aura/internal/platform/webhook"
)

func kbCommand(gf *GlobalFlags) *cobra.Command {
	root := &cobra.Command{Use: "aura", SilenceUsage: true, SilenceErrors: true}
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return NewUsageError(err) })
	mountOperationCommands(root, gf)
	cmd, _, _ := root.Find([]string{"kb"})
	root.RemoveCommand(cmd)
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	cmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return NewUsageError(err) })
	return cmd
}

func runKBCommand(t *testing.T, cfg string, args ...string) (string, error) {
	t.Helper()
	restoreDefaultLogger(t)
	cmd := kbCommand(&GlobalFlags{ConfigPath: cfg})
	out := &bytes.Buffer{}
	errBuf := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(errBuf)
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(t.Context())
	return out.String(), err
}

func addKBSource(ctx context.Context, service *knowledge.Service, collection, location string) (knowledge.IngestResult, error) {
	var prepared *knowledge.Prepared
	if knowledge.IsURLReference(location) {
		var err error
		if prepared, err = service.PrepareURL(ctx, collection, location); err != nil {
			return knowledge.IngestResult{}, err
		}
		return service.Write(ctx, knowledge.ActionAdd, prepared)
	}
	file, err := os.Open(location)
	if err != nil {
		return knowledge.IngestResult{}, err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return knowledge.IngestResult{}, err
	}
	prepared, err = service.PrepareStream(ctx, collection, &knowledge.Stream{URI: location, Name: filepath.Base(location), Size: info.Size(), Reader: file})
	if err != nil {
		return knowledge.IngestResult{}, err
	}
	return service.Write(ctx, knowledge.ActionAdd, prepared)
}

func requireKBCode(t *testing.T, err error, want knowledge.ErrorCode) {
	t.Helper()
	code, ok := fault.CodeOf[knowledge.ErrorCode](err)
	if !ok || code != want {
		t.Fatalf("error = %v, want code %s", err, want)
	}
}

func requireOperationCode(t *testing.T, err error, want operation.ErrorCode) {
	t.Helper()
	code, ok := fault.CodeOf[operation.ErrorCode](err)
	if !ok || code != want {
		t.Fatalf("error = %v, want code %s", err, want)
	}
}

func writeKBDocument(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write document: %v", err)
	}
	return path
}

func kbTestStorage(t *testing.T, cfgPath string) (*appconfig.Config, *sql.DB, string) {
	t.Helper()
	loaded, err := appconfig.Load(cfgPath)
	if err != nil {
		t.Fatalf("appconfig.Load: %v", err)
	}
	db, err := openStorage(t.Context(), loaded.Config)
	if err != nil {
		t.Fatalf("openStorage: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	dbPath, _, _, err := storagePaths(loaded.Config)
	if err != nil {
		t.Fatalf("storagePaths: %v", err)
	}
	return loaded.Config, db, dbPath
}

func countRows(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var count int
	if err := db.QueryRowContext(t.Context(), query, args...).Scan(&count); err != nil {
		t.Fatalf("count %q: %v", query, err)
	}
	return count
}

func TestKBLifecycleAddListShowRefreshRemove(t *testing.T) {
	cfg := writeMemoryCLIConfig(t)
	doc := writeKBDocument(t, "policy.md", "# Refunds\nRefunds are issued within thirty days.\n\n# Shipping\nShipping takes five days.\n")

	out, err := runKBCommand(t, cfg, "add", "support", doc)
	if err != nil || !strings.Contains(out, "added: policy.md chunks=2 added=2 removed=0 kept=0") {
		t.Fatalf("add = %q, %v", out, err)
	}
	out, err = runKBCommand(t, cfg, "add", "support", doc)
	if err != nil || !strings.Contains(out, "unchanged: policy.md chunks=2 added=0 removed=0 kept=2") {
		t.Fatalf("unchanged add = %q, %v", out, err)
	}

	out, err = runKBCommand(t, cfg, "list")
	if err != nil || !strings.Contains(out, "support\t1\t2\t1") {
		t.Fatalf("list = %q, %v", out, err)
	}
	out, err = runKBCommand(t, cfg, "list", "--json")
	if err != nil {
		t.Fatalf("list --json: %v", err)
	}
	var listed struct {
		Rows []struct {
			Collection string `json:"collection"`
			Chunks     int    `json:"chunks"`
		} `json:"rows"`
	}
	if err := json.Unmarshal([]byte(out), &listed); err != nil || len(listed.Rows) != 1 || listed.Rows[0].Collection != "support" || listed.Rows[0].Chunks != 2 {
		t.Fatalf("list --json = %q, %v", out, err)
	}
	out, err = runKBCommand(t, cfg, "show", "support")
	if err != nil || !strings.Contains(out, "collection: support sources=1 chunks=2") || !strings.Contains(out, "label=policy.md kind=file chunks=2") {
		t.Fatalf("show = %q, %v", out, err)
	}

	if err := os.WriteFile(doc, []byte("# Refunds\nRefunds are issued within sixty days.\n\n# Shipping\nShipping takes five days.\n"), 0o600); err != nil {
		t.Fatalf("rewrite document: %v", err)
	}
	out, err = runKBCommand(t, cfg, "refresh", "support")
	if err != nil || !strings.Contains(out, "replaced: policy.md chunks=2 added=1 removed=1 kept=1") {
		t.Fatalf("refresh = %q, %v", out, err)
	}
	out, err = runKBCommand(t, cfg, "refresh", "support", doc)
	if err != nil || !strings.Contains(out, "unchanged: policy.md") {
		t.Fatalf("refresh one = %q, %v", out, err)
	}

	out, err = runKBCommand(t, cfg, "remove", "support", doc)
	if err != nil || !strings.Contains(out, "would remove 2 chunks of the source") {
		t.Fatalf("remove dry run = %q, %v", out, err)
	}
	if out, err = runKBCommand(t, cfg, "list"); err != nil || !strings.Contains(out, "support\t1\t2\t") {
		t.Fatalf("dry run changed the collection: %q, %v", out, err)
	}
	out, err = runKBCommand(t, cfg, "remove", "support", doc, "--apply")
	if err != nil || !strings.Contains(out, "removed: 2 chunks") || !strings.Contains(out, "not removed: earlier replies") || !strings.Contains(out, "vacuumed") {
		t.Fatalf("remove apply = %q, %v", out, err)
	}
	if out, err = runKBCommand(t, cfg, "list"); err != nil || out != "COLLECTION\tSOURCES\tCHUNKS\tGENERATION\n" {
		t.Fatalf("list after remove = %q, %v", out, err)
	}
	_, err = runKBCommand(t, cfg, "show", "support")
	requireKBCode(t, err, knowledge.ErrorCodeSourceNotFound)
	_, err = runKBCommand(t, cfg, "remove", "support", doc, "--apply")
	requireKBCode(t, err, knowledge.ErrorCodeSourceNotFound)
}

func TestKBEveryActionRecordsAnOwnerAction(t *testing.T) {
	cfg := writeMemoryCLIConfig(t)
	doc := writeKBDocument(t, "policy.md", "Refunds are issued within thirty days.")
	for _, args := range [][]string{
		{"add", "support", doc},
		{"refresh", "support"},
		{"remove", "support", doc},
		{"remove", "support", doc, "--apply"},
	} {
		if out, err := runKBCommand(t, cfg, args...); err != nil {
			t.Fatalf("%v: %v (%s)", args, err, out)
		}
	}
	_, db, _ := kbTestStorage(t, cfg)
	rows, err := db.QueryContext(t.Context(), `SELECT payload_json FROM runtime_event WHERE kind = ? ORDER BY sequence`, knowledgeOwnerActionKind)
	if err != nil {
		t.Fatalf("query actions: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var actions []string
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			t.Fatalf("scan: %v", err)
		}
		var decoded struct {
			Action     string `json:"action"`
			Collection string `json:"collection"`
		}
		if err := json.Unmarshal([]byte(payload), &decoded); err != nil || decoded.Collection != "support" {
			t.Fatalf("payload %q: %v", payload, err)
		}
		if strings.Contains(payload, doc) {
			t.Errorf("payload %q carries the host path", payload)
		}
		actions = append(actions, decoded.Action)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if strings.Join(actions, ",") != "add,refresh,remove" {
		t.Fatalf("recorded actions = %v, want add, refresh, remove (the dry run records nothing)", actions)
	}
}

func TestKBRefreshFailureKeepsOldChunks(t *testing.T) {
	cfg := writeMemoryCLIConfig(t)
	doc := writeKBDocument(t, "policy.md", "Refunds are issued within thirty days.")
	if out, err := runKBCommand(t, cfg, "add", "support", doc); err != nil {
		t.Fatalf("add: %v (%s)", err, out)
	}
	if err := os.Remove(doc); err != nil {
		t.Fatalf("remove document: %v", err)
	}
	_, err := runKBCommand(t, cfg, "refresh", "support")
	requireKBCode(t, err, knowledge.ErrorCodeNothingRefreshed)
	if !strings.Contains(err.Error(), "policy.md (source_unavailable)") {
		t.Errorf("error %q does not name the source and its reason", err.Error())
	}
	out, err := runKBCommand(t, cfg, "show", "support")
	if err != nil || !strings.Contains(out, "chunks=1") {
		t.Fatalf("show after failed refresh = %q, %v; want the old chunks kept", out, err)
	}
	_, db, _ := kbTestStorage(t, cfg)
	if hits := countRows(t, db, `SELECT count(*) FROM kb_chunk_fts WHERE kb_chunk_fts MATCH '"refunds"'`); hits != 1 {
		t.Errorf("old chunk hits = %d, want 1", hits)
	}
	if out, err := runKBCommand(t, cfg, "check"); err != nil || !strings.Contains(out, "ok") {
		t.Fatalf("check = %q, %v", out, err)
	}
}

func TestKBAddRefusalsReturnTheirCodes(t *testing.T) {
	cfg := writeMemoryCLIConfig(t)
	realFile := writeKBDocument(t, "real.md", "content")
	oversized := writeKBDocument(t, "big.txt", strings.Repeat("a", knowledge.MaxSourceBytes+1))
	binary := writeKBDocument(t, "latin.txt", "caf\xe9")
	cases := []struct {
		name       string
		collection string
		source     string
		want       knowledge.ErrorCode
	}{
		{"invalid collection", "Support", realFile, knowledge.ErrorCodeCollectionInvalid},
		{"unsupported extension", "support", writeKBDocument(t, "doc.pdf", "x"), knowledge.ErrorCodeSourceUnsupported},
		{"oversized", "support", oversized, knowledge.ErrorCodeSourceTooLarge},
		{"not utf8", "support", binary, knowledge.ErrorCodeSourceNotUTF8},
		{"plain http", "support", "http://docs.example.com/a", knowledge.ErrorCodeURLRefused},
		{"url credentials", "support", "https://user:pass@docs.example.com/a", knowledge.ErrorCodeURLRefused},
		{"loopback address", "support", "https://127.0.0.1/a", knowledge.ErrorCodeURLRefused},
		{"private address", "support", "https://10.1.2.3/a", knowledge.ErrorCodeURLRefused},
		{"metadata address", "support", "https://169.254.169.254/latest", knowledge.ErrorCodeURLRefused},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := runKBCommand(t, cfg, "add", tc.collection, tc.source)
			requireKBCode(t, err, tc.want)
			if strings.Contains(out, "added") {
				t.Errorf("output %q reports a refused source as added", out)
			}
			if strings.Contains(err.Error(), "pass") {
				t.Errorf("error %q prints URL credentials", err.Error())
			}
		})
	}
	_, db, _ := kbTestStorage(t, cfg)
	if chunks := countRows(t, db, `SELECT count(*) FROM kb_chunk`); chunks != 0 {
		t.Errorf("refused sources left %d chunks", chunks)
	}
}

func TestKBAddRefusesHostFilesTheKernelCannotOpen(t *testing.T) {
	cfg := writeMemoryCLIConfig(t)
	realFile := writeKBDocument(t, "real.md", "content")
	link := filepath.Join(t.TempDir(), "link.md")
	if err := os.Symlink(realFile, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	cases := []struct {
		name   string
		source string
		want   operation.ErrorCode
	}{
		{"symlink", link, operation.ErrorCodePolicyDenied},
		{"directory", t.TempDir(), operation.ErrorCodeSourceUnavailable},
		{"missing", filepath.Join(t.TempDir(), "missing.md"), operation.ErrorCodeSourceUnavailable},
		{"an attachment that does not exist", "artifact:art-nope", operation.ErrorCodeSourceUnavailable},
	}
	wantText := map[string]string{
		"symlink":                           "policy_denied: the path is a symbolic link",
		"an attachment that does not exist": "source_unavailable: the attachment is not available",
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := runKBCommand(t, cfg, "add", "support", tc.source)

			requireOperationCode(t, err, tc.want)
			if want, pinned := wantText[tc.name]; pinned && err.Error() != want {
				t.Errorf("error = %q, want exactly %q", err.Error(), want)
			}
			if strings.Contains(err.Error(), "sql:") || strings.Contains(err.Error(), "too many levels") || strings.Contains(err.Error(), "workspace_") {
				t.Errorf("error %q carries an internal cause", err.Error())
			}
			if strings.Contains(out, "added") || strings.Contains(err.Error(), filepath.Dir(tc.source)) {
				t.Errorf("out %q, error %q: a refused source was reported added or the error carries the host path", out, err)
			}
		})
	}
	_, db, _ := kbTestStorage(t, cfg)
	if chunks := countRows(t, db, `SELECT count(*) FROM kb_chunk`); chunks != 0 {
		t.Errorf("refused sources left %d chunks", chunks)
	}
}

type privateResolver struct{}

func (privateResolver) LookupIP(context.Context, string) ([]net.IP, error) {
	return []net.IP{net.ParseIP("10.0.0.5")}, nil
}

var _ egress.Resolver = privateResolver{}

func TestKnowledgeURLThroughEgressRefusesAHostnameResolvingToAPrivateAddress(t *testing.T) {
	cfgPath := writeMemoryCLIConfig(t)
	cfg, db, _ := kbTestStorage(t, cfgPath)
	service, err := newKnowledgeService(cfg, db, privateResolver{})
	if err != nil {
		t.Fatalf("newKnowledgeService: %v", err)
	}
	_, err = addKBSource(t.Context(), service, "support", "https://docs.example.test/refunds?token=abc")
	requireKBCode(t, err, knowledge.ErrorCodeURLRefused)
	if strings.Contains(err.Error(), "token=abc") || strings.Contains(err.Error(), "10.0.0.5") {
		t.Errorf("error %q leaks the URL or the resolved address", err.Error())
	}
}

func TestKnowledgeIngestRejectsSecretBearingSourcesAndFailsClosedOnUnresolvableReferences(t *testing.T) {
	cfgPath := writeMemoryCLIConfig(t)
	cfg, db, _ := kbTestStorage(t, cfgPath)
	const secretValue = "kb-webhook-secret-value-0123456789"
	t.Setenv("KB_TEST_WEBHOOK_SECRET", secretValue)
	cfg.Webhook.Keys = []platformwebhook.Key{{ID: "hook", SecretEnv: "KB_TEST_WEBHOOK_SECRET"}}
	service, err := newKnowledgeService(cfg, db, nil)
	if err != nil {
		t.Fatalf("newKnowledgeService: %v", err)
	}

	clean := writeKBDocument(t, "clean.md", "Refunds are issued within thirty days.")
	if _, err := addKBSource(t.Context(), service, "support", clean); err != nil {
		t.Fatalf("clean Add: %v", err)
	}

	leaky := writeKBDocument(t, "leaky.md", "# Intro\nfine\n\n# Ops\nThe webhook secret is "+secretValue+" in production.\n")
	_, err = addKBSource(t.Context(), service, "support", leaky)
	requireKBCode(t, err, knowledge.ErrorCodeSecretInvalid)
	if strings.Contains(err.Error(), secretValue) {
		t.Errorf("error %q prints the matched secret", err.Error())
	}
	if chunks := countRows(t, db, `SELECT count(*) FROM kb_chunk c JOIN kb_source s ON s.id = c.source_id WHERE s.label = 'leaky.md'`); chunks != 0 {
		t.Errorf("rejected source left %d chunks", chunks)
	}

	cfg.Webhook.Keys = []platformwebhook.Key{{ID: "hook", SecretEnv: "KB_TEST_UNSET_SECRET_VARIABLE"}}
	_, err = addKBSource(t.Context(), service, "support", writeKBDocument(t, "other.md", "harmless text"))
	requireKBCode(t, err, knowledge.ErrorCodeSecretUnavailable)
	if !strings.Contains(err.Error(), "webhook_hook") {
		t.Errorf("error %q does not name the unresolved reference", err.Error())
	}
	if sources := countRows(t, db, `SELECT count(*) FROM kb_source WHERE label = 'other.md'`); sources != 0 {
		t.Error("ingest wrote a source although a configured secret reference was unresolvable")
	}
}

func TestStrictSecretVaultNamesEveryUnresolvedReferenceAndMatchesTheLenientVault(t *testing.T) {
	cfg := appconfig.Default()
	t.Setenv("KB_TEST_SET_SECRET", "set-secret-value")
	cfg.Webhook.Keys = []platformwebhook.Key{
		{ID: "set", SecretEnv: "KB_TEST_SET_SECRET"},
		{ID: "unset", SecretEnv: "KB_TEST_UNSET_SECRET_ONE"},
	}
	cfg.Tools = &appconfig.Tools{WebSearch: search.Config{CredentialRef: secrettest.Ref("env://KB_TEST_UNSET_SECRET_TWO")}}

	_, err := buildStrictSecretVault(&cfg)
	if err == nil || !strings.Contains(err.Error(), "webhook_unset") || !strings.Contains(err.Error(), "web_search_credential") {
		t.Fatalf("strict vault error = %v, want both unresolved names", err)
	}
	if strings.Contains(err.Error(), "set-secret-value") || strings.Contains(err.Error(), "KB_TEST_UNSET") {
		t.Errorf("error %q prints a value or variable name", err.Error())
	}

	lenient := buildSecretVault(&cfg)
	if !lenient.Contains("the key is set-secret-value") {
		t.Error("lenient vault lost a resolvable secret")
	}
	if lenient.Contains("nothing here") {
		t.Error("lenient vault matches unrelated text")
	}

	t.Setenv("KB_TEST_UNSET_SECRET_ONE", "one-secret-value")
	t.Setenv("KB_TEST_UNSET_SECRET_TWO", "two-secret-value")
	strict, err := buildStrictSecretVault(&cfg)
	if err != nil {
		t.Fatalf("strict vault: %v", err)
	}
	for _, value := range []string{"set-secret-value", "one-secret-value", "two-secret-value"} {
		if !strict.Contains("x " + value + " y") {
			t.Errorf("strict vault does not hold %q", value)
		}
	}
}

func TestKBRemoveApplyErasesTheTermFromTheIndexAndDatabaseFiles(t *testing.T) {
	cfg := writeMemoryCLIConfig(t)
	const term = "quokkafernweed"
	doc := writeKBDocument(t, "secret-notes.md", "The "+term+" appears only in this document.")
	keep := writeKBDocument(t, "keep.md", "This unrelated document stays in the collection.")
	for _, source := range []string{doc, keep} {
		if out, err := runKBCommand(t, cfg, "add", "support", source); err != nil {
			t.Fatalf("add: %v (%s)", err, out)
		}
	}
	_, db, dbPath := kbTestStorage(t, cfg)
	if !fileContains(t, dbPath, term) {
		t.Fatal("database file does not hold the term before removal")
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if out, err := runKBCommand(t, cfg, "remove", "support", doc, "--apply"); err != nil {
		t.Fatalf("remove: %v (%s)", err, out)
	}

	for _, path := range []string{dbPath, dbPath + "-wal"} {
		if fileContains(t, path, term) {
			t.Errorf("%s still holds the term after removal", filepath.Base(path))
		}
	}
	_, reopened, _ := kbTestStorage(t, cfg)
	rows, err := reopened.QueryContext(t.Context(), `SELECT block FROM kb_chunk_fts_data`)
	if err != nil {
		t.Fatalf("read index: %v", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var block []byte
		if err := rows.Scan(&block); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if bytes.Contains(block, []byte(term[:10])) {
			t.Fatal("kb_chunk_fts_data still holds the term after removal")
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if out, err := runKBCommand(t, cfg, "check"); err != nil || !strings.Contains(out, "ok") {
		t.Fatalf("check after removal = %q, %v", out, err)
	}
}

func fileContains(t *testing.T, path, needle string) bool {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return false
	}
	if err != nil {
		t.Fatalf("read %s: %v", filepath.Base(path), err)
	}
	return bytes.Contains(data, []byte(needle))
}

func TestKBCheckReportsADamagedIndexAndFailsOtherwiseClean(t *testing.T) {
	cfg := writeMemoryCLIConfig(t)
	doc := writeKBDocument(t, "policy.md", "# One\nalpha\n\n# Two\nbeta\n")
	if out, err := runKBCommand(t, cfg, "add", "support", doc); err != nil {
		t.Fatalf("add: %v (%s)", err, out)
	}
	out, err := runKBCommand(t, cfg, "check")
	if err != nil || !strings.Contains(out, "collections=1 sources=1 chunks=2 indexed=2") || !strings.Contains(out, "ok") {
		t.Fatalf("healthy check = %q, %v", out, err)
	}

	_, db, _ := kbTestStorage(t, cfg)
	for _, statement := range []string{`DROP TRIGGER kb_chunk_fts_delete`, `DELETE FROM kb_chunk WHERE ordinal = 0`} {
		if _, err := db.ExecContext(t.Context(), statement); err != nil {
			t.Fatalf("damage index: %v", err)
		}
	}
	out, err = runKBCommand(t, cfg, "check")
	requireKBCode(t, err, knowledge.ErrorCodeIndexDamaged)
	if out != "" || !strings.Contains(err.Error(), "problem") {
		t.Errorf("check output %q error %v; want the problems in the error", out, err)
	}
}

func TestKBCommandsRejectBadArguments(t *testing.T) {
	cfg := writeMemoryCLIConfig(t)
	for _, args := range [][]string{
		{"add"}, {"add", "support"}, {"remove", "support"}, {"show"}, {"refresh"}, {"list", "extra"}, {"check", "extra"},
	} {
		if _, err := runKBCommand(t, cfg, args...); err == nil {
			t.Errorf("%v succeeded", args)
		}
	}
}

func TestKnowledgeStoreAdapterTranslatesStoreErrors(t *testing.T) {
	cfgPath := writeMemoryCLIConfig(t)
	_, db, _ := kbTestStorage(t, cfgPath)
	kbStore, err := knowledge.NewSQLiteStore(db)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	err = sqlite.RunInTx(t.Context(), db, func(txCtx context.Context) error {
		_, err := kbStore.DeleteSource(txCtx, "support", "/missing.md")
		return err
	})
	requireKBCode(t, err, knowledge.ErrorCodeSourceNotFound)
	if _, err := kbStore.ReplaceSource(t.Context(), nil); err == nil {
		t.Fatal("ReplaceSource accepted nil")
	}
}

func availableSandbox() (sandbox.Primitives, error) {
	return sandbox.Primitives{UserNamespace: true, PidNamespace: true, Seccomp: true, CgroupV2: true, Landlock: true, ProcessGroups: true}, nil
}

func TestStatusToleratesADatabaseWithoutTheKnowledgeSchema(t *testing.T) {
	dataRoot := t.TempDir()
	seedHealthyStorage(t, dataRoot)
	dbPath := filepath.Join(dataRoot, "aura.db")
	db, err := sqlite.Open(t.Context(), dbPath, sqlite.Options{})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	for _, statement := range []string{
		`DROP TABLE kb_chunk_fts`, `DROP TABLE kb_chunk`, `DROP TABLE kb_source`, `DROP TABLE kb_collection`,
		`DELETE FROM schema_migration WHERE version = 39`,
	} {
		if _, err := db.ExecContext(t.Context(), statement); err != nil {
			t.Fatalf("downgrade %q: %v", statement, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	cfg := appconfig.Default()
	cfg.Storage.Path = dataRoot
	ignoreHostDiskHeadroom(&cfg)
	cfg.Models.Definitions = map[string]model.Definition{"primary": {Protocol: "anthropic", Model: "claude-sonnet-4"}}
	cmd := &cobra.Command{Use: "status"}
	cmd.SetContext(t.Context())
	cmd.RunE = func(c *cobra.Command, _ []string) error {
		return runStatus(c, &cfg, capability.Report{}, availableSandbox, true, false, pinnedProcessProbe)
	}
	var out bytes.Buffer
	cmd.SetOut(&out)
	err = cmd.RunE(cmd, nil)
	if code, ok := clikit.ExitCodeOf(err); ok && code == exitCommand {
		t.Fatalf("status failed as a command error on a pre-knowledge database: %v", err)
	}
	if strings.Contains(out.String(), "no such table") || strings.Contains(out.String(), "kb_") {
		t.Errorf("status output touches the missing schema:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "migration") {
		t.Errorf("status does not report the pending migration:\n%s", out.String())
	}
}

func TestKnowledgeHTTPClientRefusesRedirectsToDeniedDestinations(t *testing.T) {
	client := knowledge.NewHTTPClient(privateResolver{})
	cases := []struct {
		name   string
		target string
	}{
		{"private literal", "https://10.0.0.1/a"},
		{"hostname resolving privately", "https://docs.example.test/a"},
		{"plain http", "http://docs.example.com/a"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, tc.target, http.NoBody)
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			err = client.CheckRedirect(request, nil)
			requireKBCode(t, err, knowledge.ErrorCodeURLRefused)
		})
	}
}

func TestKBRefreshReadsTheWorkspaceFilesAndAttachmentsAnAgentAdded(t *testing.T) {
	cfgPath := writeMemoryCLIConfig(t)
	cfg, db, _ := kbTestStorage(t, cfgPath)
	service, err := newKnowledgeService(cfg, db, nil)
	if err != nil {
		t.Fatalf("newKnowledgeService: %v", err)
	}
	now := time.Now().UTC()
	if err := turnstore.NewSessionService(db).Create(t.Context(), &turn.Session{ID: "session-1", OwnerID: "owner-1", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("create the session: %v", err)
	}
	_, artifactRoot, _, err := storage.ResolvePaths(&cfg.Storage)
	if err != nil {
		t.Fatalf("ResolvePaths: %v", err)
	}
	artifacts := storage.NewArtifactStore(db, artifactRoot, int64(cfg.Storage.ArtifactQuota))
	attachment, err := artifacts.Put(t.Context(), strings.NewReader("# Faq\nHow do refunds work?\n"), &storage.ArtifactMetadata{
		ID: "art-0123456789abcdef", SessionID: "session-1", Filename: "faq.md", MediaType: "text/markdown",
	})
	if err != nil {
		t.Fatalf("store the attachment: %v", err)
	}
	workspaceFile := filepath.Join(cfg.Tools.Workspace, "docs", "policy.md")
	if err := os.MkdirAll(filepath.Dir(workspaceFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(workspaceFile, []byte("# Policy\nRefunds are issued within thirty days.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, stored := range []struct{ uri, name, body string }{
		{"workspace:docs/policy.md", "policy.md", "# Policy\nRefunds are issued within thirty days.\n"},
		{"artifact:" + attachment.ID, "faq.md", "# Faq\nHow do refunds work?\n"},
	} {
		prepared, err := service.PrepareStream(t.Context(), "support", &knowledge.Stream{
			URI: stored.uri, Name: stored.name, MediaType: "text/markdown", Size: int64(len(stored.body)), Reader: strings.NewReader(stored.body),
		})
		if err != nil {
			t.Fatalf("prepare %s: %v", stored.uri, err)
		}
		if _, err := service.Write(t.Context(), knowledge.ActionAdd, prepared); err != nil {
			t.Fatalf("write %s: %v", stored.uri, err)
		}
	}
	if err := os.WriteFile(workspaceFile, []byte("# Policy\nRefunds are issued within sixty days.\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := runKBCommand(t, cfgPath, "refresh", "support")

	if err != nil {
		t.Fatalf("refresh: %v (%s)", err, out)
	}
	if !strings.Contains(out, "replaced: policy.md") || !strings.Contains(out, "unchanged: faq.md") {
		t.Fatalf("refresh output = %q, want the workspace file replaced and the attachment reported unchanged", out)
	}
	show, err := runKBCommand(t, cfgPath, "show", "support")
	if err != nil || !strings.Contains(show, "workspace:docs/policy.md") || !strings.Contains(show, "artifact:"+attachment.ID) {
		t.Fatalf("show = %q, %v; want both stored sources kept", show, err)
	}
}

func TestKBAddHelpDescribesOnlyWhatTheCLIDoes(t *testing.T) {
	cfg := writeMemoryCLIConfig(t)

	out, err := runKBCommand(t, cfg, "add", "--help")

	if err != nil {
		t.Fatalf("help: %v", err)
	}
	if !strings.Contains(out, "add <collection> <source>") || !strings.Contains(out, "a file on this host") {
		t.Errorf("help = %q, want the source as a file on this host or an address", out)
	}
	for _, refused := range []string{"workspace", "attachment", "artifact:"} {
		if strings.Contains(out, refused) {
			t.Errorf("help = %q, mentions %q which is not true on the CLI", out, refused)
		}
	}
}
