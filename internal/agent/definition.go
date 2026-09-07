package agent

import (
	"slices"
	"time"
)

type Definition struct {
	ID           string
	Description  string
	Instructions string
	Tools        []string
	Capabilities []string
	ModelRoute   string
	Limits       Limits
}

type Limits struct {
	TurnTimeout time.Duration
}

func (d *Definition) key() string {
	return d.ID
}

func (d *Definition) clone() Definition {
	return Definition{
		ID:           d.ID,
		Description:  d.Description,
		Instructions: d.Instructions,
		Tools:        append([]string(nil), d.Tools...),
		Capabilities: append([]string(nil), d.Capabilities...),
		ModelRoute:   d.ModelRoute,
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
