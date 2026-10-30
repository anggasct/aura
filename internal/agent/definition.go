package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"
	"time"
)

type Definition struct {
	ID           string
	Description  string
	Instructions string
	SoulFile     string
	Tools        []string
	Capabilities []string
	ModelRoute   string
	MemoryScope  string
	Limits       Limits
}

const (
	MemoryScopePersona = "persona"
	MemoryScopeShared  = "shared"
)

type Limits struct {
	TurnTimeout time.Duration
}

type Fingerprint struct {
	AgentID     string
	SoulSHA256  string
	ToolsSHA256 string
	ModelRoute  string
}

func (d *Definition) Fingerprint() Fingerprint {
	tools := slices.Clone(d.Tools)
	slices.Sort(tools)
	return Fingerprint{
		AgentID:     d.ID,
		SoulSHA256:  sha256Hex(d.Instructions),
		ToolsSHA256: sha256Hex(strings.Join(tools, "\n")),
		ModelRoute:  d.ModelRoute,
	}
}

func sha256Hex(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

func (d *Definition) key() string {
	return d.ID
}

func (d *Definition) clone() Definition {
	return Definition{
		ID:           d.ID,
		Description:  d.Description,
		Instructions: d.Instructions,
		SoulFile:     d.SoulFile,
		Tools:        append([]string(nil), d.Tools...),
		Capabilities: append([]string(nil), d.Capabilities...),
		ModelRoute:   d.ModelRoute,
		MemoryScope:  d.MemoryScope,
		Limits:       d.Limits,
	}
}

func (d *Definition) supports(required []string) bool {
	for _, need := range required {
		if !slices.Contains(d.Capabilities, need) {
			return false
		}
	}
	return true
}

func (d *Definition) unusedCapabilityCount(required []string) int {
	unused := 0
	for _, declared := range d.Capabilities {
		if !slices.Contains(required, declared) {
			unused++
		}
	}
	return unused
}
