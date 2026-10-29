package replay

import (
	"encoding/json"
	"math"
	"os"
	"testing"

	d "github.com/liquiditylab/liquiditylab/internal/domain"
)

func TestReplacementWaitsForKnownFinalWithRacingFill(t *testing.T) {
	raw, err := os.ReadFile("../../conformance/golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var golden observationGolden
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}
	var fixture *observationGoldenCase
	for index := range golden.Cases {
		if golden.Cases[index].Name == "replacement_waits_for_known_final_with_racing_fill" {
			fixture = &golden.Cases[index]
			break
		}
	}
	if fixture == nil {
		t.Fatal("shared replacement fixture missing")
	}
	engine, err := buildObservationCase(*fixture)
	if err != nil {
		t.Fatal(err)
	}
	// Expected states, times, amounts and rewards are hand calculated from the
	// tape and configuration, independently of Python's generated outputs.
	rewards := 0.0
	step := func(action d.Action, at int64, reason string, reserved, reward float64) StepResult {
		t.Helper()
		result, err := engine.Step(action)
		if err != nil {
			t.Fatal(err)
		}
		decision := engine.Decisions[len(engine.Decisions)-1]
		if engine.Now != at || decision.Reason != reason || math.Abs(float64(result.Observation[4])-reserved) > 1e-7 || math.Abs(result.Reward-reward) > 1e-10 {
			t.Fatalf("now=%d reason=%s reserved=%g reward=%.17g; want %d %s %g %.17g", engine.Now, decision.Reason, result.Observation[4], result.Reward, at, reason, reserved, reward)
		}
		rewards += result.Reward
		return result
	}

	step(d.PassiveNormal, d.Second, "submit_passive", 1, 50)
	original := engine.Child
	if original == nil || original.OrderID != "child-1" || original.State != "open" || !original.Intent.Quantity.Equal(dec(".2")) || !original.Remaining.Equal(dec(".2")) || !original.QueueAhead.Equal(dec("2")) || !original.Intent.Price.Equal(dec("99")) || len(engine.Orders) != 1 {
		t.Fatal("initial .2 passive child was not retained after the quote change")
	}
	step(d.PassiveNormal, 2*d.Second, "minimum_resting_age", 1, 0)
	if engine.Child != original || original.State != "open" || original.CancelAt != nil || len(engine.Orders) != 1 {
		t.Fatal("minimum resting age allowed cancellation or replacement")
	}

	step(d.PassiveNormal, 3*d.Second, "cancel_before_replace", .3, 28.07)
	if engine.Child != original || original.State != "cancel_requested" || original.CancelAt == nil || *original.CancelAt != 7*d.Second/2 || !original.Remaining.Equal(dec(".06")) || len(engine.Orders) != 1 || len(engine.Ledger.Fills) != 1 {
		t.Fatal("cancel request lost its remaining obligation or created a replacement")
	}
	fill := engine.Ledger.Fills[0]
	if fill.OrderID != original.OrderID || !fill.Quantity.Equal(dec(".14")) || !fill.Price.Equal(dec("99")) || !fill.Fee.Equal(dec(".01386")) || fill.FeeCurrency != "quote" || !fill.Maker || fill.Time != 5*d.Second/2 {
		t.Fatalf("partial fill during cancellation differs: %+v", fill)
	}
	if !engine.Ledger.Base.Equal(dec(".14")) || !engine.Ledger.Quote.Equal(dec("86.12614")) || !engine.Ledger.Remaining().Equal(dec(".06")) {
		t.Fatal("racing fill did not reconcile exact cash and parent remainder")
	}

	step(d.PassiveNormal, 4*d.Second, "awaiting_order_final_state", 0, 0)
	if engine.Child != nil || original.State != "cancelled" || len(engine.Orders) != 1 || !original.Remaining.Equal(dec(".06")) {
		t.Fatal("proposal during pending cancellation replaced the child before finality")
	}
	step(d.PassiveNormal, 5*d.Second, "submit_passive", .3, 0)
	replacement := engine.Child
	if replacement == nil || replacement == original || replacement.OrderID != "child-2" || replacement.State != "open" || replacement.SubmittedAt != 4*d.Second || !replacement.Intent.Quantity.Equal(dec(".06")) || !replacement.Remaining.Equal(dec(".06")) || !replacement.Intent.Price.Equal(dec("98")) || len(engine.Orders) != 2 || !engine.Ledger.Filled.Add(replacement.Remaining).Equal(engine.Plan.Quantity) {
		t.Fatal("known-final cancellation did not permit exactly the residual replacement")
	}

	terminal := step(d.Hold, 15*d.Second/2, "hold", 0, -30)
	report := engine.Report()
	if !terminal.Terminated || terminal.Truncated || engine.Child != nil || replacement.State != "cancelled" || engine.Status != "deadline_partial" || report["economic_score_valid"] != true || !report["residual_midpoint"].(d.Decimal).Equal(dec("99.5")) || !report["marked_cost_quote"].(d.Decimal).Equal(dec("-.15614")) || math.Abs(report["terminal_objective"].(float64)-48.07) > 1e-10 || math.Abs(rewards-48.07) > 1e-10 || len(engine.Ledger.Fills) != 1 {
		t.Fatal("replacement settlement did not preserve the partial economics and one residual penalty")
	}
}
