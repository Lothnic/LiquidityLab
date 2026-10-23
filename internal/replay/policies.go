package replay

import (
	d "github.com/liquiditylab/liquiditylab/internal/domain"
	x "github.com/liquiditylab/liquiditylab/internal/execution"
)

type ScheduledPolicy struct{ NextSlice int64 }

func (p *ScheduledPolicy) Act(e *Engine) d.Action {
	action, next := x.ScheduledAction(e.Now, p.NextSlice)
	p.NextSlice = next
	return action
}

type LiquidityAwarePolicy struct{}

func (LiquidityAwarePolicy) Act(e *Engine) d.Action {
	return x.LiquidityAwareAction(e.Plan, e.Now, e.Ledger.Filled)
}
func RunPolicy(e *Engine, name string) (Report, error) {
	if err := x.ValidateBaselinePolicy(name); err != nil {
		return nil, err
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
