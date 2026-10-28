package execution

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/big"
	"math/rand"
	"testing"

	d "github.com/liquiditylab/liquiditylab/internal/domain"
)

func propertyDecimal(t testing.TB, coefficient int64, scale int32) d.Decimal {
	t.Helper()
	value, err := d.FromCoefficient(fmt.Sprint(coefficient), scale)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func propertyRat(t testing.TB, value d.Decimal) *big.Rat {
	t.Helper()
	number, ok := new(big.Rat).SetString(value.String())
	if !ok {
		t.Fatalf("invalid decimal %q", value)
	}
	return number
}

func propertyEqualRat(t testing.TB, name string, got d.Decimal, want *big.Rat) {
	t.Helper()
	if propertyRat(t, got).Cmp(want) != 0 {
		t.Fatalf("%s: got %s, want exact %s", name, got, want.RatString())
	}
}

func propertyLedgerSnapshot(t testing.TB, ledger *Ledger) []byte {
	t.Helper()
	// Include the private identity index: exported balances alone cannot detect
	// a conflicting duplicate which silently replaces its original identity.
	data, err := json.Marshal(struct {
		Ledger     *Ledger
		Executions map[string]d.Fill
	}{ledger, ledger.executions})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func checkGeneratedLedger(t testing.TB, input []byte) {
	t.Helper()
	if len(input) > 64 {
		input = input[:64]
	}
	for _, side := range []string{"buy", "sell"} {
		for _, currency := range []string{"base", "quote"} {
			plan := d.DefaultPlan()
			plan.Side, plan.Quantity = side, d.One
			plan.BaseBalance, plan.QuoteBudget = dec("10"), dec("1000")
			plan.LimitPrice = dec("110")
			if side == "sell" {
				plan.LimitPrice = dec("90")
			}
			if err := plan.Validate(); err != nil {
				t.Fatal(err)
			}
			ledger := NewLedger(plan)
			base, quote := big.NewRat(10, 1), big.NewRat(1000, 1)
			filled, notional, quoteFees, baseFees := new(big.Rat), new(big.Rat), new(big.Rat), new(big.Rat)
			for index, raw := range input {
				units, price, rate := int64(raw%10)+1, int64(raw%19)+91, int64((int(raw)+index)%5)
				feeUnits := units * rate
				if currency == "quote" {
					feeUnits *= price
				}
				fill := d.Fill{ExecutionID: fmt.Sprintf("generated-%d", index), OrderID: "child", Side: side,
					Quantity: propertyDecimal(t, units, 3), Price: propertyDecimal(t, price, 0),
					Fee: propertyDecimal(t, feeUnits, 6), FeeCurrency: currency, Time: int64(index), Maker: raw&1 != 0}
				if added, err := ledger.Apply(fill); err != nil || !added {
					t.Fatalf("%s/%s fill %d: added=%v err=%v", side, currency, index, added, err)
				}
				// This oracle uses integer units and rational cashflows, without any
				// production Decimal arithmetic, FeeFor, or Ledger calculations.
				quantity, amount := big.NewRat(units, 1000), big.NewRat(units*price, 1000)
				fee := big.NewRat(feeUnits, 1_000_000)
				filled.Add(filled, quantity)
				notional.Add(notional, amount)
				if side == "buy" {
					base.Add(base, quantity)
					quote.Sub(quote, amount)
				} else {
					base.Sub(base, quantity)
					quote.Add(quote, amount)
				}
				if currency == "quote" {
					quote.Sub(quote, fee)
					quoteFees.Add(quoteFees, fee)
				} else {
					base.Sub(base, fee)
					baseFees.Add(baseFees, fee)
				}
				propertyEqualRat(t, "base", ledger.Base, base)
				propertyEqualRat(t, "quote", ledger.Quote, quote)
				propertyEqualRat(t, "filled", ledger.Filled, filled)
				propertyEqualRat(t, "remaining", ledger.Remaining(), new(big.Rat).Sub(big.NewRat(1, 1), filled))
				propertyEqualRat(t, "notional", ledger.Notional, notional)
				propertyEqualRat(t, "quote fees", ledger.FeesQuote, quoteFees)
				propertyEqualRat(t, "base fees", ledger.FeesBase, baseFees)
				if len(ledger.Fills) != index+1 || len(ledger.executions) != index+1 {
					t.Fatal("execution identity or fill count differs from unique fills")
				}
				before := propertyLedgerSnapshot(t, ledger)
				if added, err := ledger.Apply(fill); added || err != nil {
					t.Fatalf("exact duplicate: added=%v err=%v", added, err)
				}
				if !bytes.Equal(before, propertyLedgerSnapshot(t, ledger)) {
					t.Fatal("exact duplicate changed ledger or private execution index")
				}
				conflict := fill
				switch index % 8 {
				case 0:
					conflict.Quantity = propertyDecimal(t, units+1, 3)
				case 1:
					conflict.Price = propertyDecimal(t, price+1, 0)
				case 2:
					conflict.Fee = propertyDecimal(t, feeUnits+1, 6)
				case 3:
					conflict.Side = "buy"
					if side == "buy" {
						conflict.Side = "sell"
					}
				case 4:
					conflict.OrderID = "different-child"
				case 5:
					conflict.Time++
				case 6:
					conflict.Maker = !conflict.Maker
				case 7:
					conflict.FeeCurrency = "base"
					if currency == "base" {
						conflict.FeeCurrency = "quote"
					}
				}
				if added, err := ledger.Apply(conflict); added || err == nil {
					t.Fatalf("conflicting duplicate: added=%v err=%v", added, err)
				}
				if !bytes.Equal(before, propertyLedgerSnapshot(t, ledger)) {
					t.Fatal("duplicate changed ledger or private execution index")
				}
				if added, err := ledger.Apply(fill); added || err != nil {
					t.Fatalf("original duplicate after conflict: added=%v err=%v", added, err)
				}
				if !bytes.Equal(before, propertyLedgerSnapshot(t, ledger)) || !ledger.executions[fill.ExecutionID].Equal(fill) {
					t.Fatal("conflict replaced the legitimate execution identity")
				}
			}
			// Empty fuzz inputs still check initialization and the zero-fill obligation.
			propertyEqualRat(t, "final base", ledger.Base, base)
			propertyEqualRat(t, "final quote", ledger.Quote, quote)
			propertyEqualRat(t, "final filled", ledger.Filled, filled)
			propertyEqualRat(t, "final remaining", ledger.Remaining(), new(big.Rat).Sub(big.NewRat(1, 1), filled))
		}
	}
}

func TestGeneratedLedgerConservationAndDuplicateAtomicity(t *testing.T) {
	for seed := int64(0); seed < 8; seed++ {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			input := make([]byte, 32+seed*4)
			if _, err := rand.New(rand.NewSource(seed)).Read(input); err != nil {
				t.Fatal(err)
			}
			checkGeneratedLedger(t, input)
		})
	}
}

