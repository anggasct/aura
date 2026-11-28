package operation

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/anggasct/aura/internal/platform/telemetry"
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
	Withholdable []string
	ChatCommands bool
	Telemetry    *telemetry.OperationRecorder
}

type Discovery struct {
	registry     *Registry
	behaviors    []Behavior
	capabilities []string
	chatCommands bool
	recorder     *telemetry.OperationRecorder
}

func NewDiscovery(deps *DiscoveryDeps) (*Discovery, error) {
	if deps == nil || deps.Registry == nil {
		return nil, NewError(ErrorCodeInvalidArgument, "operation: discovery needs a registry")
	}
	discovery := &Discovery{
		registry:     deps.Registry,
		behaviors:    slices.Clone(deps.Behaviors),
		capabilities: slices.Clone(deps.Capabilities),
		chatCommands: deps.ChatCommands,
		recorder:     deps.Telemetry,
	}
	slices.SortFunc(discovery.behaviors, func(a, b Behavior) int { return compareStrings(a.ID, b.ID) })
	if err := discovery.validateBehaviors(); err != nil {
		return nil, err
	}
	if err := discovery.checkBound(deps.Withholdable); err != nil {
		return nil, err
	}
	return discovery, nil
}

func (d *Discovery) checkBound(withholdable []string) error {
	var generated []string
	for _, id := range d.registry.ids {
		if descriptor := d.registry.entries[id].descriptor; descriptor.GeneratesTool() {
			generated = append(generated, ToolName(descriptor.ID))
		} else if descriptor.ModuleTool != "" {
			generated = append(generated, descriptor.ModuleTool)
		}
	}
	for _, kind := range []TurnKind{TurnAttended, TurnUnattended} {
		for _, tracked := range []bool{false, true} {
			for _, search := range []bool{true, false} {
				in := &DescriptionInput{
					Turn:     Turn{Origin: turn.OriginTerminal, Kind: kind, ContentTaintTracked: tracked},
					Search:   search,
					Listed:   generated,
					Withheld: withholdable,
				}
				if _, err := d.Describe(in); err != nil {
					return fmt.Errorf("describe the %s turn: %w", kind, err)
				}
			}
		}
	}
	return nil
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
	return capabilitiesMet(descriptor, d.capabilities)
}
