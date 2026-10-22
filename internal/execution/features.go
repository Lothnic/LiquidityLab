package execution

import (
	"fmt"
	d "github.com/liquiditylab/liquiditylab/internal/domain"
	"math"
)

const ObservationVersion = "compact-v1"
const ObservationSize = 20

var ObservationFields = []string{"remaining_fraction", "time_remaining_fraction", "filled_fraction", "average_fill_to_mid_ratio", "reserved_fraction", "free_quote_fraction", "signed_limit_distance", "spread_bps", "bid_depth_to_target", "ask_depth_to_target", "recent_mid_return", "recent_log_return_std", "cumulative_trade_flow_to_target", "signed_child_price_distance", "child_age_fraction", "child_remaining_fraction", "has_child", "market_valid", "simulated_account_valid", "child_pending"}

func flag(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

type FeatureSnapshot struct {
	Plan         d.Plan
	Ledger       *Ledger
	Book         *Book
	Child        *d.ChildOrder
	Now          int64
	ReferenceMid d.Decimal
	DecisionMids []d.Decimal
	TradeFlow    d.Decimal
	AccountValid bool
}

// BuildObservation is shared by replay and supervised runtime decisions. Inputs
// are read synchronously under their owner's serialization; it does not mutate state.
func BuildObservation(e FeatureSnapshot) ([]float32, error) {
	if e.Ledger == nil || e.Book == nil || e.Plan.Quantity.Sign() <= 0 || e.Plan.Deadline <= 0 || e.Now < 0 {
		return nil, fmt.Errorf("invalid feature snapshot")
	}
	for _, midpoint := range e.DecisionMids {
		if midpoint.Sign() <= 0 {
			return nil, fmt.Errorf("invalid causal midpoint history")
		}
	}

	q := e.Plan.Quantity
	mid := e.ReferenceMid
	if mid.IsZero() {
		mid = e.Book.Mid()
	}
	if mid.IsZero() {
		mid = d.One
	}
	c := e.Child
	reserved := d.Zero
	if c != nil {
		reserved = c.Remaining
	}
	sign := d.One
	if e.Plan.Side == "sell" {
		sign = sign.Neg()
	}
	ratio := func(a, b d.Decimal) float64 { return a.Quo(b).Float64() }
	recent, vol := float64(0), float64(0)
	if len(e.DecisionMids) > 0 {
		recent = ratio(mid, e.DecisionMids[0]) - 1
		if len(e.DecisionMids) > 2 {
			returns := make([]float64, len(e.DecisionMids)-1)
			mean := float64(0)
			for i := 1; i < len(e.DecisionMids); i++ {
				returns[i-1] = math.Log(e.DecisionMids[i].Float64()) - math.Log(e.DecisionMids[i-1].Float64())
				mean += returns[i-1]
			}
			mean /= float64(len(returns))
			for _, r := range returns {
				vol += (r - mean) * (r - mean)
			}
			vol = math.Sqrt(vol / float64(len(returns)))
		}
	}
	avg := float64(0)
	if !e.Ledger.Filled.IsZero() {
		avg = ratio(e.Ledger.AveragePrice(), mid)
	}
	distance, age := float64(0), float64(0)
	if c != nil {
		distance = ratio(sign.Mul(c.Intent.Price.Sub(mid)), mid)
		age = float64(max(e.Now-c.ArrivalAt, 0)) / float64(e.Plan.Deadline)
	}
	values := []float64{ratio(e.Ledger.Remaining(), q), float64(max(e.Plan.Deadline-e.Now, 0)) / float64(e.Plan.Deadline), ratio(e.Ledger.Filled, q), avg, ratio(reserved, q), ratio(e.Ledger.Quote, d.Max(e.Plan.QuoteBudget, d.One)), ratio(sign.Mul(e.Plan.LimitPrice.Sub(mid)), mid), ratio(e.Book.BestAsk().Sub(e.Book.BestBid()), mid) * 10000, ratio(e.Book.Visible("bids"), q), ratio(e.Book.Visible("asks"), q), recent, vol, ratio(e.TradeFlow, q), distance, age, ratio(reserved, q), flag(c != nil), flag(e.Book.Valid), flag(e.AccountValid), flag(c != nil && (c.State == "submitting" || c.State == "cancel_requested"))}
	out := make([]float32, len(values))
	for i, v := range values {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return nil, fmt.Errorf("nonfinite feature")
		}
		out[i] = float32(math.Max(-1e6, math.Min(1e6, v)))
	}
	return out, nil
}
