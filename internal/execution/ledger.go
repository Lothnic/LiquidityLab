package execution

import (
	"errors"
	d "github.com/liquiditylab/liquiditylab/internal/domain"
)

type Ledger struct {
	Plan                                               d.Plan
	Base, Quote, Filled, Notional, FeesQuote, FeesBase d.Decimal
	Fills                                              []d.Fill
	executions                                         map[string]d.Fill
}

func NewLedger(p d.Plan) *Ledger {
	return &Ledger{Plan: p, Base: p.BaseBalance, Quote: p.QuoteBudget, Fills: []d.Fill{}, executions: map[string]d.Fill{}}
}
func (l *Ledger) Remaining() d.Decimal { return l.Plan.Quantity.Sub(l.Filled) }
func (l *Ledger) AveragePrice() d.Decimal {
	if l.Filled.IsZero() {
		return d.Zero
	}
	return l.Notional.Quo(l.Filled)
}
func (l *Ledger) Apply(f d.Fill) (bool, error) {
	if e := f.Validate(); e != nil {
		return false, e
	}
	if p, ok := l.executions[f.ExecutionID]; ok {
		if !p.Equal(f) {
			return false, errors.New("conflicting duplicate execution")
		}
		return false, nil
	}
	if f.Side != l.Plan.Side || f.Quantity.Cmp(l.Remaining()) > 0 {
		return false, errors.New("fill exceeds parent obligation or side")
	}
	if f.Side == "buy" && f.Price.Cmp(l.Plan.LimitPrice) > 0 || f.Side == "sell" && f.Price.Cmp(l.Plan.LimitPrice) < 0 {
		return false, errors.New("fill breaches hard price boundary")
	}
	qf, bf := d.Zero, d.Zero
	if f.FeeCurrency == "quote" {
		qf = f.Fee
	} else {
		bf = f.Fee
	}
	notional := f.Quantity.Mul(f.Price)
	base, quote := l.Base, l.Quote
	if f.Side == "buy" {
		base = base.Add(f.Quantity)
		quote = quote.Sub(notional)
	} else {
		base = base.Sub(f.Quantity)
		quote = quote.Add(notional)
	}
	base = base.Sub(bf)
	quote = quote.Sub(qf)
	if d.Min(base, quote).Sign() < 0 {
		return false, errors.New("fill would create a negative balance")
	}
	l.Base, l.Quote = base, quote
	l.Filled = l.Filled.Add(f.Quantity)
	l.Notional = l.Notional.Add(notional)
	l.FeesQuote = l.FeesQuote.Add(qf)
	l.FeesBase = l.FeesBase.Add(bf)
	l.Fills = append(l.Fills, f)
	l.executions[f.ExecutionID] = f
	return true, nil
}
func (l *Ledger) MarkedCost(initial, residual d.Decimal) d.Decimal {
	sign := d.One
	if l.Plan.Side == "sell" {
		sign = sign.Neg()
	}
	fees := d.Zero
	for _, f := range l.Fills {
		fee := f.Fee
		if f.FeeCurrency == "base" {
			fee = fee.Mul(f.Price)
		}
		fees = fees.Add(fee)
	}
	return sign.Mul(l.Notional.Sub(l.Filled.Mul(initial))).Add(fees).Add(sign.Mul(l.Remaining()).Mul(residual.Sub(initial)))
}
