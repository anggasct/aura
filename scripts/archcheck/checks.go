package main

import (
	"fmt"
	"slices"
	"strings"
)

type groupKind int

const (
	kindExempt groupKind = iota
	kindLegacy
	kindApp
	kindPlatform
	kindModule
	kindChannelKit
	kindChannel
	kindPlugin
)

type group struct {
	kind groupKind
	name string
}

var legacyRoots = []string{"internal/cli", "internal/config", "internal/store"}

var allowedInternalDirs = []string{"app", "channels", "cli", "config", "modules", "platform", "store"}

const pluginWire = "internal/modules/plugin/wire"

func within(path, root string) bool {
	return path == root || strings.HasPrefix(path, root+"/")
}

func segmentAfter(path, root string) string {
	rest := strings.TrimPrefix(path, root+"/")
	name, _, _ := strings.Cut(rest, "/")
	return name
}

func classify(path string) group {
	switch {
	case slices.ContainsFunc(legacyRoots, func(root string) bool { return within(path, root) }):
		return group{kind: kindLegacy, name: path}
	case within(path, "internal/app"):
		return group{kind: kindApp}
	case within(path, "internal/platform"):
		return group{kind: kindPlatform}
	case within(path, "internal/modules"):
		return group{kind: kindModule, name: segmentAfter(path, "internal/modules")}
	case within(path, "internal/channels/kit"):
		return group{kind: kindChannelKit}
	case within(path, "internal/channels"):
		return group{kind: kindChannel, name: segmentAfter(path, "internal/channels")}
	case within(path, "plugins"):
		return group{kind: kindPlugin, name: segmentAfter(path, "plugins")}
	default:
		return group{kind: kindExempt}
	}
}

func layerViolation(from, to string) bool {
	source, target := classify(from), classify(to)
	if source.kind == kindExempt || source.kind == kindLegacy || source.kind == kindApp || target.kind == kindLegacy {
		return false
	}
	if source.kind == kindPlugin {
		return !within(to, pluginWire)
	}
	upward := target.kind == kindApp
	switch source.kind {
	case kindPlatform:
		return upward || target.kind == kindModule || target.kind == kindChannel || target.kind == kindChannelKit
	case kindModule:
		return upward || target.kind == kindChannel || target.kind == kindChannelKit || (target.kind == kindModule && target.name != source.name)
	case kindChannelKit:
		return upward || target.kind == kindModule || target.kind == kindChannel
	case kindChannel:
		return upward || target.kind == kindModule || (target.kind == kindChannel && target.name != source.name)
	default:
		return false
	}
}

type allowEntry struct {
	from, to string
	line     int
}

func parseAllow(content string) (entries []allowEntry, problems []string) {
	for index, raw := range strings.Split(content, "\n") {
		line, _, _ := strings.Cut(raw, "#")
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		from, to, ok := strings.Cut(line, "->")
		if !ok || strings.TrimSpace(from) == "" || strings.TrimSpace(to) == "" {
			problems = append(problems, fmt.Sprintf("layers.allow:%d: malformed entry %q", index+1, raw))
			continue
		}
		entries = append(entries, allowEntry{from: strings.TrimSpace(from), to: strings.TrimSpace(to), line: index + 1})
	}
	return entries, problems
}

func checkLayers(packages []repoPackage, allowed []allowEntry) []string {
	used := make([]bool, len(allowed))
	var violations []string
	for _, pkg := range packages {
		for _, imported := range uniqueSorted(pkg.imports) {
			if !layerViolation(pkg.path, imported) {
				continue
			}
			index := slices.IndexFunc(allowed, func(entry allowEntry) bool {
				return within(pkg.path, entry.from) && within(imported, entry.to)
			})
			if index >= 0 {
				used[index] = true
				continue
			}
			violations = append(violations, fmt.Sprintf("layers: %s imports %s", pkg.path, imported))
		}
	}
	for index, entry := range allowed {
		if !used[index] {
			violations = append(violations, fmt.Sprintf("layers.allow:%d: stale allow entry %s -> %s", entry.line, entry.from, entry.to))
		}
	}
	return violations
}

func uniqueSorted(values []string) []string {
	sorted := slices.Clone(values)
	slices.Sort(sorted)
	return slices.Compact(sorted)
}

type dirEntry struct {
	name  string
	isDir bool
}

func checkLayout(internalEntries map[string][]dirEntry) []string {
	var violations []string
	for _, entry := range internalEntries["internal"] {
		if !entry.isDir {
			violations = append(violations, "layout: file internal/"+entry.name+" is outside a package group")
			continue
		}
		if !slices.Contains(allowedInternalDirs, entry.name) {
			violations = append(violations, "layout: internal/"+entry.name+" is not an allowed top-level directory")
		}
	}
	for _, groupDir := range []string{"internal/platform", "internal/modules", "internal/channels"} {
		for _, entry := range internalEntries[groupDir] {
			if !entry.isDir && strings.HasSuffix(entry.name, ".go") {
				violations = append(violations, "layout: "+groupDir+"/"+entry.name+" sits directly in a group directory")
			}
		}
	}
	return violations
}

func checkStdNames(packages []repoPackage, stdNames map[string]string, importedStd map[string]bool) []string {
	taken := map[string]string{}
	for path := range importedStd {
		taken[stdNames[path]] = path
	}
	var violations []string
	for _, pkg := range packages {
		if pkg.name == "main" {
			continue
		}
		if stdPath, ok := taken[pkg.name]; ok {
			violations = append(violations, fmt.Sprintf("stdnames: %s is named %q like standard library %s", pkg.path, pkg.name, stdPath))
		}
	}
	return violations
}
