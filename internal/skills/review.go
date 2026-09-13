package skills

import (
	"context"
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	DecisionAccepted = "accepted"
	DecisionRejected = "rejected"
)

const (
	maxGrantRunes   = 128
	maxReasonRunes  = 500
	maxReviewLinks  = 64
	maxLinkRunes    = 2048
	maxLinkScanKiB  = 32
	maxLinkScanFile = 8
)

var validGrantPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._:-]{0,127}$`)

var linkPattern = regexp.MustCompile(`https?://[^\s)'">\]]+`)

type ReviewBundle struct {
	ID            string
	Name          string
	Description   string
	Origin        string
	Digest        string
	State         string
	Compatibility string
	Requested     []string
	AllowedTools  []string
	Granted       []string
	Scripts       []string
	Files         []string
	Links         []string
	Findings      []string
	DigestChanged bool
}

type AuditEvent struct {
	SkillID       string
	Digest        string
	Decision      string
	Reason        string
	PolicyVersion string
	At            time.Time
}

type localOrigin struct {
	Kind  string `json:"kind"`
	Scope string `json:"scope"`
	Root  string `json:"root"`
}

func parseLocalOrigin(raw string) (localOrigin, error) {
	var origin localOrigin
	if err := json.Unmarshal([]byte(raw), &origin); err != nil {
		return localOrigin{}, Errorf(ErrorCodeSkillInvalid, "skill origin is not valid")
	}
	if origin.Kind != "local" || !validScope(origin.Scope) || origin.Root == "" {
		return localOrigin{}, Errorf(ErrorCodeSkillInvalid, "skill origin is not supported")
	}
	return origin, nil
}

func (e *Engine) Review(ctx context.Context, id string) (ReviewBundle, error) {
	if ctx == nil {
		return ReviewBundle{}, errNilArgument("ctx")
	}
	if strings.TrimSpace(id) == "" {
		return ReviewBundle{}, Errorf(ErrorCodeInvalidArgument, "skill id must not be empty")
	}
	record, err := e.registry.Get(ctx, id)
	if err != nil {
		return ReviewBundle{}, err
	}
	origin, err := parseLocalOrigin(record.Origin)
	if err != nil {
		return ReviewBundle{}, err
	}
	result := ScanDir(ctx, origin.Root, origin.Scope)
	if result.Err != nil {
		return ReviewBundle{}, result.Err
	}
	bundle := ReviewBundle{
		ID:     record.ID,
		Name:   record.Name,
		Origin: record.Origin,
		State:  record.State,
	}
	requested, err := decodeCapabilities(record.Requested)
	if err != nil {
		return ReviewBundle{}, err
	}
	bundle.Requested = requested
	granted, err := decodeCapabilities(record.Granted)
	if err != nil {
		return ReviewBundle{}, err
	}
	bundle.Granted = granted
	if len(result.Findings) > 0 {
		bundle.Findings = result.Findings
	}
	scanned := result.Scanned
	if scanned == nil {
		bundle.Digest = record.Digest
		return bundle, nil
	}
	bundle.Digest = scanned.Digest
	bundle.DigestChanged = scanned.Digest != record.Digest
	for _, file := range scanned.Files {
		bundle.Files = append(bundle.Files, file.Path)
		if strings.HasPrefix(file.Path, "scripts/") {
			bundle.Scripts = append(bundle.Scripts, file.Path)
		}
	}
	slices.Sort(bundle.Files)
	slices.Sort(bundle.Scripts)
	if scanned.Manifest == nil {
		return bundle, nil
	}
	bundle.Description = scanned.Manifest.Description
	bundle.Compatibility = scanned.Manifest.Compatibility
	bundle.AllowedTools = append([]string(nil), scanned.Manifest.AllowedTools...)
	bundle.Links = extractLinks(scanned)
	return bundle, nil
}

