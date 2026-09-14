package context

import (
	"strings"
	"testing"
)

func testPlanner(counter TokenCounter) *Planner {
	planner, err := NewPlanner(1024, 0.80, counter)
	if err != nil {
		panic(err)
	}
	return planner
}

func testCapability() Capability {
	return Capability{ID: "test-model", ContextTokens: 100000, Tokenizer: "test-tokenizer-v1"}
}

func TestPlanBudgetFormula(t *testing.T) {
	planner := testPlanner(exactCounter{})
	invocation := Invocation{
		SystemText:      strings.Repeat("s", 4000),
		ToolText:        strings.Repeat("t", 2000),
		MaxOutputTokens: 4000,
		InvocationID:    "inv-new",
	}
	plan, err := planner.Plan(testCapability(), invocation, []Event{turnEvent(1, "t1", "hi")})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	budget := plan.Budget
	if budget.SystemTokens != 4000 || budget.ToolTokens != 2000 || budget.ReservedOutput != 4000 {
		t.Errorf("reserves = %+v", budget)
	}
	if budget.Usable != 100000-4000-4000-2000-1024 {
		t.Errorf("usable = %d", budget.Usable)
	}
	if budget.AccountingClass != AccountingExact || budget.CounterName != "test-exact-v1" {
		t.Errorf("accounting = %+v", budget)
	}
	if budget.Effective != budget.Usable {
		t.Errorf("exact plans keep the full usable budget: %+v", budget)
	}
	if plan.CapabilityID != "test-model" || plan.HeadTokens != 4000+4000+2000+1024 {
		t.Errorf("plan = %+v", plan)
	}
	if plan.TotalEventTokens != 2 || len(plan.Groups) != 1 {
		t.Errorf("plan = %+v", plan)
	}
}

func TestPlanEstimatedDeratesBudget(t *testing.T) {
	planner := testPlanner(ConservativeEstimator{})
	plan, err := planner.Plan(testCapability(), Invocation{MaxOutputTokens: 1000}, nil)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan.Budget.AccountingClass != AccountingEstimated {
		t.Errorf("class = %q", plan.Budget.AccountingClass)
	}
	want := int(float64(plan.Budget.Usable) * 0.80)
	if plan.Budget.Effective != want || plan.Budget.Effective >= plan.Budget.Usable {
		t.Errorf("effective = %d, usable = %d", plan.Budget.Effective, plan.Budget.Usable)
	}
}

func TestPlanDefaultsToEstimator(t *testing.T) {
	planner, err := NewPlanner(1024, 0.80, nil)
	if err != nil {
		t.Fatalf("NewPlanner: %v", err)
	}
	plan, err := planner.Plan(testCapability(), Invocation{}, nil)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan.Budget.AccountingClass != AccountingEstimated || plan.Budget.CounterName != "utf8-conservative-v1" {
		t.Errorf("budget = %+v", plan.Budget)
	}
}

func TestPlanRejects(t *testing.T) {
	planner := testPlanner(exactCounter{})
	cases := map[string]struct {
		capability Capability
		invocation Invocation
		code       ErrorCode
	}{
		"missing capability id":       {capability: Capability{ContextTokens: 1000}, code: ErrorCodeCapabilityMissing},
		"zero window":                 {capability: Capability{ID: "m"}, code: ErrorCodeCapabilityMissing},
		"negative window":             {capability: Capability{ID: "m", ContextTokens: -5}, code: ErrorCodeCapabilityMissing},
		"negative output":             {capability: testCapability(), invocation: Invocation{MaxOutputTokens: -1}, code: ErrorCodeInvalidArgument},
		"reserves consume window":     {capability: Capability{ID: "m", ContextTokens: 100}, invocation: Invocation{MaxOutputTokens: 100}, code: ErrorCodeBudgetExceeded},
		"system text consumes window": {capability: Capability{ID: "m", ContextTokens: 100}, invocation: Invocation{SystemText: strings.Repeat("s", 200)}, code: ErrorCodeBudgetExceeded},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := planner.Plan(tc.capability, tc.invocation, nil); err == nil {
				t.Errorf("expected error")
			} else if code, ok := CodeOf(err); !ok || code != tc.code {
				t.Errorf("code = %v, %v (%v)", code, ok, err)
			}
		})
	}
}

func TestNewPlannerRejects(t *testing.T) {
	for name, args := range map[string]struct {
		margin int
		ratio  float64
	}{
		"zero margin":     {margin: 0, ratio: 0.8},
		"negative margin": {margin: -1, ratio: 0.8},
		"zero ratio":      {margin: 1, ratio: 0},
		"unit ratio":      {margin: 1, ratio: 1},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewPlanner(args.margin, args.ratio, nil); err == nil {
				t.Errorf("expected error")
			} else if code, ok := CodeOf(err); !ok || code != ErrorCodeInvalidArgument {
				t.Errorf("code = %v, %v (%v)", code, ok, err)
			}
		})
	}
}

func TestPlanNilPlanner(t *testing.T) {
	var planner *Planner
	_, err := planner.Plan(testCapability(), Invocation{}, nil)
	if code, ok := CodeOf(err); !ok || code != ErrorCodeInvalidArgument {
		t.Errorf("code = %v, %v (%v)", code, ok, err)
	}
}
