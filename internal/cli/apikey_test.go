package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	auraagent "github.com/anggasct/aura/internal/agent"
	"github.com/anggasct/aura/internal/channel/httpchat"
	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/health"
	"github.com/anggasct/aura/internal/store"
)

const apiKeyTestWait = 30 * time.Second

var apiKeyShape = regexp.MustCompile(`^aura_sk_[a-z2-7]{12}_[a-z2-7]{52}$`)

func runAPIKey(t *testing.T, cfg string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	cmd := newAPIKeyCmd(&globalFlags{configPath: cfg})
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	err = cmd.ExecuteContext(t.Context())
	return out.String(), errOut.String(), err
}

func createTestAPIKey(t *testing.T, cfg string) string {
	t.Helper()
	stdout, _, err := runAPIKey(t, cfg, "create", "--persona", "main", "--class", "trusted", "--label", "laptop")
	if err != nil {
		t.Fatalf("apikey create: %v", err)
	}
	return strings.TrimSpace(stdout)
}

func TestAPIKeyCreatePrintsOnceAndStoresOnlyTheHash(t *testing.T) {
	cfg := writePrincipalCLIConfig(t)
	stdout, stderr, err := runAPIKey(t, cfg, "create", "--persona", "main", "--class", "trusted", "--label", "  laptop  ")
	if err != nil {
		t.Fatalf("apikey create: %v", err)
	}
	plaintext := strings.TrimSuffix(stdout, "\n")
	if !apiKeyShape.MatchString(plaintext) {
		t.Fatalf("stdout = %q, want exactly the key", stdout)
	}
	id, err := httpchat.ParseKey(plaintext)
	if err != nil {
		t.Fatalf("ParseKey: %v", err)
	}
	secret := plaintext[len(httpchat.KeyPrefix)+httpchat.KeyIDLength+1:]
	if !strings.Contains(stderr, id) || strings.Contains(stderr, secret) {
		t.Fatalf("stderr = %q, want the key id without the secret", stderr)
	}

	db := openAccessDB(t, cfg)
	key, err := store.NewAPIKeyStore(db).GetByID(t.Context(), id)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if key.KeySHA256 != httpchat.KeyDigest(plaintext) || key.PersonaID != "main" || key.Class != store.APIKeyClassTrusted || key.Label != "laptop" || !key.Active() {
		t.Fatalf("stored key = %+v", key)
	}
	var stored string
	if err := db.QueryRowContext(t.Context(), `SELECT id || '|' || key_sha256 || '|' || persona_id || '|' || class || '|' || label || '|' || created_at || '|' || COALESCE(revoked_at, '') FROM api_key`).Scan(&stored); err != nil {
		t.Fatalf("read api_key row: %v", err)
	}
	if strings.Contains(stored, secret) || strings.Contains(stored, plaintext) {
		t.Fatalf("api_key row holds key material: %q", stored)
	}
	var payload string
	if err := db.QueryRowContext(t.Context(), `SELECT payload_json FROM runtime_event WHERE kind = ? AND json_extract(payload_json, '$.action') = ?`, store.AccessOwnerActionEventKind, accessActionAPIKeyCreate).Scan(&payload); err != nil {
		t.Fatalf("read create audit event: %v", err)
	}
	if strings.Contains(payload, secret) || !strings.Contains(payload, `"principal_id":"http:`+id+`"`) || !strings.Contains(payload, `"agent_id":"main"`) {
		t.Fatalf("audit payload = %s", payload)
	}
}

func TestAPIKeyCreateMintsDistinctKeys(t *testing.T) {
	cfg := writePrincipalCLIConfig(t)
	first := createTestAPIKey(t, cfg)
	second := createTestAPIKey(t, cfg)
	if first == second {
		t.Fatal("two creates printed the same key")
	}
}

func TestAPIKeyCreateOwnerNeedsConfirmation(t *testing.T) {
	cfg := writePrincipalCLIConfig(t)
	_, _, err := runAPIKey(t, cfg, "create", "--persona", "main", "--class", "owner", "--label", "ops")
	var usage *usageError
	if !errors.As(err, &usage) {
		t.Fatalf("create owner without confirmation = %v, want a usage error", err)
	}
	if keys, listErr := store.NewAPIKeyStore(openAccessDB(t, cfg)).List(t.Context()); listErr != nil || len(keys) != 0 {
		t.Fatalf("keys after refused create = %v, %v", keys, listErr)
	}
	stdout, _, err := runAPIKey(t, cfg, "create", "--persona", "main", "--class", "owner", "--label", "ops", "--confirm-owner")
	if err != nil || !apiKeyShape.MatchString(strings.TrimSpace(stdout)) {
		t.Fatalf("create owner with confirmation = %q, %v", stdout, err)
	}
	keys, err := store.NewAPIKeyStore(openAccessDB(t, cfg)).List(t.Context())
	if err != nil || len(keys) != 1 || keys[0].Class != store.APIKeyClassOwner {
		t.Fatalf("keys = %+v, %v; want one owner key", keys, err)
	}
}