func extractLinks(scanned *ScannedDir) []string {
	links := make([]string, 0, 8)
	seen := make(map[string]bool)
	add := func(text string) {
		for _, link := range linkPattern.FindAllString(text, maxReviewLinks) {
			if len([]rune(link)) > maxLinkRunes || seen[link] {
				continue
			}
			seen[link] = true
			links = append(links, link)
			if len(links) >= maxReviewLinks {
				return
			}
		}
	}
	if scanned.Manifest != nil {
		add(scanned.Manifest.Body)
	}
	scannedFiles := 0
	for path, content := range scanned.Contents {
		if !strings.HasSuffix(path, ".md") || path == "SKILL.md" {
			continue
		}
		if scannedFiles >= maxLinkScanFile {
			break
		}
		scannedFiles++
		if len(content) > maxLinkScanKiB<<10 {
			content = content[:maxLinkScanKiB<<10]
		}
		if utf8.Valid(content) {
			add(string(content))
		}
	}
	slices.Sort(links)
	return links
}

func (e *Engine) Accept(ctx context.Context, id, expectedDigest string, grants []string) (AuditEvent, error) {
	if ctx == nil {
		return AuditEvent{}, errNilArgument("ctx")
	}
	if strings.TrimSpace(id) == "" || strings.TrimSpace(expectedDigest) == "" {
		return AuditEvent{}, Errorf(ErrorCodeInvalidArgument, "skill id and digest must not be empty")
	}
	granted, err := normalizeGrants(grants)
	if err != nil {
		return AuditEvent{}, err
	}
	record, err := e.registry.Get(ctx, id)
	if err != nil {
		return AuditEvent{}, err
	}
	if record.State != StateQuarantined {
		return AuditEvent{}, Errorf(ErrorCodeSkillInvalid, "skill is not reviewable")
	}
	if record.Digest != expectedDigest {
		return AuditEvent{}, Errorf(ErrorCodeSkillDigestChanged, "skill content changed since review")
	}
	encoded, err := json.Marshal(granted)
	if err != nil {
		return AuditEvent{}, codedError(ErrorCodeSkillInvalid, "encode skill grants", err)
	}
	if err := e.registry.AcceptSkill(ctx, id, expectedDigest, string(encoded)); err != nil {
		return AuditEvent{}, err
	}
	return e.audit(ctx, id, expectedDigest, DecisionAccepted, ""), nil
}

func (e *Engine) Reject(ctx context.Context, id, reason string) (AuditEvent, error) {
	if ctx == nil {
		return AuditEvent{}, errNilArgument("ctx")
	}
	if strings.TrimSpace(id) == "" {
		return AuditEvent{}, Errorf(ErrorCodeInvalidArgument, "skill id must not be empty")
	}
	if len([]rune(reason)) > maxReasonRunes {
		return AuditEvent{}, Errorf(ErrorCodeInvalidArgument, "rejection reason is too long")
	}
	record, err := e.registry.Get(ctx, id)
	if err != nil {
		return AuditEvent{}, err
	}
	if record.State != StateQuarantined {
		return AuditEvent{}, Errorf(ErrorCodeSkillInvalid, "skill is not reviewable")
	}
	if err := e.registry.RejectSkill(ctx, id); err != nil {
		return AuditEvent{}, err
	}
	return e.audit(ctx, id, record.Digest, DecisionRejected, reason), nil
}

func (e *Engine) audit(ctx context.Context, id, digest, decision, reason string) AuditEvent {
	event := AuditEvent{
		SkillID:       id,
		Digest:        digest,
		Decision:      decision,
		Reason:        reason,
		PolicyVersion: e.policy,
		At:            time.Now().UTC(),
	}
	e.logger.InfoContext(ctx, "skill review decision",
		"component", "skills",
		"skill_id", id,
		"digest", digest,
		"decision", decision)
	return event
}

func normalizeGrants(grants []string) ([]string, error) {
	out := make([]string, 0, len(grants))
	seen := make(map[string]bool)
	for _, grant := range grants {
		if len([]rune(grant)) == 0 || len([]rune(grant)) > maxGrantRunes || !validGrantPattern.MatchString(grant) {
			return nil, Errorf(ErrorCodeInvalidArgument, "capability grant is not valid")
		}
		if !seen[grant] {
			seen[grant] = true
			out = append(out, grant)
		}
	}
	slices.Sort(out)
	return out, nil
}
