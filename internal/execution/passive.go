package execution

import d "github.com/liquiditylab/liquiditylab/internal/domain"

// PassiveMatch describes the queue depletion and possible maker execution from
// one unique public trade. The caller owns trade deduplication and fill booking.
type PassiveMatch struct {
	QueueAhead d.Decimal
	Quantity   d.Decimal
}

// MatchPassiveTrade uses the replay model: only an opposite-side trade exactly
// at the child price depletes its queue. A pending cancellation can still fill;
// book touches and trades through the price do not imply an execution.
// Neither input is mutated.
func MatchPassiveTrade(child d.ChildOrder, event d.MarketEvent) PassiveMatch {
	match := PassiveMatch{QueueAhead: child.QueueAhead, Quantity: d.Zero}
	if event.Kind != "trade" || event.Invalid || (child.State != "open" && child.State != "cancel_requested") {
		return match
	}
	qualifying := child.Intent.Side == "buy" && event.Side == "sell" || child.Intent.Side == "sell" && event.Side == "buy"
	if !qualifying || !event.Price.Equal(child.Intent.Price) {
		return match
	}
	ahead := d.Min(event.Quantity, child.QueueAhead)
	match.QueueAhead = child.QueueAhead.Sub(ahead)
	volume := event.Quantity.Sub(ahead)
	if volume.Sign() > 0 {
		match.Quantity = d.Min(volume, child.Remaining)
	}
	return match
}