func FuzzLedgerConservationAndDuplicateAtomicity(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0, 1, 9, 19, 255})
	f.Add(bytes.Repeat([]byte{255}, 64))
	f.Fuzz(func(t *testing.T, input []byte) { checkGeneratedLedger(t, input) })
}

func propertyGrid(value, step *big.Rat, ceiling bool) *big.Rat {
	ratio := new(big.Rat).Quo(value, step)
	integer, remainder := new(big.Int), new(big.Int)
	integer.QuoRem(ratio.Num(), ratio.Denom(), remainder)
	if ceiling && remainder.Sign() != 0 {
		integer.Add(integer, big.NewInt(1))
	}
	return new(big.Rat).Mul(new(big.Rat).SetInt(integer), step)
}

func propertyMin(values ...*big.Rat) *big.Rat {
	result := new(big.Rat).Set(values[0])
	for _, value := range values[1:] {
		if value.Cmp(result) < 0 {
			result.Set(value)
		}
	}
	return result
}

func checkGeneratedCompiler(t testing.TB, seed uint64) {
	t.Helper()
	rng := rand.New(rand.NewSource(int64(seed)))
	plan, rules, fees, config := d.DefaultPlan(), d.DefaultRules(), d.DefaultFees(), d.DefaultConfig()
	if seed&1 != 0 {
		plan.Side = "sell"
	}
	plan.Quantity = propertyDecimal(t, int64(50+rng.Intn(201)), 3)
	plan.MaxChildQuantity = propertyDecimal(t, int64(1+rng.Intn(101)), 3)
	plan.Deadline = int64(1+rng.Intn(31)) * d.Second
	plan.QuoteBudget, plan.BaseBalance = propertyDecimal(t, int64(1+rng.Intn(31)), 0), propertyDecimal(t, int64(10+rng.Intn(491)), 3)
	plan.LimitPrice = propertyDecimal(t, int64(95002+rng.Intn(10000)), 3)
	rules.TickSize = []d.Decimal{dec(".01"), dec(".05"), dec(".1"), d.One}[seed%4]
	rules.LotSize = []d.Decimal{dec(".001"), dec(".005"), dec(".01")}[seed%3]
	rules.MinQuantity = rules.LotSize.Mul(propertyDecimal(t, int64(1+seed%3), 0))
	rules.MinNotional = []d.Decimal{dec(".01"), dec(".1"), d.One, dec("5")}[seed%4]
	fees.Maker, fees.Taker = propertyDecimal(t, int64(rng.Intn(10)), 3), propertyDecimal(t, int64(rng.Intn(20)), 3)
	if seed&2 != 0 {
		fees.Currency = "base"
	}
	config.MaxParticipation = []d.Decimal{dec(".1"), dec(".25"), dec(".5"), d.One}[seed%4]
	bid, ask := propertyDecimal(t, int64(91003+rng.Intn(7997)), 3), propertyDecimal(t, int64(101007+rng.Intn(7993)), 3)
	bidDepth, askDepth := propertyDecimal(t, int64(1+rng.Intn(501)), 3), propertyDecimal(t, int64(1+rng.Intn(501)), 3)
	book := NewBook()
	if err := book.Apply(d.MarketEvent{Kind: "snapshot", Bids: []d.Level{{Price: bid, Quantity: bidDepth}}, Asks: []d.Level{{Price: ask, Quantity: askDepth}}}); err != nil {
		t.Fatal(err)
	}
	// Some candidates see consumed depth, expired plans, or invalid feeds.
	book.Consume("bids", bid, bidDepth.Quo(dec("2")))
	book.Consume("asks", ask, askDepth.Quo(dec("2")))
	if seed%11 == 0 {
		book.Valid = false
	}
	now := int64(0)
	if seed%13 == 0 {
		now = plan.Deadline
	}
	ledger := NewLedger(plan)
	prior := d.Fill{ExecutionID: "prior", OrderID: "prior-child", Side: plan.Side, Quantity: dec(".001"), Price: plan.LimitPrice, FeeCurrency: fees.Currency}
	if added, err := ledger.Apply(prior); err != nil || !added {
		t.Fatal("invalid generated compiler ledger", err)
	}
	var child *d.ChildOrder
	if seed%7 == 0 {
		child = &d.ChildOrder{OrderID: "pending", Intent: d.OrderIntent{Side: plan.Side, Quantity: dec(".001"), Price: bid, Passive: true}, Remaining: dec(".001"), State: "submitting"}
	}
	for _, err := range []error{plan.Validate(), rules.Validate(), fees.Validate(), config.Validate()} {
		if err != nil {
			t.Fatal("invalid generated inputs", err)
		}
	}
	before := propertyLedgerSnapshot(t, ledger)
	bookBefore, _ := json.Marshal(book)
	childBefore, _ := json.Marshal(child)
	for action := d.Hold; action <= d.AggressiveNormal; action++ {
		got, err := Compile(action, plan, rules, fees, config, ledger, book, child, now)
		if err != nil {
			t.Fatalf("seed=%d action=%d: %v", seed, action, err)
		}
		expectedCancel := action == d.CancelChild && child != nil
		if got.Cancel != expectedCancel {
			t.Fatalf("unexpected cancellation for seed=%d action=%d: %v", seed, action, got)
		}
		if action <= d.CancelChild || !book.Valid || now >= plan.Deadline {
			if got.Intent != nil {
				t.Fatal("hold/cancel/invalid market/deadline emitted an intent")
			}
		} else {
			passive := action == d.PassiveSmall || action == d.PassiveNormal
			price := propertyRat(t, ask)
			if plan.Side == "buy" {
				price.Add(price, propertyRat(t, rules.TickSize))
				if passive {
					price = propertyRat(t, bid)
				}
				price = propertyGrid(propertyMin(price, propertyRat(t, plan.LimitPrice)), propertyRat(t, rules.TickSize), false)
			} else {
				price = new(big.Rat).Sub(propertyRat(t, bid), propertyRat(t, rules.TickSize))
				if passive {
					price = propertyRat(t, ask)
				}
				if price.Cmp(propertyRat(t, plan.LimitPrice)) < 0 {
					price = propertyRat(t, plan.LimitPrice)
				}
				price = propertyGrid(price, propertyRat(t, rules.TickSize), true)
			}
			feeRate := propertyRat(t, fees.Maker)
			if feeRate.Cmp(propertyRat(t, fees.Taker)) < 0 {
				feeRate = propertyRat(t, fees.Taker)
			}
			unitDebit, balance := big.NewRat(1, 1), propertyRat(t, ledger.Base)
			debitCurrency := "base"
			if plan.Side == "buy" {
				unitDebit, balance, debitCurrency = new(big.Rat).Set(price), propertyRat(t, ledger.Quote), "quote"
			}
			if fees.Currency == debitCurrency {
				unitDebit.Mul(unitDebit, new(big.Rat).Add(big.NewRat(1, 1), feeRate))
			}
			slices := (plan.Deadline-1)/(15*d.Second) + 1
			quantity := new(big.Rat).Quo(propertyRat(t, plan.Quantity), big.NewRat(slices, 1))
			if action == d.PassiveSmall || action == d.AggressiveSmall {
				quantity.Quo(quantity, big.NewRat(2, 1))
			}
			depth := new(big.Rat).Quo(propertyRat(t, askDepth), big.NewRat(2, 1))
			if passive && plan.Side == "buy" || !passive && plan.Side == "sell" {
				depth.Quo(propertyRat(t, bidDepth), big.NewRat(2, 1))
			}
			remaining := new(big.Rat).Sub(propertyRat(t, plan.Quantity), propertyRat(t, ledger.Filled))
			quantity = propertyGrid(propertyMin(quantity, remaining, propertyRat(t, plan.MaxChildQuantity), new(big.Rat).Quo(balance, unitDebit), new(big.Rat).Mul(depth, propertyRat(t, config.MaxParticipation))), propertyRat(t, rules.LotSize), false)
			acceptable := quantity.Cmp(propertyRat(t, rules.MinQuantity)) >= 0 && new(big.Rat).Mul(quantity, price).Cmp(propertyRat(t, rules.MinNotional)) >= 0 && child == nil
			if (got.Intent != nil) != acceptable {
				t.Fatalf("seed=%d action=%d expected accepted=%v: %v", seed, action, acceptable, got)
			}
			if got.Intent != nil {
				intent := got.Intent
				propertyEqualRat(t, "compiled price", intent.Price, price)
				propertyEqualRat(t, "compiled quantity", intent.Quantity, quantity)
				if intent.Side != plan.Side || intent.Passive != passive || new(big.Rat).Mul(propertyRat(t, intent.Quantity), unitDebit).Cmp(balance) > 0 {
					t.Fatal("intent side/passivity or full-fee reservation failed")
				}
			}
		}
		bookAfter, _ := json.Marshal(book)
		childAfter, _ := json.Marshal(child)
		if !bytes.Equal(before, propertyLedgerSnapshot(t, ledger)) || !bytes.Equal(bookBefore, bookAfter) || !bytes.Equal(childBefore, childAfter) {
			t.Fatalf("compiler mutated its inputs for action %d", action)
		}
	}
}

