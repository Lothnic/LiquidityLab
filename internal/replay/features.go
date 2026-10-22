package replay

import (
	d "github.com/liquiditylab/liquiditylab/internal/domain"
	"math"
)

func flag(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
func (e *Engine) Observe() []float32 {
	q := e.Plan.Quantity
	mid := e.lastMid
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
	if len(e.decisionMids) > 0 {
		recent = ratio(mid, e.decisionMids[0]) - 1
		if len(e.decisionMids) > 2 {
			returns := make([]float64, len(e.decisionMids)-1)
			mean := float64(0)
			for i := 1; i < len(e.decisionMids); i++ {
				returns[i-1] = math.Log(e.decisionMids[i].Float64()) - math.Log(e.decisionMids[i-1].Float64())
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
	values := []float64{ratio(e.Ledger.Remaining(), q), float64(max(e.Plan.Deadline-e.Now, 0)) / float64(e.Plan.Deadline), ratio(e.Ledger.Filled, q), avg, ratio(reserved, q), ratio(e.Ledger.Quote, d.Max(e.Plan.QuoteBudget, d.One)), ratio(sign.Mul(e.Plan.LimitPrice.Sub(mid)), mid), ratio(e.Book.BestAsk().Sub(e.Book.BestBid()), mid) * 10000, ratio(e.Book.Visible("bids"), q), ratio(e.Book.Visible("asks"), q), recent, vol, ratio(e.tradeFlow, q), distance, age, ratio(reserved, q), flag(c != nil), flag(e.Book.Valid), 1, flag(c != nil && (c.State == "submitting" || c.State == "cancel_requested"))}
	out := make([]float32, len(values))
	for i, v := range values {
		out[i] = float32(math.Max(-1e6, math.Min(1e6, v)))
	}
	return out
}
