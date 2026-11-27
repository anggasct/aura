package runtimeadk

import (
	"crypto/sha256"
	"encoding/hex"
	"maps"
	"slices"
	"strings"

	"golang.org/x/text/language"
	"golang.org/x/text/language/display"
)

const (
	ownerContextHeader = "[owner context: owner-stated facts, trusted; take precedence over learned or inferred facts]"
	ownerContextFooter = "[/owner context]"
)

type OwnerContext struct {
	Name        string
	Language    string
	Timezone    string
	Preferences map[string]string
}

func RenderOwnerContext(owner *OwnerContext) string {
	if owner == nil {
		return ""
	}
	lines := make([]string, 0, 3+len(owner.Preferences))
	if owner.Name != "" {
		lines = append(lines, "name: "+owner.Name)
	}
	if owner.Language != "" {
		lines = append(lines, "language: "+formatOwnerLanguage(owner.Language))
	}
	if owner.Timezone != "" {
		lines = append(lines, "timezone: "+owner.Timezone)
	}
	for _, key := range slices.Sorted(maps.Keys(owner.Preferences)) {
		value := owner.Preferences[key]
		if key == "" || value == "" {
			continue
		}
		lines = append(lines, key+": "+value)
	}
	if len(lines) == 0 {
		return ""
	}
	return ownerContextHeader + "\n" + strings.Join(lines, "\n") + "\n" + ownerContextFooter
}

func formatOwnerLanguage(raw string) string {
	tag, err := language.Parse(raw)
	if err != nil {
		return raw
	}
	canonical := tag.String()
	base, conf := tag.Base()
	if conf != language.Exact {
		return canonical
	}
	name := display.English.Languages().Name(base)
	if name == "" {
		return canonical
	}
	return name + " (" + canonical + ")"
}

func OwnerContextDigest(block string) string {
	sum := sha256.Sum256([]byte(block))
	return hex.EncodeToString(sum[:])
}
