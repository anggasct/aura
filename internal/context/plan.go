package context

import (
	"strings"
)

type Capability struct {
	ID            string
	ContextTokens int
	Tokenizer     string
}

type Invocation struct {
	SystemText      string
	ToolText        string
	MaxOutputTokens int
	InvocationID    string
}

type Budget struct {
	Window          int
	ReservedOutput  int
	SystemTokens    int
	ToolTokens      int
	SafetyMargin    int
	Usable          int
	Effective       int
	AccountingClass AccountingClass
	CounterName     string
}

type Plan struct {
	CapabilityID     string
	Budget           Budget
	HeadTokens       int
	Groups           []Group
	TotalEventTokens int
}

type Planner struct {
	safetyMargin      int
	conservativeRatio float64
	counter           TokenCounter
}

func NewPlanner(safetyMarginTokens int, conservativeRatio float64, counter TokenCounter) (*Planner, error) {
	if safetyMarginTokens <= 0 {
		return nil, Errorf(ErrorCodeInvalidArgument, "safety margin must be positive")
	}
	if conservativeRatio <= 0 || conservativeRatio >= 1 {
		return nil, Errorf(ErrorCodeInvalidArgument, "conservative ratio must be in (0,1)")
	}
	if counter == nil {
		counter = ConservativeEstimator{}
	}
	return &Planner{
		safetyMargin:      safetyMarginTokens,
		conservativeRatio: conservativeRatio,
		counter:           counter,
	}, nil
}

func (p *Planner) Plan(capability Capability, invocation Invocation, events []Event) (*Plan, error) {
	if p == nil {
		return nil, errNilArgument("planner")
	}
	if strings.TrimSpace(capability.ID) == "" || capability.ContextTokens <= 0 {
		return nil, Errorf(ErrorCodeCapabilityMissing, "model capability carries no usable input budget")
	}
	if invocation.MaxOutputTokens < 0 {
		return nil, Errorf(ErrorCodeInvalidArgument, "reserved output must not be negative")
	}
	systemTokens, err := sumCounts(p.counter, []string{invocation.SystemText})
	if err != nil {
		return nil, err
	}
	toolTokens, err := sumCounts(p.counter, []string{invocation.ToolText})
	if err != nil {
		return nil, err
	}
	head, err := checkedAdd(invocation.MaxOutputTokens, systemTokens)
	if err != nil {
		return nil, err
	}
	head, err = checkedAdd(head, toolTokens)
	if err != nil {
		return nil, err
	}
	head, err = checkedAdd(head, p.safetyMargin)
	if err != nil {
		return nil, err
	}
	usable := capability.ContextTokens - head
	if usable <= 0 {
		return nil, Errorf(ErrorCodeBudgetExceeded, "reserves leave no usable input budget")
	}
	groups, err := GroupEvents(events, p.counter, invocation.InvocationID)
	if err != nil {
		return nil, err
	}
	total := 0
	for i := range groups {
		total, err = checkedAdd(total, groups[i].Tokens)
		if err != nil {
			return nil, err
		}
	}
	effective := usable
	if p.counter.Class() == AccountingEstimated {
		effective = int(float64(usable) * p.conservativeRatio)
	}
	return &Plan{
		CapabilityID: capability.ID,
		Budget: Budget{
			Window:          capability.ContextTokens,
			ReservedOutput:  invocation.MaxOutputTokens,
			SystemTokens:    systemTokens,
			ToolTokens:      toolTokens,
			SafetyMargin:    p.safetyMargin,
			Usable:          usable,
			Effective:       effective,
			AccountingClass: p.counter.Class(),
			CounterName:     p.counter.Name(),
		},
		HeadTokens:       head,
		Groups:           groups,
		TotalEventTokens: total,
	}, nil
}
