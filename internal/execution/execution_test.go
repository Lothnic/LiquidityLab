package execution

import (
	d "github.com/liquiditylab/liquiditylab/internal/domain"
	"testing"
)

func dec(s string) d.Decimal { return d.MustDecimal(s) }
func TestLedgerGoldenAndDuplicate(t *testing.T) {
	for _, tt := range []struct{ side, currency, fee, base, quote, cost string }{{"buy", "quote", ".0199", ".1", "40.0301", ".0699"}, {"sell", "base", ".0001", ".1999", "10.1", "-.1899"}} {
		p := d.DefaultPlan()
		p.Side = tt.side
		p.Quantity = dec(".2")
		p.LimitPrice = dec("100")
		p.BaseBalance = dec(".3")
		p.QuoteBudget = dec("0")
		price := dec("101")
		if tt.side == "buy" {
			p.BaseBalance = d.Zero
			p.QuoteBudget = dec("50")
			price = dec("99.5")
		}
		l := NewLedger(p)
		fill := d.Fill{ExecutionID: "e", OrderID: "o", Side: tt.side, Quantity: dec(".1"), Price: price, Fee: dec(tt.fee), FeeCurrency: tt.currency, Time: 1}
		if ok, err := l.Apply(fill); !ok || err != nil {
			t.Fatal(err)
		}
		if !l.Base.Equal(dec(tt.base)) || !l.Quote.Equal(dec(tt.quote)) {
			t.Fatal(l.Base, l.Quote)
		}
		if ok, err := l.Apply(fill); ok || err != nil {
			t.Fatal(ok, err)
		}
		if !l.MarkedCost(dec("100"), dec("101")).Equal(dec(tt.cost)) {
			t.Fatal(l.MarkedCost(dec("100"), dec("101")))
		}
		fill.Fee = fill.Fee.Add(dec(".01"))
		if _, err := l.Apply(fill); err == nil {
			t.Fatal("duplicate conflict")
		}
	}
}
func TestLedgerRejectsAtomically(t *testing.T) {
	p := d.DefaultPlan()
	p.BaseBalance = d.Zero
	p.QuoteBudget = dec("1")
	l := NewLedger(p)
	for _, f := range []d.Fill{{ExecutionID: "e", OrderID: "o", Side: "buy", Quantity: dec(".2"), Price: dec("100"), FeeCurrency: "quote"}, {ExecutionID: "e", OrderID: "o", Side: "buy", Quantity: dec(".1"), Price: dec("103"), FeeCurrency: "quote"}, {ExecutionID: "e", OrderID: "o", Side: "buy", Quantity: dec(".1"), Price: dec("100"), FeeCurrency: "quote"}} {
		if _, err := l.Apply(f); err == nil {
			t.Fatal("bad fill accepted")
		}
		if !l.Base.IsZero() || !l.Quote.Equal(d.One) || len(l.Fills) != 0 {
			t.Fatal("partial mutation")
		}
	}
}
func TestBookDebtPersistsUntilExplicitDeletion(t *testing.T) {
	b := NewBook()
	snap := d.MarketEvent{Kind: "snapshot", Bids: []d.Level{{Price: dec("99"), Quantity: dec("2")}}, Asks: []d.Level{{Price: dec("101"), Quantity: dec("2")}}}
	b.Apply(snap)
	b.Consume("asks", dec("101"), d.One)
	b.Apply(snap)
	if !b.Visible("asks").Equal(d.One) {
		t.Fatal("snapshot erased debt")
	}
	b.Apply(d.MarketEvent{Kind: "book", Asks: []d.Level{{Price: dec("101"), Quantity: d.Zero}}})
	b.Apply(snap)
	if !b.Visible("asks").Equal(dec("2")) {
		t.Fatal("deletion did not clear debt")
	}
}
func TestCompilerFeesAndFloors(t *testing.T) {
	b := NewBook()
	b.Apply(d.MarketEvent{Kind: "snapshot", Bids: []d.Level{{Price: dec("99"), Quantity: dec("2")}}, Asks: []d.Level{{Price: dec("100"), Quantity: dec("2")}}})
	p := d.DefaultPlan()
	p.Deadline = 2 * d.Second
	p.QuoteBudget = dec("10.05")
	fees := d.DefaultFees()
	fees.Maker = dec(".01")
	fees.Taker = dec(".01")
	res, err := Compile(d.AggressiveNormal, p, d.DefaultRules(), fees, d.DefaultConfig(), NewLedger(p), b, nil, 0)
	if err != nil || res.Intent == nil {
		t.Fatal(res, err)
	}
	if res.Intent.Quantity.Mul(res.Intent.Price).Mul(dec("1.01")).Cmp(p.QuoteBudget) > 0 {
		t.Fatal("underreserved fee")
	}
	p.Quantity = dec(".15")
	rules := d.DefaultRules()
	rules.LotSize = dec(".1")
	rules.MinQuantity = dec(".1")
	res, _ = Compile(d.PassiveSmall, p, rules, fees, d.DefaultConfig(), NewLedger(p), b, nil, 0)
	if res.Intent != nil || res.Reason != "below_minimum_or_no_free_balance" {
		t.Fatal(res)
	}
}
