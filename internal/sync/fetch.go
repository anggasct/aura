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

func isSkillFile(path string) bool {
	return strings.HasPrefix(path, "skills/") && strings.HasSuffix(path, "/SKILL.md")
}

func ValidateFetch(ref, branch string, entries []FetchEntry, contents map[string][]byte, roots []string, validator FetchValidator) (FetchSnapshot, []string, error) {
	if strings.TrimSpace(ref) == "" {
		return FetchSnapshot{}, nil, Errorf(ErrorCodeInvalidArgument, "fetch ref must not be empty")
	}
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
		if actual := digestContent(content); actual != entry.Digest {
			return FetchSnapshot{}, nil, Errorf(ErrorCodeConflict, "fetch content does not match its manifest")
		}
		exportEntries = append(exportEntries, ExportEntry(entry))
	}
	var findings []string
	if validator == nil {
		for _, entry := range ordered {
			if isSkillFile(entry.Path) {
				findings = append(findings, entry.Path+": skill review requires a validator")
			}
		}
	} else {
		for _, entry := range ordered {
			if !isSkillFile(entry.Path) {
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
	return FetchSnapshot{Ref: ref, Branch: branch, Entries: ordered, Digest: digestSnapshot(exportEntries)}, nil, nil
}
