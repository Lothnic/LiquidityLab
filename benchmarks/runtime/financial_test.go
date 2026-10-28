package main

import (
	"testing"

	d "github.com/liquiditylab/liquiditylab/internal/domain"
	ar "github.com/liquiditylab/liquiditylab/internal/runtime"
	pg "github.com/liquiditylab/liquiditylab/internal/store/postgres"
)

func TestConservationAuditHandlesSidesFeesAndRejectsInconsistency(t *testing.T) {
	initial := []pg.Balance{{Currency: "BTC", Total: d.MustDecimal("10")}, {Currency: "USD", Total: d.MustDecimal("100000")}}
	for _, side := range []string{"buy", "sell"} {
		for _, feeCurrency := range []string{"base", "quote"} {
			t.Run(side+"_"+feeCurrency, func(t *testing.T) {
				fill := d.Fill{ExecutionID: "fill-1", OrderID: "child-1", Side: side, Quantity: d.MustDecimal("0.01"), Price: d.MustDecimal("100"), Fee: d.MustDecimal("0.001"), FeeCurrency: feeCurrency}
				plan := d.DefaultPlan()
				plan.Side = side
				plan.Quantity = d.MustDecimal("1")
				plan.LimitPrice = d.MustDecimal("100")
				base, quote := d.MustDecimal("10"), d.MustDecimal("100000")
				if side == "buy" {
					base = base.Add(fill.Quantity)
					quote = quote.Sub(d.One)
				} else {
					base = base.Sub(fill.Quantity)
					quote = quote.Add(d.One)
				}
				if feeCurrency == "base" {
					base = base.Sub(fill.Fee)
				} else {
					quote = quote.Sub(fill.Fee)
				}
				v := ar.View{State: ar.State{BaseCurrency: "BTC", QuoteCurrency: "USD", Parent: &ar.Parent{Spec: plan}, Fills: []d.Fill{fill}}, Account: pg.Account{Balances: []pg.Balance{{Currency: "BTC", Total: base, Free: base}, {Currency: "USD", Total: quote, Free: quote}}}}
				if audit := auditFinancial(initial, v, []d.Fill{fill}); !audit.Valid || !audit.Filled.Equal(fill.Quantity) {
					t.Fatalf("valid audit rejected: %+v", audit)
				}
				v.Account.Balances[0].Total = v.Account.Balances[0].Total.Add(d.One)
				if auditFinancial(initial, v, []d.Fill{fill}).Valid {
					t.Fatal("nonconserving total accepted")
				}
				v.Account.Balances[0].Total = base
				if auditFinancial(initial, v, nil).Valid {
					t.Fatal("missing durable execution accepted")
				}
				v.State.Fills = append(v.State.Fills, fill)
				if auditFinancial(initial, v, []d.Fill{fill, fill}).Valid {
					t.Fatal("duplicate fill accepted")
				}
				v.State.Fills = []d.Fill{fill}
				v.State.Parent.Spec.Quantity = d.MustDecimal("0.001")
				if auditFinancial(initial, v, []d.Fill{fill}).Valid {
					t.Fatal("parent overfill accepted")
				}
			})
		}
	}
}

func TestReservedSchemasAreRefused(t *testing.T) {
	for _, schema := range []string{"public", "information_schema", "pg_catalog", "pg_temp_1"} {
		c := testConfig()
		c.Schema = schema
		if c.validate() == nil {
			t.Fatalf("reserved schema %s accepted", schema)
		}
	}
}
