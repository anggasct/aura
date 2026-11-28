package operation

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/anggasct/aura/internal/platform/turn"
)

const (
	maxBehaviorSummaryBytes = 240
	maxBehaviors            = 32
)

var behaviorRule = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

type DiscoveryDeps struct {
	Registry     *Registry
	Behaviors    []Behavior
	Capabilities []string
}

type Discovery struct {
	registry     *Registry
	behaviors    []Behavior
	capabilities []string
}

func NewDiscovery(deps *DiscoveryDeps) (*Discovery, error) {
	if deps == nil || deps.Registry == nil {
		return nil, NewError(ErrorCodeInvalidArgument, "operation: discovery needs a registry")
	}
	discovery := &Discovery{
		registry:     deps.Registry,
		behaviors:    slices.Clone(deps.Behaviors),
		capabilities: slices.Clone(deps.Capabilities),
	}
	slices.SortFunc(discovery.behaviors, func(a, b Behavior) int { return compareStrings(a.ID, b.ID) })
	if err := discovery.validateBehaviors(); err != nil {
		return nil, err
	}
	for _, kind := range []TurnKind{TurnAttended, TurnUnattended} {
		for _, tracked := range []bool{false, true} {
			if _, err := discovery.Describe(&DescriptionInput{Turn: Turn{Origin: turn.OriginTerminal, Kind: kind, ContentTaintTracked: tracked}, Search: true}); err != nil {
				return nil, fmt.Errorf("describe the %s turn: %w", kind, err)
			}
		}
	}
	return discovery, nil
}

func (d *Discovery) validateBehaviors() error {
	var defects []error
	if len(d.behaviors) > maxBehaviors {
		defects = append(defects, NewError(ErrorCodeInvalid, fmt.Sprintf("operation: %d behaviors are declared, over %d", len(d.behaviors), maxBehaviors)))
	}
	seen := map[string]bool{}
	for i := range d.behaviors {
		behavior := &d.behaviors[i]
		switch {
		case !behaviorRule.MatchString(behavior.ID):
			defects = append(defects, NewError(ErrorCodeInvalid, "operation: a behavior id does not match its pattern"))
		case seen[behavior.ID]:
			defects = append(defects, NewError(ErrorCodeInvalid, "operation: behavior "+behavior.ID+" is declared twice"))
		case behavior.Summary == "" || len(behavior.Summary) > maxBehaviorSummaryBytes || strings.ContainsAny(behavior.Summary, "\r\n"):
			defects = append(defects, NewError(ErrorCodeInvalid, "operation: behavior "+behavior.ID+" has no single-line summary within its bound"))
		}
		seen[behavior.ID] = true
	}
	return errors.Join(defects...)
}

func (d *Discovery) available(descriptor *Descriptor) bool {
	for _, required := range descriptor.RequiredCapabilities {
		if !slices.Contains(d.capabilities, required) {
			return false
		}
	}
	return true
}