func TestAPIKeyCreateUsageErrors(t *testing.T) {
	cases := map[string][]string{
		"missing persona":             {"create", "--class", "trusted", "--label", "x"},
		"missing label":               {"create", "--persona", "main", "--class", "trusted"},
		"blank label":                 {"create", "--persona", "main", "--class", "trusted", "--label", "   "},
		"missing class":               {"create", "--persona", "main", "--label", "x"},
		"unknown class":               {"create", "--persona", "main", "--class", "admin", "--label", "x"},
		"label with newline":          {"create", "--persona", "main", "--class", "trusted", "--label", "a\nb"},
		"label with tab":              {"create", "--persona", "main", "--class", "trusted", "--label", "a\tb"},
		"label too long":              {"create", "--persona", "main", "--class", "trusted", "--label", strings.Repeat("x", apiKeyLabelMaxLength+1)},
		"confirm owner on public key": {"create", "--persona", "main", "--class", "public", "--label", "x", "--confirm-owner"},
		"positional argument":         {"create", "--persona", "main", "--class", "trusted", "--label", "x", "extra"},
	}
	cfg := writePrincipalCLIConfig(t)
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			_, _, err := runAPIKey(t, cfg, args...)
			if err == nil {
				t.Fatal("create accepted invalid arguments")
			}
			var usage *usageError
			if !errors.As(err, &usage) {
				t.Fatalf("error = %v, want a usage error", err)
			}
		})
	}
	if keys, err := store.NewAPIKeyStore(openAccessDB(t, cfg)).List(t.Context()); err != nil || len(keys) != 0 {
		t.Fatalf("keys after refused creates = %v, %v", keys, err)
	}
}

func TestAPIKeyCreateUnknownPersonaFailsWithStableCode(t *testing.T) {
	cfg := writePrincipalCLIConfig(t)
	_, _, err := runAPIKey(t, cfg, "create", "--persona", "ghost", "--class", "trusted", "--label", "x")
	code, ok := auraagent.CodeOf(err)
	if !ok || code != auraagent.ErrorCodeNotFound {
		t.Fatalf("error = %v, want agent_not_found", err)
	}
	if keys, listErr := store.NewAPIKeyStore(openAccessDB(t, cfg)).List(t.Context()); listErr != nil || len(keys) != 0 {
		t.Fatalf("keys after unknown persona = %v, %v", keys, listErr)
	}
}

func TestAPIKeyExitCodes(t *testing.T) {
	cfg := writePrincipalCLIConfig(t)
	cases := map[string]struct {
		args []string
		want int
	}{
		"owner without confirmation": {args: []string{"apikey", "create", "--persona", "main", "--class", "owner", "--label", "x"}, want: 2},
		"unknown persona":            {args: []string{"apikey", "create", "--persona", "ghost", "--class", "trusted", "--label", "x"}, want: 1},
		"unknown key on revoke":      {args: []string{"apikey", "revoke", "zzzzzzzzzzzz"}, want: 1},
		"revoke without id":          {args: []string{"apikey", "revoke"}, want: 2},
		"revoke with two ids":        {args: []string{"apikey", "revoke", "a", "b"}, want: 2},
		"unknown flag":               {args: []string{"apikey", "create", "--bogus"}, want: 2},
		"list succeeds":              {args: []string{"apikey", "list"}, want: 0},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			args := append([]string{"--config", cfg}, tc.args...)
			if code := ExecuteContext(t.Context(), args...); code != tc.want {
				t.Fatalf("exit code = %d, want %d", code, tc.want)
			}
		})
	}
}

