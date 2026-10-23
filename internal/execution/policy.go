package execution

import (
	"errors"

	d "github.com/liquiditylab/liquiditylab/internal/domain"
)

const (
	BaselineScheduled      = "scheduled"
	BaselineLiquidityAware = "liquidity-aware"
	BaselineSliceInterval  = 15 * d.Second
)

// ValidateBaselinePolicy checks the shared baseline names before a session starts.
func ValidateBaselinePolicy(name string) error {
	if name != BaselineScheduled && name != BaselineLiquidityAware {
		return errors.New("unknown baseline policy")
	}
	return nil
}

// ScheduledAction advances the explicit schedule by one slice per eligible
// decision, including when the caller is several slices behind. Callers persist
// the returned nextSlice with the decision; this function has no hidden state.
func ScheduledAction(elapsed, nextSlice int64) (d.Action, int64) {
	if elapsed < nextSlice {
		return d.Hold, nextSlice
	}
	return d.AggressiveNormal, nextSlice + BaselineSliceInterval
}

// LiquidityAwareAction retains the replay baseline's elapsed/filled comparisons,
// including its float64 threshold behavior. Inputs come from a validated plan;
// the compiler enforces market validity, deadline, and execution constraints.
func LiquidityAwareAction(plan d.Plan, elapsed int64, filled d.Decimal) d.Action {
	elapsedFraction := float64(elapsed) / float64(plan.Deadline)
	filledFraction := filled.Quo(plan.Quantity).Float64()
	if elapsedFraction-filledFraction > 0.1 || elapsedFraction >= 0.75 {
		return d.AggressiveNormal
	}
	return d.PassiveNormal
}

// BaselineAction selects a pure baseline proposal and returns its explicit
// schedule state. Liquidity-aware decisions leave scheduled state unchanged.
func BaselineAction(name string, plan d.Plan, elapsed int64, filled d.Decimal, nextSlice int64) (d.Action, int64, error) {
	if err := ValidateBaselinePolicy(name); err != nil {
		return d.Unspecified, nextSlice, err
	}
	if name == BaselineScheduled {
		action, next := ScheduledAction(elapsed, nextSlice)
		return action, next, nil
	}
	return LiquidityAwareAction(plan, elapsed, filled), nextSlice, nil
}
