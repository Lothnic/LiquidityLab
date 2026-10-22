package execution

import (
	"testing"

	d "github.com/liquiditylab/liquiditylab/internal/domain"
)

func TestSharedFeeAndReservationRules(t *testing.T) {
	for _, test := range []struct{ side, currency, kind, amount, fee string }{
		{"buy", "quote", "quote", "200.4", "0.4"},
		{"buy", "base", "quote", "200", "0.004"},
		{"sell", "quote", "base", "2", "0.4"},
		{"sell", "base", "base", "2.004", "0.004"},
	} {
		fees := d.DefaultFees()
		fees.Currency = test.currency
		intent := d.OrderIntent{Side: test.side, Quantity: d.MustDecimal("2"), Price: d.MustDecimal("100")}
		kind, amount, err := ReservationFor(intent, fees)
		if err != nil || kind != test.kind || amount.String() != test.amount {
			t.Fatal(test, kind, amount, err)
		}
		fee, err := FeeFor(intent.Quantity, intent.Price, fees.Taker, test.currency)
		if err != nil || fee.String() != test.fee {
			t.Fatal(test, fee, err)
		}
	}
}

func TestSharedObservationUsesAccountValidity(t *testing.T) {
	plan := d.DefaultPlan()
	book := NewBook()
	if err := book.Apply(d.MarketEvent{Kind: "snapshot", Bids: []d.Level{{Price: d.MustDecimal("99"), Quantity: d.One}}, Asks: []d.Level{{Price: d.MustDecimal("101"), Quantity: d.One}}}); err != nil {
		t.Fatal(err)
	}
	input := FeatureSnapshot{Plan: plan, Ledger: NewLedger(plan), Book: book, ReferenceMid: d.MustDecimal("100"), AccountValid: false}
	invalid, err := BuildObservation(input)
	if err != nil {
		t.Fatal(err)
	}
	input.AccountValid = true
	valid, err := BuildObservation(input)
	if err != nil {
		t.Fatal(err)
	}
	if invalid[18] != 0 || valid[18] != 1 {
		t.Fatal("account validity not reflected")
	}
	if _, err := BuildObservation(FeatureSnapshot{}); err == nil {
		t.Fatal("invalid snapshot accepted")
	}
}
