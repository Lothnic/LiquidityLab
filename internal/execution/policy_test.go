package execution

import (
	"testing"

	d "github.com/liquiditylab/liquiditylab/internal/domain"
)

func TestScheduledActionAdvancesOncePerEligibleDecision(t *testing.T) {
	next := int64(0)
	for _, step := range []struct {
		elapsed, wantNext int64
		wantAction        d.Action
	}{
		{0, 15 * d.Second, d.AggressiveNormal},
		{0, 15 * d.Second, d.Hold},
		{15*d.Second - 1, 15 * d.Second, d.Hold},
		{15 * d.Second, 30 * d.Second, d.AggressiveNormal},
		// Late decisions advance one slice each, preserving replay catch-up.
		{75 * d.Second, 45 * d.Second, d.AggressiveNormal},
		{75 * d.Second, 60 * d.Second, d.AggressiveNormal},
		{75 * d.Second, 75 * d.Second, d.AggressiveNormal},
		{75 * d.Second, 90 * d.Second, d.AggressiveNormal},
		{75 * d.Second, 90 * d.Second, d.Hold},
	} {
		action, after := ScheduledAction(step.elapsed, next)
		if action != step.wantAction || after != step.wantNext {
			t.Fatalf("elapsed=%d next=%d: got %v/%d want %v/%d", step.elapsed, next, action, after, step.wantAction, step.wantNext)
		}
		next = after
	}
}

func TestLiquidityAwareActionPreservesReplayThresholds(t *testing.T) {
	plan := d.DefaultPlan()
	plan.Quantity, plan.Deadline = d.One, 100*d.Second
	for _, tc := range []struct {
		name    string
		elapsed int64
		filled  string
		want    d.Action
	}{
		{"initial", 0, "0", d.PassiveNormal},
		{"on schedule", 50 * d.Second, "0.5", d.PassiveNormal},
		{"ahead of schedule", 50 * d.Second, "0.7", d.PassiveNormal},
		{"below lag threshold", 9 * d.Second, "0", d.PassiveNormal},
		{"at lag threshold", 10 * d.Second, "0", d.PassiveNormal},
		{"past lag threshold", 10*d.Second + 1, "0", d.AggressiveNormal},
		// 0.4 - 0.3 is slightly greater than 0.1 in the existing baseline.
		// Preserve this behavior until a separately versioned policy changes it.
		{"existing float boundary", 40 * d.Second, "0.3", d.AggressiveNormal},
		{"before final quarter", 75*d.Second - 1, "0.8", d.PassiveNormal},
		{"at final quarter", 75 * d.Second, "0.8", d.AggressiveNormal},
		{"deadline proposal", 100 * d.Second, "1", d.AggressiveNormal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := LiquidityAwareAction(plan, tc.elapsed, d.MustDecimal(tc.filled)); got != tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestBaselineActionNamesAndExplicitState(t *testing.T) {
	plan := d.DefaultPlan()
	for _, name := range []string{BaselineScheduled, BaselineLiquidityAware} {
		if err := ValidateBaselinePolicy(name); err != nil {
			t.Fatalf("valid policy %q: %v", name, err)
		}
	}
	for _, name := range []string{"", "Scheduled", "scheduled ", "liquidity_aware", "rl"} {
		if err := ValidateBaselinePolicy(name); err == nil {
			t.Fatalf("accepted unknown policy %q", name)
		}
		action, next, err := BaselineAction(name, plan, 0, d.Zero, 123)
		if err == nil || action != d.Unspecified || next != 123 {
			t.Fatalf("invalid policy mutated proposal/state: %v %d %v", action, next, err)
		}
	}
	action, next, err := BaselineAction(BaselineScheduled, plan, 0, d.Zero, 0)
	if err != nil || action != d.AggressiveNormal || next != 15*d.Second {
		t.Fatalf("scheduled dispatch: %v %d %v", action, next, err)
	}
	action, next, err = BaselineAction(BaselineLiquidityAware, plan, 0, d.Zero, 123)
	if err != nil || action != d.PassiveNormal || next != 123 {
		t.Fatalf("liquidity-aware changed schedule state: %v %d %v", action, next, err)
	}
}
