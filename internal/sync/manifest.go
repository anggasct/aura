package sync

import (
	"path"
	"slices"
	"strings"
)

const (
	maxManifestBytes int64 = 1 << 20
	maxManifestFiles       = 4096
)

var deniedSuffixes = []string{
	".db", ".db-wal", ".db-shm", ".sqlite", ".sqlite-wal", ".sqlite-shm",
}

var deniedNames = []string{
	".env", "known_hosts", "id_rsa", "id_ed25519", "id_ecdsa",
}

var deniedSegments = []string{
	".git", ".ssh", "artifacts", "blobs", "logs", "cache", "workspace",
	"approvals", "pending", "tmp",
}

func NormalizeManifest(include []string) ([]string, error) {
	if len(include) == 0 {
		return nil, Errorf(ErrorCodeManifestInvalid, "manifest must list at least one pattern")
	}
	seen := make(map[string]struct{}, len(include))
	roots := make([]string, 0, len(include))
	for _, pattern := range include {
		root, ok := strings.CutSuffix(pattern, "/**")
		if !ok || strings.TrimSpace(root) == "" {
			return nil, Errorf(ErrorCodeManifestInvalid, "manifest pattern %q must use <root>/**", pattern)
		}
		if root != "skills" && root != "config-templates" {
			return nil, Errorf(ErrorCodeManifestInvalid, "manifest root %q is not exportable", root)
		}
		if _, dup := seen[root]; dup {
			return nil, Errorf(ErrorCodeManifestInvalid, "manifest root %q is listed more than once", root)
		}
		seen[root] = struct{}{}
		roots = append(roots, root)
	}
	slices.Sort(roots)
	return roots, nil
}

func DeniedPath(rel string) bool {
	if rel == "" || path.IsAbs(rel) {
		return true
	}
	if raw := path.Clean(rel); raw == "." || raw == ".." || strings.HasPrefix(raw, "../") {
		return true
	}
	cleaned := path.Clean("/" + rel)[1:]
	if cleaned == "." || cleaned == "" {
		return true
	}
	lower := strings.ToLower(cleaned)
	for _, suffix := range deniedSuffixes {
		if strings.HasSuffix(lower, suffix) {
			return true
		}
	}
	base := path.Base(lower)
	for _, name := range deniedNames {
		if base == name || strings.HasSuffix(base, ".key") || strings.HasSuffix(base, ".pem") || strings.HasSuffix(base, ".token") {
			return true
		}
	}
	for _, segment := range strings.Split(lower, "/") {
		for _, denied := range deniedSegments {
			if segment == denied {
				return true
			}
		}
	}
	return false
}

func FilterExportable(roots []string, rel string, size int64) error {
	if size < 0 || size > maxManifestBytes {
		return Errorf(ErrorCodePathDenied, "entry exceeds the export bound")
	}
	cleaned := path.Clean(rel)
	if cleaned == "." || strings.HasPrefix(cleaned, "../") || path.IsAbs(rel) {
		return Errorf(ErrorCodePathDenied, "entry escapes its root")
	}
	matched := false
	for _, root := range roots {
		if cleaned == root || strings.HasPrefix(cleaned, root+"/") {
			matched = true
			break
		}
	}
	if !matched {
		return Errorf(ErrorCodePathDenied, "entry is outside the manifest")
	}
	if DeniedPath(cleaned) {
		return Errorf(ErrorCodePathDenied, "entry is not exportable")
	}
	return nil
}
