package execution

import (
	d "github.com/liquiditylab/liquiditylab/internal/domain"
)

type CompileResult struct {
	Intent *d.OrderIntent
	Cancel bool
	Reason string
}

func Compile(action d.Action, plan d.Plan, rules d.InstrumentRules, fees d.FeeConfig, config d.ReplayConfig, ledger *Ledger, book *Book, child *d.ChildOrder, now int64) (CompileResult, error) {
	if e := action.Validate(); e != nil {
		return CompileResult{}, e
	}
	result := func(reason string) CompileResult { return CompileResult{Reason: reason} }
	if action == d.Hold {
		return result("hold"), nil
	}
	if action == d.CancelChild {
		return CompileResult{Cancel: child != nil, Reason: "policy_cancel"}, nil
	}
	if !book.Valid || now >= plan.Deadline {
		return result("market_invalid_or_deadline"), nil
	}
	passive := action == d.PassiveSmall || action == d.PassiveNormal
	small := action == d.PassiveSmall || action == d.AggressiveSmall
	var price d.Decimal
	if plan.Side == "buy" {
		price = book.BestAsk().Add(rules.TickSize)
		if passive {
			price = book.BestBid()
		}
		price = d.Min(price, plan.LimitPrice).FloorStep(rules.TickSize)
	} else {
		price = book.BestBid().Sub(rules.TickSize)
		if passive {
			price = book.BestAsk()
		}
		price = d.Max(price, plan.LimitPrice).CeilStep(rules.TickSize)
	}
	if price.Sign() <= 0 {
		return result("invalid_tick_price"), nil
	}
	slices := (plan.Deadline-1)/(15*d.Second) + 1
	quantity := plan.Quantity.Quo(d.MustDecimal(integerString(slices)))
	if small {
		quantity = quantity.Quo(d.MustDecimal("2"))
	}
	kind, unitDebit, err := ReservationFor(d.OrderIntent{Side: plan.Side, Quantity: d.One, Price: price}, fees)
	if err != nil {
		return CompileResult{}, err
	}
	balance := ledger.Base.Quo(unitDebit)
	if kind == "quote" {
		balance = ledger.Quote.Quo(unitDebit)
	}
	side := "asks"
	if passive && plan.Side == "buy" || !passive && plan.Side == "sell" {
		side = "bids"
	}
	quantity = d.Min(quantity, ledger.Remaining(), plan.MaxChildQuantity, balance, book.Visible(side).Mul(config.MaxParticipation)).FloorStep(rules.LotSize)
	if quantity.Cmp(rules.MinQuantity) < 0 || quantity.Mul(price).Cmp(rules.MinNotional) < 0 {
		return result("below_minimum_or_no_free_balance"), nil
	}
	desired := d.OrderIntent{Side: plan.Side, Quantity: quantity, Price: price, Passive: passive}
	if child != nil {
		if child.State == "submitting" || child.State == "cancel_requested" {
			return result("awaiting_order_final_state"), nil
		}
		if child.Intent.Equal(desired) {
			return result("existing_order_matches"), nil
		}
		if now-child.ArrivalAt < config.MinRestingAge {
			return result("minimum_resting_age"), nil
		}
		return CompileResult{Cancel: true, Reason: "cancel_before_replace"}, nil
	}
	reason := "submit_ioc"
	if passive {
		reason = "submit_passive"
	}
	return CompileResult{Intent: &desired, Reason: reason}, nil
}
