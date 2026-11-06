package cli

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/embedding"
	"github.com/anggasct/aura/internal/knowledge"
)

type providerCall struct {
	path          string
	authorization string
	body          map[string]any
	inputs        []string
}

type providerItem struct {
	Object    string    `json:"object"`
	Index     int       `json:"index"`
	Embedding []float64 `json:"embedding"`
}

type providerList struct {
	Object string         `json:"object"`
	Data   []providerItem `json:"data"`
}

type embeddingProvider struct {
	server    *httptest.Server
	dims      int
	mu        sync.Mutex
	calls     []providerCall
	failCall  int
	badShape  bool
	onRequest func(n int)
	vectorFor func(input string, width int) []float64
}

func newEmbeddingProvider(t *testing.T, dims int) *embeddingProvider {
	t.Helper()
	provider := &embeddingProvider{dims: dims}
	provider.server = httptest.NewServer(http.HandlerFunc(provider.serve))
	t.Cleanup(provider.server.Close)
	return provider
}

func (p *embeddingProvider) serve(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	call := providerCall{path: r.URL.Path, authorization: r.Header.Get("Authorization")}
	_ = json.Unmarshal(raw, &call.body)
	if list, ok := call.body["input"].([]any); ok {
		for _, item := range list {
			call.inputs = append(call.inputs, fmt.Sprint(item))
		}
	}
	p.mu.Lock()
	p.calls = append(p.calls, call)
	number := len(p.calls)
	failCall, badShape, dims, hook, vectorFor := p.failCall, p.badShape, p.dims, p.onRequest, p.vectorFor
	p.mu.Unlock()
	if hook != nil {
		hook(number)
	}
	if failCall != 0 && number == failCall {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	items := make([]providerItem, 0, len(call.inputs))
	for i, input := range call.inputs {
		width := dims
		if badShape {
			width = dims - 1
		}
		vector := providerVector(input, width)
		if vectorFor != nil {
			vector = vectorFor(input, width)
		}
		items = append(items, providerItem{Object: "embedding", Index: i, Embedding: vector})
	}
	body, err := json.Marshal(providerList{Object: "list", Data: items})
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

func providerVector(input string, width int) []float64 {
	hash := fnv.New64a()
	_, _ = hash.Write([]byte(input))
	seed := float64(hash.Sum64()%1000) + 1
	vector := make([]float64, width)
	for i := range vector {
		vector[i] = math.Sin(float64(i)*0.37+seed) + 2
	}
	return vector
}

func (p *embeddingProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.calls)
}

func (p *embeddingProvider) inputCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	total := 0
	for i := range p.calls {
		total += len(p.calls[i].inputs)
	}
	return total
}

func (p *embeddingProvider) reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = nil
}

func embeddingYAML(baseURL string, dims int, extra string) string {
	return fmt.Sprintf("knowledge:\n  embedding:\n    base_url: %s\n    model: test-model\n    dimensions: %d\n%s", baseURL, dims, extra)
}

