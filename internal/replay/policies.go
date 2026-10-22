package replay

import (
	"errors"
	d "github.com/liquiditylab/liquiditylab/internal/domain"
)

type ScheduledPolicy struct{ NextSlice int64 }

func (p *ScheduledPolicy) Act(e *Engine) d.Action {
	if e.Now < p.NextSlice {
		return d.Hold
	}
	p.NextSlice += 15 * d.Second
	return d.AggressiveNormal
}

type LiquidityAwarePolicy struct{}

func (LiquidityAwarePolicy) Act(e *Engine) d.Action {
	elapsed := float64(e.Now) / float64(e.Plan.Deadline)
	filled := e.Ledger.Filled.Quo(e.Plan.Quantity).Float64()
	if elapsed-filled > 0.1 || elapsed >= 0.75 {
		return d.AggressiveNormal
	}
	return d.PassiveNormal
}
func RunPolicy(e *Engine, name string) (Report, error) {
	if name != "scheduled" && name != "liquidity-aware" {
		return nil, errors.New("unknown baseline policy")
	}
	if _, err := e.Reset(); err != nil {
		return nil, err
	}
	scheduled := ScheduledPolicy{}
	for !e.done() {
		action := d.Hold
		if name == "scheduled" {
			action = scheduled.Act(e)
		} else {
			action = LiquidityAwarePolicy{}.Act(e)
		}
		if _, err := e.Step(action); err != nil {
			return nil, err
		}
	}
	r := e.Report()
	r["policy"] = name
	return r, nil
}
func SyntheticEvents() []d.MarketEvent {
	events := []d.MarketEvent{{Time: 0, Kind: "snapshot", Bids: []d.Level{{Price: d.MustDecimal("99.99"), Quantity: d.MustDecimal("2")}}, Asks: []d.Level{{Price: d.MustDecimal("100.01"), Quantity: d.MustDecimal("2")}}}}
	for t := int64(1); t <= 65; t++ {
		events = append(events, d.MarketEvent{Time: t * d.Second, Kind: "trade", Side: "sell", Price: d.MustDecimal("99.99"), Quantity: d.MustDecimal(".4"), ReceiptIndex: uint64(t)})
	}
	return events
}
