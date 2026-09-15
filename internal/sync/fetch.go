package sync

import (
	"slices"
	"strings"
)

type FetchEntry struct {
	Path   string
	Size   int64
	Digest string
}

type FetchSnapshot struct {
	Ref     string
	Branch  string
	Entries []FetchEntry
	Digest  string
}

type FetchValidator interface {
	ValidateSkillFile(path string, content []byte) []string
	ProvenanceOK(path, digest string) bool
}

func ValidateFetch(entries []FetchEntry, contents map[string][]byte, roots []string, validator FetchValidator) (FetchSnapshot, []string, error) {
	ordered := slices.Clone(entries)
	slices.SortFunc(ordered, func(a, b FetchEntry) int { return strings.Compare(a.Path, b.Path) })
	seen := make(map[string]struct{}, len(ordered))
	exportEntries := make([]ExportEntry, 0, len(ordered))
	for _, entry := range ordered {
		if _, dup := seen[entry.Path]; dup {
			return FetchSnapshot{}, nil, Errorf(ErrorCodeConflict, "fetch carries a duplicate path")
		}
		seen[entry.Path] = struct{}{}
		if err := FilterExportable(roots, entry.Path, entry.Size); err != nil {
			return FetchSnapshot{}, nil, err
		}
		content, ok := contents[entry.Path]
		if !ok {
			return FetchSnapshot{}, nil, Errorf(ErrorCodeConflict, "fetch is missing staged content")
		}
		if int64(len(content)) != entry.Size {
			return FetchSnapshot{}, nil, Errorf(ErrorCodeConflict, "fetch content does not match its manifest")
		}
		exportEntries = append(exportEntries, ExportEntry(entry))
	}
	var findings []string
	if validator != nil {
		for _, entry := range ordered {
			if !strings.HasPrefix(entry.Path, "skills/") || !strings.HasSuffix(entry.Path, "/SKILL.md") {
				continue
			}
			if fileFindings := validator.ValidateSkillFile(entry.Path, contents[entry.Path]); len(fileFindings) > 0 {
				findings = append(findings, entry.Path+": "+strings.Join(fileFindings, ","))
				continue
			}
			if !validator.ProvenanceOK(entry.Path, entry.Digest) {
				findings = append(findings, entry.Path+": provenance rejected")
			}
		}
	}
	if len(findings) > 0 {
		slices.Sort(findings)
		return FetchSnapshot{}, findings, nil
	}
	return FetchSnapshot{Entries: ordered, Digest: digestSnapshot(exportEntries)}, nil, nil
}