func writeEmbeddingConfig(t *testing.T, section string) string {
	t.Helper()
	path := writeMemoryCLIConfig(t)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if err := os.WriteFile(path, append(raw, []byte(section)...), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func replaceEmbeddingSection(t *testing.T, path, section string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	text := string(raw)
	if index := strings.Index(text, "knowledge:\n"); index >= 0 {
		text = text[:index]
	}
	if err := os.WriteFile(path, []byte(text+section), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

type embedRig struct {
	cfgPath  string
	cfg      *config.Config
	db       *sql.DB
	dbPath   string
	provider *embeddingProvider
}

func newEmbedRig(t *testing.T, dims int, extra string) *embedRig {
	t.Helper()
	provider := newEmbeddingProvider(t, dims)
	cfgPath := writeEmbeddingConfig(t, embeddingYAML(provider.server.URL+"/v1", dims, extra))
	cfg, db, dbPath := kbTestStorage(t, cfgPath)
	return &embedRig{cfgPath: cfgPath, cfg: cfg, db: db, dbPath: dbPath, provider: provider}
}

func (r *embedRig) run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	return runKBCommand(t, r.cfgPath, args...)
}

func (r *embedRig) addDoc(t *testing.T, collection, name, content string) string {
	t.Helper()
	path := writeKBDocument(t, name, content)
	if out, err := r.run(t, "add", collection, path); err != nil {
		t.Fatalf("add %s: %v\n%s", name, err, out)
	}
	return path
}

func (r *embedRig) fingerprint(t *testing.T) string {
	t.Helper()
	loaded, err := config.Load(r.cfgPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	fingerprint, err := embedding.Fingerprint(loaded.Config.Knowledge.Embedding)
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	return fingerprint
}

func (r *embedRig) storedFingerprint(t *testing.T, collection string) sql.NullString {
	t.Helper()
	var stored sql.NullString
	if err := r.db.QueryRowContext(t.Context(), `SELECT embedding_fingerprint FROM kb_collection WHERE id = ?`, collection).Scan(&stored); err != nil {
		t.Fatalf("read stored fingerprint: %v", err)
	}
	return stored
}

func (r *embedRig) vectors(t *testing.T) int {
	t.Helper()
	return countRows(t, r.db, `SELECT count(*) FROM kb_embedding`)
}

func (r *embedRig) generation(t *testing.T, collection string) int {
	t.Helper()
	return countRows(t, r.db, `SELECT generation FROM kb_collection WHERE id = ?`, collection)
}

const twoSectionDocument = "# Refunds\nRefunds are issued within thirty days.\n\n# Shipping\nShipping takes five days.\n"

func TestKBEmbedDryRunChangesNothingAndSendsNothing(t *testing.T) {
	rig := newEmbedRig(t, 4, "")
	rig.addDoc(t, "support", "policy.md", twoSectionDocument)
	before := rig.generation(t, "support")

	out, err := rig.run(t, "embed", "support")
	if err != nil {
		t.Fatalf("embed dry run: %v\n%s", err, out)
	}
	host := strings.TrimPrefix(rig.provider.server.URL, "http://")
	for _, want := range []string{
		"dry-run: collection support would send 2 of 2 chunks (about ", "estimated", "model test-model at 4 dimensions",
		"destination: " + host + " (this host; nothing leaves it)",
		"public queries are not sent", "owner and trusted queries", "rerun with --apply",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dry run output lacks %q:\n%s", want, out)
		}
	}
	if rig.provider.callCount() != 0 {
		t.Fatalf("provider calls = %d, want none", rig.provider.callCount())
	}
	if rig.vectors(t) != 0 || rig.storedFingerprint(t, "support").Valid || rig.generation(t, "support") != before {
		t.Fatal("the dry run changed the database")
	}
}

func TestKBEmbedDryRunStatesThatPublicQueriesAreSentWhenConfigured(t *testing.T) {
	rig := newEmbedRig(t, 4, "    public_queries: true\n")
	rig.addDoc(t, "support", "policy.md", twoSectionDocument)
	out, err := rig.run(t, "embed", "support")
	if err != nil || !strings.Contains(out, "public_queries is true") {
		t.Fatalf("embed dry run = %q, %v", out, err)
	}
}

func TestKBEmbedApplyStoresUnitVectorsFingerprintAndOneGenerationBump(t *testing.T) {
	t.Setenv("KB_EMBED_TEST_KEY", "kb-embed-test-bearer-value")
	rig := newEmbedRig(t, 4, "    credential_ref: env://KB_EMBED_TEST_KEY\n    batch_size: 1\n")
	rig.addDoc(t, "support", "policy.md", twoSectionDocument)
	before := rig.generation(t, "support")

	out, err := rig.run(t, "embed", "support", "--apply")
	if err != nil || !strings.Contains(out, "embedded: 2 chunks in 2 batches") || !strings.Contains(out, "complete: every chunk has a vector") {
		t.Fatalf("embed apply = %q, %v", out, err)
	}
	if rig.vectors(t) != 2 {
		t.Fatalf("vectors = %d, want 2", rig.vectors(t))
	}
	if stored := rig.storedFingerprint(t, "support"); !stored.Valid || stored.String != rig.fingerprint(t) {
		t.Fatalf("stored fingerprint = %+v", stored)
	}
	if got := rig.generation(t, "support"); got != before+1 {
		t.Errorf("generation = %d, want %d (one bump across two batches)", got, before+1)
	}
	rows, err := rig.db.QueryContext(t.Context(), `SELECT vector, fingerprint FROM kb_embedding`)
	if err != nil {
		t.Fatalf("read vectors: %v", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var blob []byte
		var fingerprint string
		if err := rows.Scan(&blob, &fingerprint); err != nil {
			t.Fatalf("scan: %v", err)
		}
		var length float64
		for i := 0; i+4 <= len(blob); i += 4 {
			value := math.Float32frombits(uint32(blob[i]) | uint32(blob[i+1])<<8 | uint32(blob[i+2])<<16 | uint32(blob[i+3])<<24)
			length += float64(value) * float64(value)
		}
		if len(blob) != 16 || math.Abs(math.Sqrt(length)-1) > 1e-5 || fingerprint != rig.fingerprint(t) {
			t.Errorf("vector blob %d bytes length %v fingerprint %q", len(blob), math.Sqrt(length), fingerprint)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read vectors: %v", err)
	}
	if rig.provider.callCount() != 2 {
		t.Fatalf("provider calls = %d, want 2", rig.provider.callCount())
	}
	call := rig.provider.calls[0]
	if call.authorization != "Bearer kb-embed-test-bearer-value" || call.path != "/v1/embeddings" ||
		call.body["model"] != "test-model" || call.body["dimensions"] != float64(4) || len(call.body) != 3 {
		t.Errorf("provider request = %+v", call)
	}

	rig.provider.reset()
	out, err = rig.run(t, "embed", "support", "--apply")
	if err != nil || !strings.Contains(out, "embedded: 0 chunks") || rig.provider.callCount() != 0 {
		t.Fatalf("second apply = %q, %v, calls %d", out, err, rig.provider.callCount())
	}
	if got := rig.generation(t, "support"); got != before+1 {
		t.Errorf("generation after a no-op run = %d, want %d", got, before+1)
	}
}

func TestKBEmbedResumesAfterAProviderFailureAndSkipsStoredVectors(t *testing.T) {
	rig := newEmbedRig(t, 4, "    batch_size: 1\n")
	rig.addDoc(t, "support", "policy.md", "# One\nFirst body.\n\n# Two\nSecond body.\n\n# Three\nThird body.\n")
	before := rig.generation(t, "support")
	rig.provider.failCall = 2

	out, err := rig.run(t, "embed", "support", "--apply")
	requireKBCode(t, err, knowledge.ErrorCodeEmbeddingFailed)
	if !strings.Contains(out, "partial: rerun aura kb embed support --apply to resume") || rig.vectors(t) != 1 {
		t.Fatalf("after failure: out %q vectors %d", out, rig.vectors(t))
	}
	if rig.generation(t, "support") != before {
		t.Error("the generation moved before the run completed")
	}
	if !rig.storedFingerprint(t, "support").Valid {
		t.Error("the opt-in was not recorded when the run started")
	}

	rig.provider.reset()
	rig.provider.mu.Lock()
	rig.provider.failCall = 0
	rig.provider.mu.Unlock()
	out, err = rig.run(t, "embed", "support", "--apply")
	if err != nil || !strings.Contains(out, "embedded: 2 chunks") {
		t.Fatalf("resume = %q, %v", out, err)
	}
	if rig.provider.inputCount() != 2 || rig.vectors(t) != 3 || rig.generation(t, "support") != before+1 {
		t.Fatalf("resume sent %d inputs, vectors %d, generation %d", rig.provider.inputCount(), rig.vectors(t), rig.generation(t, "support"))
	}
}

func TestKBEmbedStoresNothingForAMalformedProviderResponse(t *testing.T) {
	rig := newEmbedRig(t, 4, "")
	rig.addDoc(t, "support", "policy.md", twoSectionDocument)
	rig.provider.badShape = true
	_, err := rig.run(t, "embed", "support", "--apply")
	requireKBCode(t, err, knowledge.ErrorCodeEmbeddingFailed)
	if rig.vectors(t) != 0 {
		t.Fatalf("vectors = %d, want none for a malformed batch", rig.vectors(t))
	}
}

func TestKBWithoutEmbeddingConfigurationBehavesAsTextOnly(t *testing.T) {
	provider := newEmbeddingProvider(t, 4)
	cfgPath := writeMemoryCLIConfig(t)
	_, db, _ := kbTestStorage(t, cfgPath)
	doc := writeKBDocument(t, "policy.md", twoSectionDocument)

	out, err := runKBCommand(t, cfgPath, "add", "support", doc)
	if err != nil || !strings.Contains(out, "added: policy.md chunks=2") || strings.Contains(out, "embedding:") {
		t.Fatalf("add = %q, %v", out, err)
	}
	for _, args := range [][]string{{"embed", "support"}, {"embed", "support", "--apply"}} {
		_, err = runKBCommand(t, cfgPath, args...)
		requireKBCode(t, err, knowledge.ErrorCodeEmbeddingUnavailable)
	}
	if provider.callCount() != 0 || countRows(t, db, `SELECT count(*) FROM kb_embedding`) != 0 {
		t.Fatal("an unconfigured deployment sent text or stored vectors")
	}
	if countRows(t, db, `SELECT count(*) FROM kb_collection WHERE embedding_fingerprint IS NOT NULL`) != 0 {
		t.Fatal("an opt-in was recorded without configuration")
	}
}

func TestKBEmbedRejectsAMissingOrInvalidCollection(t *testing.T) {
	rig := newEmbedRig(t, 4, "")
	_, err := rig.run(t, "embed", "missing", "--apply")
	requireKBCode(t, err, knowledge.ErrorCodeSourceNotFound)
	_, err = rig.run(t, "embed", "Bad Name")
	requireKBCode(t, err, knowledge.ErrorCodeCollectionInvalid)
	_, err = rig.run(t, "embed")
	if err == nil {
		t.Error("embed without a collection succeeded")
	}
	if rig.provider.callCount() != 0 {
		t.Fatal("text was sent for a missing collection")
	}
}

func TestKBChangedConfigurationStopsSendingUntilReembedded(t *testing.T) {
	rig := newEmbedRig(t, 4, "")
	rig.addDoc(t, "support", "policy.md", twoSectionDocument)
	if _, err := rig.run(t, "embed", "support", "--apply"); err != nil {
		t.Fatalf("embed: %v", err)
	}
	rig.provider.reset()
	oldFingerprint := rig.fingerprint(t)

	replaceEmbeddingSection(t, rig.cfgPath, strings.Replace(embeddingYAML(rig.provider.server.URL+"/v1", 4, ""), "test-model", "other-model", 1))
	if rig.fingerprint(t) == oldFingerprint {
		t.Fatal("changing the model did not change the fingerprint")
	}
	second := writeKBDocument(t, "faq.md", "# Hours\nWe open at nine.\n")
	out, err := rig.run(t, "add", "support", second)
	if err != nil || !strings.Contains(out, "added: faq.md") || !strings.Contains(out, "kb_reembed_required") {
		t.Fatalf("add after a model change = %q, %v", out, err)
	}
	preview, err := rig.run(t, "embed", "support")
	if err != nil || !strings.Contains(preview, "reembed:") {
		t.Fatalf("dry run after a model change = %q, %v", preview, err)
	}
	if rig.provider.callCount() != 0 {
		t.Fatalf("provider calls = %d after a changed configuration, want none", rig.provider.callCount())
	}
	if got := rig.storedFingerprint(t, "support"); got.String != oldFingerprint {
		t.Fatalf("stored fingerprint = %q, want the old one kept until the owner re-embeds", got.String)
	}

	out, err = rig.run(t, "embed", "support", "--apply")
	if err != nil || !strings.Contains(out, "embedded: 3 chunks") {
		t.Fatalf("re-embed = %q, %v", out, err)
	}
	if rig.provider.inputCount() != 3 || rig.storedFingerprint(t, "support").String != rig.fingerprint(t) {
		t.Fatalf("re-embed sent %d inputs; fingerprint %q", rig.provider.inputCount(), rig.storedFingerprint(t, "support").String)
	}
	if got := countRows(t, rig.db, `SELECT count(*) FROM kb_embedding WHERE fingerprint <> ?`, rig.fingerprint(t)); got != 0 {
		t.Errorf("vectors under the old fingerprint = %d", got)
	}
}

func TestKBAddAndRefreshEmbedOnlyChangedChunksOfAnOptedInCollection(t *testing.T) {
	rig := newEmbedRig(t, 4, "")
	doc := rig.addDoc(t, "support", "policy.md", twoSectionDocument)
	if rig.provider.callCount() != 0 {
		t.Fatal("add sent text before the collection opted in")
	}
	if _, err := rig.run(t, "embed", "support", "--apply"); err != nil {
		t.Fatalf("embed: %v", err)
	}
	rig.provider.reset()

	second := writeKBDocument(t, "faq.md", "# Hours\nWe open at nine.\n")
	out, err := rig.run(t, "add", "support", second)
	if err != nil || !strings.Contains(out, "embedding: 1 chunks sent for vectors") {
		t.Fatalf("add = %q, %v", out, err)
	}
	if rig.provider.inputCount() != 1 || rig.vectors(t) != 3 {
		t.Fatalf("add sent %d inputs, vectors %d", rig.provider.inputCount(), rig.vectors(t))
	}

	rig.provider.reset()
	if err := os.WriteFile(doc, []byte("# Refunds\nRefunds are issued within sixty days.\n\n# Shipping\nShipping takes five days.\n"), 0o600); err != nil {
		t.Fatalf("rewrite document: %v", err)
	}
	out, err = rig.run(t, "refresh", "support")
	if err != nil || !strings.Contains(out, "embedding: 1 chunks sent for vectors") {
		t.Fatalf("refresh = %q, %v", out, err)
	}
	if rig.provider.inputCount() != 1 || !strings.Contains(rig.provider.calls[0].inputs[0], "sixty days") || rig.vectors(t) != 3 {
		t.Fatalf("refresh sent %v, vectors %d", rig.provider.calls, rig.vectors(t))
	}

	rig.provider.reset()
	if out, err = rig.run(t, "refresh", "support"); err != nil || strings.Contains(out, "embedding:") || rig.provider.callCount() != 0 {
		t.Fatalf("unchanged refresh = %q, %v, calls %d", out, err, rig.provider.callCount())
	}
}

func TestKBAddKeepsTheTextWhenEmbeddingFails(t *testing.T) {
	rig := newEmbedRig(t, 4, "")
	rig.addDoc(t, "support", "policy.md", twoSectionDocument)
	if _, err := rig.run(t, "embed", "support", "--apply"); err != nil {
		t.Fatalf("embed: %v", err)
	}
	rig.provider.failCall = 2
	second := writeKBDocument(t, "faq.md", "# Hours\nWe open at nine.\n")
	out, err := rig.run(t, "add", "support", second)
	requireKBCode(t, err, knowledge.ErrorCodeEmbeddingFailed)
	if !strings.Contains(out, "added: faq.md") || countRows(t, rig.db, `SELECT count(*) FROM kb_chunk`) != 3 || rig.vectors(t) != 2 {
		t.Fatalf("out %q chunks %d vectors %d; the text must stay committed and the collection partial",
			out, countRows(t, rig.db, `SELECT count(*) FROM kb_chunk`), rig.vectors(t))
	}
}

func TestKBEmbedNeverSendsAChunkMatchingACurrentVaultValue(t *testing.T) {
	const credential = "kb-embed-vault-value-0123456789"
	rig := newEmbedRig(t, 4, "    credential_ref: env://KB_EMBED_VAULT_KEY\n")
	rig.addDoc(t, "support", "policy.md", "# Notes\nThe shared key is "+credential+" for the vendor.\n\n# Shipping\nShipping takes five days.\n")
	t.Setenv("KB_EMBED_VAULT_KEY", credential)

	out, err := rig.run(t, "embed", "support", "--apply")
	requireKBCode(t, err, knowledge.ErrorCodeSecretInvalid)
	if rig.provider.callCount() != 0 || strings.Contains(out, credential) || strings.Contains(fmt.Sprint(err), credential) {
		t.Fatalf("provider calls %d; output %q error %v", rig.provider.callCount(), out, err)
	}
	if rig.vectors(t) != 0 {
		t.Fatalf("vectors = %d, want none", rig.vectors(t))
	}
}

func TestKBEmbedWithAnUnresolvedCredentialSendsNothingAndKeepsTextRetrieval(t *testing.T) {
	rig := newEmbedRig(t, 4, "    credential_ref: env://KB_EMBED_UNSET_KEY\n")
	doc := rig.addDoc(t, "support", "policy.md", twoSectionDocument)
	_, err := rig.run(t, "embed", "support", "--apply")
	requireKBCode(t, err, knowledge.ErrorCodeEmbeddingUnavailable)
	if rig.provider.callCount() != 0 || rig.storedFingerprint(t, "support").Valid {
		t.Fatal("an unresolved credential sent text or recorded an opt-in")
	}
	if out, err := rig.run(t, "refresh", "support", doc); err != nil || !strings.Contains(out, "unchanged: policy.md") {
		t.Fatalf("refresh with an unresolved embedding credential = %q, %v", out, err)
	}
	if out, err := rig.run(t, "check"); err != nil || !strings.Contains(out, "ok") {
		t.Fatalf("check = %q, %v", out, err)
	}
}

func TestKBEmbedOffErasesVectorsFromDatabaseAndWriteAheadLogWithoutConfiguration(t *testing.T) {
	rig := newEmbedRig(t, 1024, "")
	rig.addDoc(t, "support", "policy.md", twoSectionDocument)
	rig.addDoc(t, "other", "faq.md", "# Hours\nWe open at nine.\n")
	if _, err := rig.run(t, "embed", "support", "--apply"); err != nil {
		t.Fatalf("embed: %v", err)
	}
	var blob []byte
	if err := rig.db.QueryRowContext(t.Context(), `SELECT vector FROM kb_embedding ORDER BY chunk_rowid LIMIT 1`).Scan(&blob); err != nil {
		t.Fatalf("read vector: %v", err)
	}
	markers := [][]byte{blob[:256], blob[3000:3256]}
	walPath := rig.dbPath + "-wal"
	wal, err := os.ReadFile(walPath)
	if err != nil || !bytes.Contains(wal, markers[0]) {
		t.Fatalf("write-ahead log does not hold the vector before the opt-out: %v", err)
	}
	before := rig.generation(t, "support")

	out, err := rig.run(t, "embed", "support", "--off")
	if err != nil || !strings.Contains(out, "dry-run: would delete 2 vectors") || rig.vectors(t) != 2 {
		t.Fatalf("--off dry run = %q, %v, vectors %d", out, err, rig.vectors(t))
	}

	replaceEmbeddingSection(t, rig.cfgPath, "")
	out, err = rig.run(t, "embed", "support", "--off", "--apply")
	if err != nil || !strings.Contains(out, "embedding off: 2 vectors deleted") ||
		!strings.Contains(out, "copies an embedding provider holds under its own terms") || !strings.Contains(out, "until its next retrieval or restart") {
		t.Fatalf("--off --apply = %q, %v", out, err)
	}
	wal, err = os.ReadFile(walPath)
	if err != nil {
		t.Fatalf("read write-ahead log: %v", err)
	}
	database, err := os.ReadFile(rig.dbPath)
	if err != nil {
		t.Fatalf("read database: %v", err)
	}
	for _, marker := range markers {
		if bytes.Contains(wal, marker) || bytes.Contains(database, marker) {
			t.Fatal("vector bytes remain in the database file or the write-ahead log")
		}
	}
	if rig.vectors(t) != 0 || rig.storedFingerprint(t, "support").Valid || rig.generation(t, "support") != before+1 {
		t.Fatalf("after --off: vectors %d stored %+v generation %d", rig.vectors(t), rig.storedFingerprint(t, "support"), rig.generation(t, "support"))
	}
	if countRows(t, rig.db, `SELECT count(*) FROM kb_chunk`) != 3 {
		t.Error("--off removed text chunks")
	}
}

func TestKBRemoveApplyErasesVectorsFromDatabaseAndWriteAheadLog(t *testing.T) {
	rig := newEmbedRig(t, 1024, "")
	doc := rig.addDoc(t, "support", "policy.md", twoSectionDocument)
	rig.addDoc(t, "support", "faq.md", "# Hours\nWe open at nine.\n")
	if _, err := rig.run(t, "embed", "support", "--apply"); err != nil {
		t.Fatalf("embed: %v", err)
	}
	var blob []byte
	if err := rig.db.QueryRowContext(t.Context(),
		`SELECT e.vector FROM kb_embedding e JOIN kb_chunk c ON c.rowid = e.chunk_rowid JOIN kb_source s ON s.id = c.source_id WHERE s.uri = ? LIMIT 1`, doc).Scan(&blob); err != nil {
		t.Fatalf("read vector: %v", err)
	}
	markers := [][]byte{blob[:256], blob[3000:3256]}

	out, err := rig.run(t, "remove", "support", doc, "--apply")
	if err != nil || !strings.Contains(out, "removed: 2 chunks") || !strings.Contains(out, "copies an embedding provider holds") {
		t.Fatalf("remove = %q, %v", out, err)
	}
	wal, _ := os.ReadFile(rig.dbPath + "-wal")
	database, _ := os.ReadFile(rig.dbPath)
	for _, marker := range markers {
		if bytes.Contains(wal, marker) || bytes.Contains(database, marker) {
			t.Fatal("vector bytes remain after the source was removed")
		}
	}
	if rig.vectors(t) != 1 {
		t.Fatalf("vectors = %d, want the other source's vector kept", rig.vectors(t))
	}
}

func TestKBEmbedOffDuringAFirstApplyStopsThatRun(t *testing.T) {
	rig := newEmbedRig(t, 4, "    batch_size: 1\n")
	rig.addDoc(t, "support", "policy.md", twoSectionDocument)
	rig.provider.onRequest = func(n int) {
		if n != 1 {
			return
		}
		if out, err := runKBCommand(t, rig.cfgPath, "embed", "support", "--off", "--apply"); err != nil {
			t.Errorf("--off during the run: %v\n%s", err, out)
		}
	}
	_, err := rig.run(t, "embed", "support", "--apply")
	requireKBCode(t, err, knowledge.ErrorCodeEmbeddingFailed)
	if rig.provider.callCount() != 1 || rig.vectors(t) != 0 || rig.storedFingerprint(t, "support").Valid {
		t.Fatalf("calls %d vectors %d stored %+v; the run must stop and leave nothing", rig.provider.callCount(), rig.vectors(t), rig.storedFingerprint(t, "support"))
	}
}

func TestKBEmbedHandlesOnlyTheNamedCollection(t *testing.T) {
	rig := newEmbedRig(t, 4, "")
	rig.addDoc(t, "support", "policy.md", twoSectionDocument)
	rig.addDoc(t, "other", "faq.md", "# Hours\nWe open at nine.\n")
	if _, err := rig.run(t, "embed", "support", "--apply"); err != nil {
		t.Fatalf("embed: %v", err)
	}
	if rig.provider.inputCount() != 2 || rig.storedFingerprint(t, "other").Valid {
		t.Fatalf("inputs %d, other opted in %v", rig.provider.inputCount(), rig.storedFingerprint(t, "other").Valid)
	}
	rig.provider.reset()
	rig.addDoc(t, "other", "more.md", "# More\nExtra text.\n")
	if rig.provider.callCount() != 0 {
		t.Fatal("a collection that never opted in sent text on add")
	}
}

func TestKnowledgeServiceUsesTheLoopbackClientOnlyForEmbeddings(t *testing.T) {
	rig := newEmbedRig(t, 4, "")
	rig.addDoc(t, "support", "policy.md", twoSectionDocument)
	if _, err := rig.run(t, "embed", "support", "--apply"); err != nil {
		t.Fatalf("embedding reached no loopback server: %v", err)
	}
	client := newKnowledgeHTTPClient(nil)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, strings.Replace(rig.provider.server.URL, "http://", "https://", 1), http.NoBody)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if response, err := client.Do(request); err == nil {
		_ = response.Body.Close()
		t.Fatal("the URL source client reached a loopback server")
	} else {
		requireKBCode(t, err, knowledge.ErrorCodeURLRefused)
	}
}

func TestEmbeddingCredentialIsAnOptionalSecretThatJoinsTheVaultWhenResolved(t *testing.T) {
	cfg := &config.Config{Knowledge: &config.Knowledge{Embedding: &config.KnowledgeEmbedding{CredentialRef: "env://KB_EMBED_VAULT_ENTRY"}}}
	var found *configuredSecret
	for _, entry := range additionalConfiguredSecrets(cfg) {
		if entry.name == "knowledge_embedding_credential" {
			found = &entry
		}
	}
	if found == nil || !found.optional {
		t.Fatalf("embedding credential entry = %+v, want an optional entry", found)
	}
	if _, err := buildStrictSecretVault(cfg); err != nil {
		t.Fatalf("an unresolved embedding credential disabled the vault: %v", err)
	}
	t.Setenv("KB_EMBED_VAULT_ENTRY", "embedding-credential-value")
	vault, err := buildStrictSecretVault(cfg)
	if err != nil || !vault.Contains("a paragraph with embedding-credential-value inside") {
		t.Fatalf("vault = %v, %v; want the credential screened", vault, err)
	}
	if entries := additionalConfiguredSecrets(&config.Config{}); len(entries) != 0 {
		t.Errorf("entries without configuration = %+v", entries)
	}
}
