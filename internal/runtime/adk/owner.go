package runtimeadk

import (
	"crypto/sha256"
	"encoding/hex"
	"maps"
	"slices"
	"strings"
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
		lines = append(lines, "language: "+owner.Language)
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

func OwnerContextDigest(block string) string {
	sum := sha256.Sum256([]byte(block))
	return hex.EncodeToString(sum[:])
}
