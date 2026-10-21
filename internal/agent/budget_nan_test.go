package agent

import (
	"math"
	"testing"

	"go.harness.dev/harness/internal/engine/types"
)

// `-max-cost NaN` parses as a float. NaN fails both `< 0` and `> 0`, so it
// passes validation for an unknown-price model and then disables the cost
// check at every boundary: the refusal is bypassed and the budget is silently
// unlimited. A non-finite limit must be rejected like a negative one.
func TestBudgetValidateRejectsNaNLimits(t *testing.T) {
	unknownPrice := types.Model{Provider: "openai", ID: "unlisted", Cost: types.ModelCost{Unknown: true}}
	knownPrice := types.Model{Provider: "openai", ID: "priced", Cost: types.ModelCost{Input: 1, Output: 1}}
	for _, model := range []types.Model{unknownPrice, knownPrice} {
		if err := (Budget{MaxCostUSD: math.NaN()}).Validate(model); err == nil {
			t.Errorf("Validate(MaxCostUSD: NaN) for %s = nil, want an error", model.ID)
		}
	}
}

func TestBudgetValidateRejectsInfiniteLimits(t *testing.T) {
	for _, limit := range []float64{math.Inf(1), math.Inf(-1)} {
		if err := (Budget{MaxCostUSD: limit}).Validate(types.Model{}); err == nil {
			t.Errorf("Validate(MaxCostUSD: %g) = nil, want an error", limit)
		}
	}
}