func TestGeneratedCompilerConstraints(t *testing.T) {
	for seed := uint64(0); seed < 64; seed++ {
		t.Run(fmt.Sprint(seed), func(t *testing.T) { checkGeneratedCompiler(t, seed) })
	}
}

func FuzzCompilerConstraints(f *testing.F) {
	for _, seed := range []uint64{0, 1, 7, 19, 42, 63} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, seed uint64) { checkGeneratedCompiler(t, seed) })
}

func TestGeneratedCompilerExactThresholds(t *testing.T) {
	// Exercise values immediately below, at, and above full-fee affordability,
	// minimum notional, and lot dust boundaries. These deliberately include
	// non-grid market prices and hard limits on either side of a tick.
	for _, side := range []string{"buy", "sell"} {
		for _, currency := range []string{"base", "quote"} {
			for _, tick := range []string{".01", ".05"} {
				for action := d.PassiveSmall; action <= d.AggressiveNormal; action++ {
					t.Run(fmt.Sprintf("%s/%s/tick%s/action%d", side, currency, tick, action), func(t *testing.T) {
						p, rules, fees, cfg := d.DefaultPlan(), d.DefaultRules(), d.DefaultFees(), d.DefaultConfig()
						p.Side, p.Quantity, p.MaxChildQuantity = side, dec(".021"), dec(".02")
						p.Deadline, p.BaseBalance, p.QuoteBudget = d.Second, d.One, dec("1000")
						p.LimitPrice = dec("100.004")
						if side == "sell" {
							p.LimitPrice = dec("99.996")
						}
						rules.TickSize, rules.LotSize, rules.MinQuantity, rules.MinNotional = dec(tick), dec(".01"), dec(".01"), dec(".001")
						fees.Currency, fees.Maker, fees.Taker = currency, dec(".01"), dec(".02")
						cfg.MaxParticipation = d.One
						book := NewBook()
						if err := book.Apply(d.MarketEvent{Kind: "snapshot", Bids: []d.Level{{Price: dec("99.995"), Quantity: d.One}}, Asks: []d.Level{{Price: dec("100.005"), Quantity: d.One}}}); err != nil {
							t.Fatal(err)
						}
						passive := action == d.PassiveSmall || action == d.PassiveNormal
						price := big.NewRat(100, 1)
						if passive {
							price = propertyGrid(big.NewRat(99995, 1000), propertyRat(t, rules.TickSize), false)
							if side == "sell" {
								price = propertyGrid(big.NewRat(100005, 1000), propertyRat(t, rules.TickSize), true)
							}
						}
						quantity := big.NewRat(2, 100)
						if action == d.PassiveSmall || action == d.AggressiveSmall {
							quantity.SetFrac64(1, 100)
						}
						unitDebit := big.NewRat(1, 1)
						if side == "buy" {
							unitDebit.Set(price)
						}
						if side == "buy" && currency == "quote" || side == "sell" && currency == "base" {
							unitDebit.Mul(unitDebit, big.NewRat(102, 100))
						}
						check := func(plan d.Plan, minimum d.Decimal, want *big.Rat) {
							t.Helper()
							rules.MinNotional = minimum
							got, err := Compile(action, plan, rules, fees, cfg, NewLedger(plan), book, nil, 0)
							accepted := want.Sign() > 0
							if err != nil || (got.Intent != nil) != accepted {
								t.Fatalf("expected accepted=%v quantity=%s: %v, %v", accepted, want.RatString(), got, err)
							}
							if accepted {
								propertyEqualRat(t, "threshold price", got.Intent.Price, price)
								propertyEqualRat(t, "threshold quantity", got.Intent.Quantity, want)
							}
						}
						for offset := int64(-1); offset <= 1; offset++ {
							balance := new(big.Rat).Add(new(big.Rat).Mul(quantity, unitDebit), big.NewRat(offset, 1_000_000))
							affordable := p
							if side == "buy" {
								affordable.QuoteBudget = dec(balance.FloatString(9))
							} else {
								affordable.BaseBalance = dec(balance.FloatString(9))
							}
							want := new(big.Rat).Set(quantity)
							if offset < 0 {
								want.Sub(want, big.NewRat(1, 100))
							}
							check(affordable, dec(".001"), want)
							minimum := new(big.Rat).Add(new(big.Rat).Mul(quantity, price), big.NewRat(offset, 1_000_000))
							want.Set(quantity)
							if offset > 0 {
								want.SetInt64(0)
							}
							check(p, dec(minimum.FloatString(9)), want)
						}
						for _, units := range []int64{9, 10, 11} {
							dust := p
							dust.Quantity = propertyDecimal(t, units, 3)
							want := big.NewRat(units, 1000)
							if action == d.PassiveSmall || action == d.AggressiveSmall {
								want.Quo(want, big.NewRat(2, 1))
							}
							check(dust, dec(".001"), propertyGrid(want, big.NewRat(1, 100), false))
						}
					})
				}
			}
		}
	}
}