func TestAPIKeyListShowsColumnsWithoutSecrets(t *testing.T) {
	cfg := writePrincipalCLIConfig(t)
	stdout, _, err := runAPIKey(t, cfg, "list")
	if err != nil || strings.TrimSpace(stdout) != "no api keys" {
		t.Fatalf("empty list = %q, %v", stdout, err)
	}
	active := createTestAPIKey(t, cfg)
	revoked := createTestAPIKey(t, cfg)
	revokedID, err := httpchat.ParseKey(revoked)
	if err != nil {
		t.Fatalf("ParseKey: %v", err)
	}
	if _, _, err := runAPIKey(t, cfg, "revoke", revokedID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	stdout, _, err = runAPIKey(t, cfg, "list")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) != 3 || lines[0] != "ID\tPERSONA\tCLASS\tLABEL\tCREATED\tREVOKED" {
		t.Fatalf("list output:\n%s", stdout)
	}
	activeID, err := httpchat.ParseKey(active)
	if err != nil {
		t.Fatalf("ParseKey: %v", err)
	}
	rows := map[string][]string{}
	for _, line := range lines[1:] {
		fields := strings.Split(line, "\t")
		if len(fields) != 6 {
			t.Fatalf("row %q has %d columns, want 6", line, len(fields))
		}
		rows[fields[0]] = fields
	}
	if row := rows[activeID]; len(row) != 6 || row[1] != "main" || row[2] != "trusted" || row[3] != "laptop" || row[5] != "-" {
		t.Errorf("active row = %v", row)
	} else if _, err := time.Parse(time.RFC3339, row[4]); err != nil {
		t.Errorf("created column %q is not RFC3339", row[4])
	}
	if row := rows[revokedID]; row == nil {
		t.Errorf("revoked row missing from %v", rows)
	} else if _, err := time.Parse(time.RFC3339, row[5]); err != nil {
		t.Errorf("revoked column %q is not RFC3339", row[5])
	}
	for _, key := range []string{active, revoked} {
		if strings.Contains(stdout, key[len(httpchat.KeyPrefix)+httpchat.KeyIDLength+1:]) {
			t.Fatalf("list output leaks a key secret:\n%s", stdout)
		}
	}
}

func TestAPIKeyRevokeIsIdempotentAndAudited(t *testing.T) {
	cfg := writePrincipalCLIConfig(t)
	id, err := httpchat.ParseKey(createTestAPIKey(t, cfg))
	if err != nil {
		t.Fatalf("ParseKey: %v", err)
	}
	stdout, _, err := runAPIKey(t, cfg, "revoke", id)
	if err != nil || strings.TrimSpace(stdout) != "revoked "+id {
		t.Fatalf("first revoke = %q, %v", stdout, err)
	}
	first, err := store.NewAPIKeyStore(openAccessDB(t, cfg)).GetByID(t.Context(), id)
	if err != nil || first.Active() {
		t.Fatalf("key after revoke = %+v, %v", first, err)
	}
	stdout, _, err = runAPIKey(t, cfg, "revoke", id)
	if err != nil || strings.TrimSpace(stdout) != "already revoked "+id {
		t.Fatalf("second revoke = %q, %v", stdout, err)
	}
	second, err := store.NewAPIKeyStore(openAccessDB(t, cfg)).GetByID(t.Context(), id)
	if err != nil || !second.RevokedAt.Equal(first.RevokedAt) {
		t.Fatalf("second revoke moved the timestamp: %v vs %v (%v)", second.RevokedAt, first.RevokedAt, err)
	}
	if got := accessEventCount(t, cfg, accessActionAPIKeyRevoke); got != 1 {
		t.Fatalf("revoke audit events = %d, want 1", got)
	}
	if got := accessEventCount(t, cfg, accessActionAPIKeyCreate); got != 1 {
		t.Fatalf("create audit events = %d, want 1", got)
	}
}

func TestAPIKeyRevokeUnknownKeyFailsWithStableCode(t *testing.T) {
	cfg := writePrincipalCLIConfig(t)
	_, _, err := runAPIKey(t, cfg, "revoke", "zzzzzzzzzzzz")
	if code, ok := store.CodeOf(err); !ok || code != store.ErrorCodeAPIKeyNotFound {
		t.Fatalf("error = %v, want apikey_not_found", err)
	}
	if got := accessEventCount(t, cfg, accessActionAPIKeyRevoke); got != 0 {
		t.Fatalf("revoke audit events = %d, want 0", got)
	}
}

func writeHTTPChatConfig(t *testing.T, enabled bool, listen string) string {
	t.Helper()
	dataRoot := t.TempDir()
	path := filepath.Join(dataRoot, "config.yaml")
	content := "version: 1\nstorage:\n  path: " + dataRoot + "\ntools:\n  workspace: " + dataRoot +
		"\nchannels:\n  http:\n    enabled: " + strconv.FormatBool(enabled) + "\n    listen_address: " + listen + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	seedConfiguredStorage(t, path)
	return path
}

func freeLoopbackAddress(t *testing.T) string {
	t.Helper()
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return address
}

