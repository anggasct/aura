package sync

import (
	"testing"
)

type stubFetchValidator struct {
	findings map[string][]string
	proven   map[string]bool
}

func (s *stubFetchValidator) ValidateSkillFile(path string, _ []byte) []string {
	return s.findings[path]
}

func (s *stubFetchValidator) ProvenanceOK(path, _ string) bool {
	if s.proven == nil {
		return true
	}
	ok, known := s.proven[path]
	return !known || ok
}

func TestValidateFetchAccepts(t *testing.T) {
	t.Parallel()
	body := "---\nname: deploy\n---\n# Deploy\n"
	entries := []FetchEntry{
		{Path: "skills/reviewed/deploy/SKILL.md", Size: int64(len(body)), Digest: "digest-1"},
		{Path: "config-templates/aura.yaml", Size: 11, Digest: "digest-2"},
	}
	contents := map[string][]byte{
		"skills/reviewed/deploy/SKILL.md": []byte(body),
		"config-templates/aura.yaml":      []byte("version: 1\n"),
	}
	snapshot, findings, err := ValidateFetch(entries, contents, []string{"skills", "config-templates"}, &stubFetchValidator{})
	if err != nil {
		t.Fatalf("ValidateFetch: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("findings = %v, want none", findings)
	}
	if len(snapshot.Entries) != 2 || snapshot.Digest == "" {
		t.Fatalf("snapshot = %+v, want two entries with a digest", snapshot)
	}
}

func TestValidateFetchQuarantines(t *testing.T) {
	t.Parallel()
	body := "---\nname: deploy\n---\n# Deploy\n"
	entries := []FetchEntry{{Path: "skills/reviewed/deploy/SKILL.md", Size: int64(len(body)), Digest: "digest-1"}}
	contents := map[string][]byte{"skills/reviewed/deploy/SKILL.md": []byte(body)}
	_, findings, err := ValidateFetch(entries, contents, []string{"skills"}, &stubFetchValidator{findings: map[string][]string{"skills/reviewed/deploy/SKILL.md": {"frontmatter_malformed"}}})
	if err != nil {
		t.Fatalf("ValidateFetch: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("findings = %v, want one quarantined skill", findings)
	}
}

func TestValidateFetchRejects(t *testing.T) {
	t.Parallel()
	denied := []FetchEntry{{Path: "skills/state.db", Size: 4, Digest: "digest-1"}}
	if _, _, err := ValidateFetch(denied, map[string][]byte{"skills/state.db": []byte("data")}, []string{"skills"}, nil); err == nil {
		t.Fatal("denied path must fail")
	} else if code, ok := CodeOf(err); !ok || code != ErrorCodePathDenied {
		t.Fatalf("code = %v,%v want sync_path_denied", code, ok)
	}
	duplicated := []FetchEntry{
		{Path: "skills/a.md", Size: 1, Digest: "d1"},
		{Path: "skills/a.md", Size: 1, Digest: "d1"},
	}
	if _, _, err := ValidateFetch(duplicated, map[string][]byte{"skills/a.md": []byte("x")}, []string{"skills"}, nil); err == nil {
		t.Fatal("duplicate path must fail")
	} else if code, ok := CodeOf(err); !ok || code != ErrorCodeConflict {
		t.Fatalf("code = %v,%v want sync_conflict", code, ok)
	}
	mismatched := []FetchEntry{{Path: "skills/a.md", Size: 8, Digest: "d1"}}
	if _, _, err := ValidateFetch(mismatched, map[string][]byte{"skills/a.md": []byte("short")}, []string{"skills"}, nil); err == nil {
		t.Fatal("size mismatch must fail")
	}
}
