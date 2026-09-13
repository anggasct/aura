package skills

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func makeReviewRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "net-tools")
	writePackageFile(t, dir, "SKILL.md", "---\nname: net-tools\ndescription: Network helpers.\ncompatibility: Requires curl\nallowed-tools: fetch\n---\nFetch https://example.com/api for data.\n")
	writePackageFile(t, dir, filepath.Join("scripts", "fetch.sh"), "#!/bin/sh\ncurl \"$1\"\n")
	writePackageFile(t, dir, filepath.Join("references", "api.md"), "# API\nSee https://example.com/docs.\n")
	return root
}

func reviewEngine(t *testing.T, registry Registry, root string) *Engine {
	t.Helper()
	engine, err := NewEngine(registry, &EngineConfig{
		Dirs:                 []string{root},
		MaxIndexed:           16,
		MaxInstructionRunes:  8192,
		MaxResourceBytes:     65536,
		ScriptToolName:       "exec",
		ScriptToolCapability: "shell.execute",
		PolicyVersion:        "1",
	})
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	if err := engine.Refresh(t.Context()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	return engine
}

func quarantinedID(t *testing.T, registry *fakeRegistry) string {
	t.Helper()
	rows, err := registry.ListByState(t.Context(), StateQuarantined, 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("quarantined = %v, %v", rows, err)
	}
	return rows[0].ID
}

func TestReviewBundle(t *testing.T) {
	root := makeReviewRoot(t)
	registry := &fakeRegistry{}
	engine := reviewEngine(t, registry, root)
	bundle, err := engine.Review(t.Context(), quarantinedID(t, registry))
	if err != nil {
		t.Fatalf("review: %v", err)
	}
	if bundle.Name != "net-tools" || bundle.State != StateQuarantined {
		t.Errorf("bundle = %+v", bundle)
	}
	if bundle.Description != "Network helpers." || bundle.Compatibility != "Requires curl" {
		t.Errorf("bundle = %+v", bundle)
	}
	if len(bundle.AllowedTools) != 1 || bundle.AllowedTools[0] != "fetch" {
		t.Errorf("allowed = %v", bundle.AllowedTools)
	}
	if len(bundle.Scripts) != 1 || bundle.Scripts[0] != "scripts/fetch.sh" {
		t.Errorf("scripts = %v", bundle.Scripts)
	}
	if len(bundle.Links) != 2 {
		t.Errorf("links = %v", bundle.Links)
	}
	if len(bundle.Findings) != 0 || bundle.DigestChanged {
		t.Errorf("bundle = %+v", bundle)
	}
	if _, err := engine.Review(t.Context(), "local/missing@0123456789ab"); !isCode(err, ErrorCodeSkillNotFound) {
		t.Errorf("missing err = %v", err)
	}
	if _, err := engine.Review(nilCtxForTest(), bundle.ID); !isCode(err, ErrorCodeInvalidArgument) {
		t.Errorf("nil ctx err = %v", err)
	}
}

func TestAcceptBindsGrants(t *testing.T) {
	root := makeReviewRoot(t)
	registry := &fakeRegistry{}
	engine := reviewEngine(t, registry, root)
	id := quarantinedID(t, registry)
	record, err := registry.Get(t.Context(), id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	event, err := engine.Accept(t.Context(), id, record.Digest, []string{"fetch", "read", "fetch"})
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	if event.Decision != DecisionAccepted || event.SkillID != id || event.PolicyVersion != "1" {
		t.Errorf("event = %+v", event)
	}
	if event.At.IsZero() {
		t.Errorf("event time not set")
	}
	updated, err := registry.Get(t.Context(), id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if updated.State != StateActive {
		t.Errorf("state = %q", updated.State)
	}
	var grants []string
	if err := json.Unmarshal([]byte(updated.Granted), &grants); err != nil {
		t.Fatalf("grants: %v", err)
	}
	if len(grants) != 2 || grants[0] != "fetch" || grants[1] != "read" {
		t.Errorf("grants = %v (want sorted, deduped)", grants)
	}
}

func TestAcceptFailsClosed(t *testing.T) {
	root := makeReviewRoot(t)
	registry := &fakeRegistry{}
	engine := reviewEngine(t, registry, root)
	id := quarantinedID(t, registry)
	record, err := registry.Get(t.Context(), id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	cases := map[string]struct {
		id     string
		digest string
		grants []string
		code   ErrorCode
	}{
		"wrong digest":   {id: id, digest: "0000000000000000000000000000000000000000000000000000000000000000", code: ErrorCodeSkillDigestChanged},
		"missing id":     {id: "local/missing@0123456789ab", digest: record.Digest, code: ErrorCodeSkillNotFound},
		"empty digest":   {id: id, code: ErrorCodeInvalidArgument},
		"bad grant":      {id: id, digest: record.Digest, grants: []string{"Has Space"}, code: ErrorCodeInvalidArgument},
		"empty grant":    {id: id, digest: record.Digest, grants: []string{""}, code: ErrorCodeInvalidArgument},
		"grant too long": {id: id, digest: record.Digest, grants: []string{strings.Repeat("a", maxGrantRunes+1)}, code: ErrorCodeInvalidArgument},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := engine.Accept(t.Context(), tc.id, tc.digest, tc.grants); !isCode(err, tc.code) {
				t.Errorf("err = %v, want %s", err, tc.code)
			}
		})
	}
	if _, err := engine.Accept(t.Context(), id, record.Digest, nil); err != nil {
		t.Fatalf("accept without grants: %v", err)
	}
	if _, err := engine.Accept(t.Context(), id, record.Digest, nil); !isCode(err, ErrorCodeSkillInvalid) {
		t.Errorf("second accept err = %v", err)
	}
}

func TestRejectTransitions(t *testing.T) {
	root := makeReviewRoot(t)
	registry := &fakeRegistry{}
	engine := reviewEngine(t, registry, root)
	id := quarantinedID(t, registry)
	event, err := engine.Reject(t.Context(), id, "too broad")
	if err != nil {
		t.Fatalf("reject: %v", err)
	}
	if event.Decision != DecisionRejected || event.Reason != "too broad" {
		t.Errorf("event = %+v", event)
	}
	updated, err := registry.Get(t.Context(), id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if updated.State != StateRejected {
		t.Errorf("state = %q", updated.State)
	}
	if _, err := engine.Reject(t.Context(), id, ""); !isCode(err, ErrorCodeSkillInvalid) {
		t.Errorf("second reject err = %v", err)
	}
	if _, err := engine.Reject(t.Context(), id, strings.Repeat("a", maxReasonRunes+1)); !isCode(err, ErrorCodeInvalidArgument) {
		t.Errorf("long reason err = %v", err)
	}
}