type bindSignal struct {
	slog.Handler
	seen chan struct{}
}

func (h *bindSignal) Enabled(context.Context, slog.Level) bool { return true }

func (h *bindSignal) Handle(ctx context.Context, record slog.Record) error {
	if record.Message == "http listener is not bindable" {
		close(h.seen)
	}
	return h.Handler.Handle(ctx, record)
}

func TestHTTPChatListenerBindsOnlyWhenEnabled(t *testing.T) {
	address := freeLoopbackAddress(t)
	disabled := writeHTTPChatConfig(t, false, address)
	db := openAccessDB(t, disabled)
	server, checks := buildTestHTTPChat(t, disabled, db, slog.New(slog.DiscardHandler))
	if server != nil || len(checks) != 0 {
		t.Fatalf("buildHTTPChat(disabled) = %v, %v; want nothing", server, checks)
	}
	if conn, err := (&net.Dialer{Timeout: time.Second}).DialContext(t.Context(), "tcp", address); err == nil {
		_ = conn.Close()
		t.Fatal("a disabled channel left its address bound")
	}
}

func TestHTTPChatListenerServesModelsForActiveKeysOnly(t *testing.T) {
	address := freeLoopbackAddress(t)
	path := writeHTTPChatConfig(t, true, address)
	active := createTestAPIKey(t, path)
	revokedKey := createTestAPIKey(t, path)
	revokedID, err := httpchat.ParseKey(revokedKey)
	if err != nil {
		t.Fatalf("ParseKey: %v", err)
	}
	if _, _, err := runAPIKey(t, path, "revoke", revokedID); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	db := openAccessDB(t, path)
	server, checks := buildTestHTTPChat(t, path, db, slog.New(slog.DiscardHandler))
	if server == nil {
		t.Fatal("buildHTTPChat(enabled) built no server")
	}
	if len(checks) != 1 || checks[0].ID != "http" || checks[0].Remediation != httpchat.RemediationReviewListener {
		t.Fatalf("checks = %+v", checks)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- server.Start(ctx) }()

	status := func(bearer string) (int, string) {
		deadline := time.Now().Add(apiKeyTestWait)
		for {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+address+"/v1/models", http.NoBody)
			if err != nil {
				t.Fatalf("NewRequest: %v", err)
			}
			req.Header.Set("Authorization", "Bearer "+bearer)
			resp, err := (&http.Client{Timeout: apiKeyTestWait}).Do(req)
			if err == nil {
				body, readErr := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if readErr != nil {
					t.Fatalf("read body: %v", readErr)
				}
				return resp.StatusCode, string(body)
			}
			if time.Now().After(deadline) {
				t.Fatalf("listener never answered: %v", err)
			}
			select {
			case <-time.After(10 * time.Millisecond):
			case <-ctx.Done():
				t.Fatal("test context ended before the listener answered")
			}
		}
	}
	if code, body := status(active); code != http.StatusOK || !strings.Contains(body, `"id":"main"`) {
		t.Fatalf("active key = %d %s", code, body)
	}
	if code, _ := status(revokedKey); code != http.StatusUnauthorized {
		t.Fatalf("revoked key status = %d, want 401", code)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
	case <-time.After(apiKeyTestWait):
		t.Fatal("listener did not stop")
	}
}

func TestHTTPChatBindFailureSurfacesAsHealthFinding(t *testing.T) {
	occupied, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer func() { _ = occupied.Close() }()
	path := writeHTTPChatConfig(t, true, occupied.Addr().String())
	db := openAccessDB(t, path)
	seen := make(chan struct{})
	logger := slog.New(&bindSignal{Handler: slog.DiscardHandler, seen: seen})
	server, checks := buildTestHTTPChat(t, path, db, logger)
	if server == nil {
		t.Fatal("buildHTTPChat built no server")
	}
	registry, err := health.NewRegistry(checks...)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if findings := registry.Evaluate(t.Context()); len(findings) != 0 {
		t.Fatalf("findings before start = %+v", findings)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- server.Start(ctx) }()
	select {
	case <-seen:
	case <-time.After(apiKeyTestWait):
		t.Fatal("bind failure was not reported")
	}
	findings := registry.Evaluate(ctx)
	if len(findings) != 1 || findings[0].Code != httpchat.FindingListenerDown || findings[0].ID != "http/http_listener_down" || findings[0].Status != health.StatusDown {
		t.Fatalf("findings = %+v", findings)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Start: %v", err)
	}
}

func mustLoadConfig(t *testing.T, path string) *config.Config {
	t.Helper()
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return loaded.Config
}
