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
	tpl := "version: 1\n"
	entries := []FetchEntry{
		{Path: "skills/reviewed/deploy/SKILL.md", Size: int64(len(body)), Digest: digestContent([]byte(body))},
		{Path: "config-templates/aura.yaml", Size: int64(len(tpl)), Digest: digestContent([]byte(tpl))},
	}
	contents := map[string][]byte{
		"skills/reviewed/deploy/SKILL.md": []byte(body),
		"config-templates/aura.yaml":      []byte(tpl),
	}
	snapshot, findings, err := ValidateFetch("ref-abc", "main", entries, contents, []string{"skills", "config-templates"}, &stubFetchValidator{})
	if err != nil {
		t.Fatalf("ValidateFetch: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("findings = %v, want none", findings)
	}
	if len(snapshot.Entries) != 2 || snapshot.Digest == "" {
		t.Fatalf("snapshot = %+v, want two entries with a digest", snapshot)
	}
	if snapshot.Ref != "ref-abc" || snapshot.Branch != "main" {
		t.Fatalf("snapshot ref/branch = %q/%q, want ref-abc/main", snapshot.Ref, snapshot.Branch)
	}
}

func TestValidateFetchQuarantines(t *testing.T) {
	t.Parallel()
	body := "---\nname: deploy\n---\n# Deploy\n"
	entries := []FetchEntry{{Path: "skills/reviewed/deploy/SKILL.md", Size: int64(len(body)), Digest: digestContent([]byte(body))}}
	contents := map[string][]byte{"skills/reviewed/deploy/SKILL.md": []byte(body)}
	_, findings, err := ValidateFetch("ref-abc", "main", entries, contents, []string{"skills"}, &stubFetchValidator{findings: map[string][]string{"skills/reviewed/deploy/SKILL.md": {"frontmatter_malformed"}}})
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
	if _, _, err := ValidateFetch("ref-abc", "main", denied, map[string][]byte{"skills/state.db": []byte("data")}, []string{"skills"}, nil); err == nil {
		t.Fatal("denied path must fail")
	} else if code, ok := CodeOf(err); !ok || code != ErrorCodePathDenied {
		t.Fatalf("code = %v,%v want sync_path_denied", code, ok)
	}
	duplicated := []FetchEntry{
		{Path: "skills/a.md", Size: 1, Digest: digestContent([]byte("x"))},
		{Path: "skills/a.md", Size: 1, Digest: digestContent([]byte("x"))},
	}
	if _, _, err := ValidateFetch("ref-abc", "main", duplicated, map[string][]byte{"skills/a.md": []byte("x")}, []string{"skills"}, nil); err == nil {
		t.Fatal("duplicate path must fail")
	} else if code, ok := CodeOf(err); !ok || code != ErrorCodeConflict {
		t.Fatalf("code = %v,%v want sync_conflict", code, ok)
	}
	mismatched := []FetchEntry{{Path: "skills/a.md", Size: 8, Digest: "d1"}}
	if _, _, err := ValidateFetch("ref-abc", "main", mismatched, map[string][]byte{"skills/a.md": []byte("short")}, []string{"skills"}, nil); err == nil {
		t.Fatal("size mismatch must fail")
	}
}

func TestValidateFetchRejectsDigestMismatch(t *testing.T) {
	t.Parallel()
	body := []byte("version: 1\n")
	entries := []FetchEntry{{Path: "config-templates/aura.yaml", Size: int64(len(body)), Digest: "attacker-digest"}}
	contents := map[string][]byte{"config-templates/aura.yaml": body}
	if _, _, err := ValidateFetch("ref-abc", "main", entries, contents, []string{"config-templates"}, nil); err == nil {
		t.Fatal("digest mismatch must fail")
	} else if code, ok := CodeOf(err); !ok || code != ErrorCodeConflict {
		t.Fatalf("code = %v,%v want sync_conflict", code, ok)
	}
}

func TestValidateFetchRejectsEmptyRef(t *testing.T) {
	t.Parallel()
	body := []byte("version: 1\n")
	entries := []FetchEntry{{Path: "config-templates/aura.yaml", Size: int64(len(body)), Digest: digestContent(body)}}
	contents := map[string][]byte{"config-templates/aura.yaml": body}
	if _, _, err := ValidateFetch("", "main", entries, contents, []string{"config-templates"}, nil); err == nil {
		t.Fatal("empty ref must fail")
	} else if code, ok := CodeOf(err); !ok || code != ErrorCodeInvalidArgument {
		t.Fatalf("code = %v,%v want invalid_argument", code, ok)
	}
	if _, _, err := ValidateFetch("   ", "main", entries, contents, []string{"config-templates"}, nil); err == nil {
		t.Fatal("blank ref must fail")
	}
}

func TestValidateFetchNilValidatorQuarantinesSkills(t *testing.T) {
	t.Parallel()
	body := []byte("---\nname: deploy\n---\n# Deploy\n")
	entries := []FetchEntry{{Path: "skills/unreviewed/evil/SKILL.md", Size: int64(len(body)), Digest: digestContent(body)}}
	contents := map[string][]byte{"skills/unreviewed/evil/SKILL.md": body}
	snapshot, findings, err := ValidateFetch("ref-abc", "main", entries, contents, []string{"skills"}, nil)
	if err != nil {
		return
	}
	if len(findings) == 0 || len(snapshot.Entries) != 0 {
		t.Fatalf("nil validator must quarantine skills: snapshot=%+v findings=%v", snapshot, findings)
	}
}

func TestValidateFetchToPlanPromotion(t *testing.T) {
	t.Parallel()
	tpl := []byte("version: 1\n")
	entries := []FetchEntry{{Path: "config-templates/aura.yaml", Size: int64(len(tpl)), Digest: digestContent(tpl)}}
	contents := map[string][]byte{"config-templates/aura.yaml": tpl}
	snapshot, findings, err := ValidateFetch("ref-abc", "main", entries, contents, []string{"config-templates"}, nil)
	if err != nil {
		t.Fatalf("ValidateFetch: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("findings = %v, want none", findings)
	}
	plan, _, err := PlanPromotion(snapshot, AdvanceFastForward, nil)
	if err != nil {
		t.Fatalf("PlanPromotion on fetch output: %v", err)
	}
	if len(plan.Entries) != 1 {
		t.Fatalf("plan = %+v, want one entry", plan)
	}
}
