package toolsbuiltin

import (
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/store"
	"github.com/anggasct/aura/internal/store/storetest"
	"github.com/anggasct/aura/internal/tools/codeexec"
)

func TestCodeLimitsReadEveryConfiguredBound(t *testing.T) {
	configured := config.Interpreter{
		Timeout:          config.Duration(7 * time.Second),
		MaxSourceBytes:   11,
		MaxStdinBytes:    12,
		MaxStdoutBytes:   13,
		MaxStderrBytes:   14,
		MaxOutputFiles:   15,
		MaxArtifactBytes: 16,
		MaxMemoryBytes:   17,
		MaxProcesses:     18,
		MaxRunDirBytes:   19,
	}
	want := codeexec.Limits{
		Timeout: 7 * time.Second, MaxSourceBytes: 11, MaxStdinBytes: 12, MaxStdoutBytes: 13, MaxStderrBytes: 14,
		MaxMemoryBytes: 17, MaxProcesses: 18, MaxRunDirBytes: 19, MaxFileBytes: 16, MaxOutputFiles: 15,
	}
	if got := codeLimits(&configured); got != want {
		t.Fatalf("codeLimits = %+v, want %+v", got, want)
	}
}

func TestCodeLimitsKeepAFileInsideTheRunDirectory(t *testing.T) {
	configured := config.DefaultInterpreter()
	configured.MaxArtifactBytes = configured.MaxRunDirBytes * 2
	if got := codeLimits(&configured); got.MaxFileBytes != int64(configured.MaxRunDirBytes) {
		t.Fatalf("file limit = %d, want it capped at the run directory limit %d", got.MaxFileBytes, configured.MaxRunDirBytes)
	}
}

func TestNewCodeRunnerRequiresItsOptions(t *testing.T) {
	limits := config.DefaultInterpreter()
	cases := []struct {
		name    string
		options *CodeOptions
	}{
		{"nil options", nil},
		{"missing limits", &CodeOptions{RunRoot: "/run"}},
		{"missing artifact store", &CodeOptions{RunRoot: "/run", Limits: &limits}},
		{"missing store", &CodeOptions{RunRoot: "/run", Limits: &limits, Artifacts: store.NewArtifactStore(nil, "/artifacts", 1)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner, err := NewCodeRunner(tc.options)
			if err == nil || runner != nil {
				t.Fatalf("NewCodeRunner = %v, %v; want an error and no runner", runner, err)
			}
		})
	}
}

func TestCodeArtifactsMapStoreFailuresToCodes(t *testing.T) {
	db := storetest.OpenMigrated(t)
	if err := store.NewSessionService(db).Create(t.Context(), &store.Session{
		ID: "session-1", OwnerID: "owner-1", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	sink := codeArtifacts{store: store.NewArtifactStore(db, t.TempDir(), 8)}
	cases := []struct {
		name     string
		content  string
		session  string
		wantCode codeexec.ErrorCode
		wantOK   bool
	}{
		{"stored", "abc", "session-1", "", true},
		{"over the quota", "abcdefghi", "session-1", codeexec.ErrorCodeArtifactQuota, false},
		{"unknown session", "xyz", "session-missing", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			receipt, err := sink.Put(t.Context(), strings.NewReader(tc.content), &codeexec.ArtifactMeta{
				ID: "art-" + tc.name, SessionID: tc.session, Filename: "a.txt", MediaType: "text/plain",
			})
			if tc.wantOK {
				if err != nil || receipt.ID != "art-"+tc.name || receipt.SizeBytes != int64(len(tc.content)) || receipt.Digest == "" {
					t.Fatalf("Put = %+v, %v", receipt, err)
				}
				return
			}
			code, coded := codeexec.CodeOf(err)
			if err == nil || coded != (tc.wantCode != "") || code != tc.wantCode {
				t.Fatalf("Put error = %v (code %q, coded %v), want code %q", err, code, coded, tc.wantCode)
			}
			if strings.Contains(err.Error(), tc.session) {
				t.Fatalf("the error echoes store detail: %v", err)
			}
		})
	}
}
