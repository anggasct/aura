package plugin

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/plugin/auth"
)

const baseManifest = `api_version: aura.plugin/v1
id: weather
version: 1.0.0
name: Weather
description: Weather forecasts.
capabilities: [network]
tools:
  - name: forecast
    description: Get a forecast for a city.
    tier: read
  - name: alerts
    description: List active alerts.
    tier: read
egress: [api.weather.example]
secrets: []
auth:
  kind: none
backend:
  kind: mcp
  transport: stdio
  command: ./server
  args: [--quiet]
`

type memoryRegistry struct {
	mu       sync.Mutex
	plugins  map[string]Record
	versions map[string]VersionRecord
	grants   map[string][]GrantRecord
}

func newMemoryRegistry() *memoryRegistry {
	return &memoryRegistry{plugins: map[string]Record{}, versions: map[string]VersionRecord{}, grants: map[string][]GrantRecord{}}
}

func versionKey(id, version string) string { return id + "@" + version }

func (r *memoryRegistry) Install(_ context.Context, in *InstallRecord) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := versionKey(in.Version.ID, in.Version.Version)
	if _, exists := r.versions[key]; exists {
		return Errorf(ErrVersionConflict, "version exists")
	}
	r.versions[key] = in.Version
	r.plugins[in.Plugin.ID] = in.Plugin
	r.grants[in.Plugin.ID] = slices.DeleteFunc(slices.Clone(r.grants[in.Plugin.ID]), func(g GrantRecord) bool {
		return slices.Contains(in.DropGrants, g.GrantKey)
	})
	return nil
}

func (r *memoryRegistry) Approve(_ context.Context, in *ApprovalRecord) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	record, ok := r.plugins[in.ID]
	if !ok {
		return Errorf(ErrNotFound, "missing")
	}
	row := r.versions[versionKey(in.ID, in.Version)]
	if record.Version != in.Version || row.Digest != in.Digest {
		return Errorf(ErrDigestMismatch, "changed")
	}
	record.State = StateActive
	record.ApprovedVersion = in.Version
	record.UpdatedAt = in.At
	r.plugins[in.ID] = record
	r.grants[in.ID] = slices.Clone(in.Grants)
	return nil
}

func (r *memoryRegistry) SetState(_ context.Context, id, state string, at time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	record, ok := r.plugins[id]
	if !ok {
		return Errorf(ErrNotFound, "missing")
	}
	record.State = state
	record.UpdatedAt = at
	r.plugins[id] = record
	return nil
}

func (r *memoryRegistry) Get(_ context.Context, id string) (Record, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	record, ok := r.plugins[id]
	if !ok {
		return Record{}, Errorf(ErrNotFound, "missing")
	}
	return record, nil
}

func (r *memoryRegistry) List(context.Context) ([]Record, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Record
	for _, record := range r.plugins {
		out = append(out, record)
	}
	slices.SortFunc(out, func(a, b Record) int { return strings.Compare(a.ID, b.ID) })
	return out, nil
}

func (r *memoryRegistry) Version(_ context.Context, id, version string) (VersionRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	row, ok := r.versions[versionKey(id, version)]
	if !ok {
		return VersionRecord{}, Errorf(ErrNotFound, "missing")
	}
	return row, nil
}

func (r *memoryRegistry) Grants(_ context.Context, id string) ([]GrantRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.grants[id]), nil
}

func (r *memoryRegistry) Remove(_ context.Context, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.plugins[id]; !ok {
		return Errorf(ErrNotFound, "missing")
	}
	delete(r.plugins, id)
	delete(r.grants, id)
	for key := range r.versions {
		if strings.HasPrefix(key, id+"@") {
			delete(r.versions, key)
		}
	}
	return nil
}

type fakeSecrets struct {
	mu     sync.Mutex
	values map[string]string
}

func (f *fakeSecrets) ResolveSecret(_ context.Context, ref string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	value, ok := f.values[ref]
	if !ok {
		return "", Errorf(ErrInvalidArgument, "unknown ref")
	}
	return value, nil
}

func (f *fakeSecrets) set(ref, value string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.values[ref] = value
}

type recordingRedactor struct {
	mu     sync.Mutex
	values []string
}

func (r *recordingRedactor) RegisterSecret(value string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.values = append(r.values, value)
}

func (r *recordingRedactor) has(value string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Contains(r.values, value)
}

type testEnv struct {
	t        *testing.T
	registry *memoryRegistry
	service  *Service
	dir      string
	authDir  string
	secrets  *fakeSecrets
	redactor *recordingRedactor
	clock    time.Time
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	root := t.TempDir()
	env := &testEnv{
		t: t, registry: newMemoryRegistry(), dir: filepath.Join(root, "plugins"), authDir: filepath.Join(root, "plugin-secrets"),
		secrets: &fakeSecrets{values: map[string]string{}}, redactor: &recordingRedactor{}, clock: time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC),
	}
	authRegistry, err := auth.NewRegistry(&auth.Options{Dir: env.authDir, Secrets: env.secrets, HTTPClient: http.DefaultClient, Redactor: env.redactor})
	if err != nil {
		t.Fatalf("auth.NewRegistry: %v", err)
	}
	service, err := NewService(&Options{Registry: env.registry, Auth: authRegistry, Dir: env.dir, Now: func() time.Time { return env.clock }})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	env.service = service
	return env
}

func writeBundle(t *testing.T, manifest string, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	if manifest != "" {
		writeFile(t, filepath.Join(dir, ManifestFile), manifest, 0o600)
	}
	for name, content := range files {
		mode := os.FileMode(0o600)
		if name == "server" {
			mode = 0o700
		}
		writeFile(t, filepath.Join(dir, name), content, mode)
	}
	return dir
}

func writeFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatalf("write %s: %v", filepath.Base(path), err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("chmod: %v", err)
	}
}

func (e *testEnv) install(manifest string, files map[string]string) *InstallResult {
	e.t.Helper()
	result, err := e.service.Install(e.t.Context(), writeBundle(e.t, manifest, files))
	if err != nil {
		e.t.Fatalf("Install: %v", err)
	}
	return result
}

func (e *testEnv) approve(id string, result *InstallResult) {
	e.t.Helper()
	if _, err := e.service.Approve(e.t.Context(), id, result.Digest[:MinDigestPrefix]); err != nil {
		e.t.Fatalf("Approve: %v", err)
	}
}

func hasCode(err error, want ErrorCode) bool {
	code, ok := CodeOf(err)
	return ok && code == want
}

func replaceOnce(t *testing.T, text, old, replacement string) string {
	t.Helper()
	if strings.Count(text, old) != 1 {
		t.Fatalf("manifest fixture does not contain %q exactly once", old)
	}
	return strings.Replace(text, old, replacement, 1)
}

func serverFiles() map[string]string {
	return map[string]string{"server": "#!/bin/sh\nexit 0\n"}
}
