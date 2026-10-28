package replay

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"math/rand"
	"reflect"
	"sort"
	"testing"

	d "github.com/liquiditylab/liquiditylab/internal/domain"
)

type generatedReplayCase struct {
	events           []d.MarketEvent
	plan             d.Plan
	rules            d.InstrumentRules
	fees             d.FeeConfig
	config           d.ReplayConfig
	actions          []d.Action
	filled, residual d.Decimal
	terminated       bool
	status           string
	endTime          int64
	orderStates      []string
}

func generatedDecimal(t testing.TB, coefficient int64, scale int32) d.Decimal {
	t.Helper()
	value, err := d.FromCoefficient(fmt.Sprint(coefficient), scale)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func generatedRat(t testing.TB, value d.Decimal) *big.Rat {
	t.Helper()
	number, ok := new(big.Rat).SetString(value.String())
	if !ok {
		t.Fatal("nondecimal value", value)
	}
	return number
}

func generatedRatEqual(t testing.TB, name string, got d.Decimal, want *big.Rat) {
	t.Helper()
	if generatedRat(t, got).Cmp(want) != 0 {
		t.Fatalf("%s got %s, want exact %s", name, got, want.RatString())
	}
}

func makeGeneratedReplayCase(t testing.TB, seed uint64) generatedReplayCase {
	t.Helper()
	mode, delta := seed%5, int64(seed/5%3)-1
	rng := rand.New(rand.NewSource(int64(seed)))
	mid := int64(95 + rng.Intn(11))
	units := int64(1 + rng.Intn(7))
	value := generatedReplayCase{plan: d.DefaultPlan(), rules: d.DefaultRules(), fees: d.DefaultFees(), config: d.DefaultConfig(), terminated: true, status: "deadline_partial"}
	p, c := &value.plan, &value.config
	if seed/15%2 != 0 {
		p.Side = "sell"
	}
	if seed/30%2 != 0 {
		value.fees.Currency = "base"
	}
	p.Quantity = generatedDecimal(t, 4*units, 3)
	p.MaxChildQuantity = generatedDecimal(t, 3*units, 3)
	p.BaseBalance, p.QuoteBudget = d.One, dec("1000")
	p.Deadline = d.Second
	c.SubmissionLatency, c.CancelLatency = d.Second/2, d.Second/4
	c.DecisionInterval, c.SettlementAllowance = d.Second, d.Second
	c.MinMessageInterval, c.MinRestingAge = 0, 0
	c.MaxParticipation = d.One
	value.rules.MinQuantity, value.rules.LotSize = dec(".001"), dec(".001")
	value.rules.MinNotional = dec(".001")
	bid, ask := generatedDecimal(t, mid-1, 0), generatedDecimal(t, mid+1, 0)
	ownPrice, tradeSide := bid, "sell"
	if p.Side == "sell" {
		ownPrice, tradeSide = ask, "buy"
	}
	p.LimitPrice = ownPrice
	value.residual = generatedDecimal(t, mid, 0)
	initial := d.MarketEvent{Kind: "snapshot", Bids: []d.Level{{Price: bid, Quantity: generatedDecimal(t, 2*units, 3)}}, Asks: []d.Level{{Price: ask, Quantity: generatedDecimal(t, 2*units, 3)}}}
	value.events = []d.MarketEvent{initial}
	trade := func(at int64, id string) d.MarketEvent {
		return d.MarketEvent{Time: at, Kind: "trade", Side: tradeSide, Price: ownPrice, Quantity: generatedDecimal(t, 3*units, 3), TradeID: id}
	}
	shift := func(at, amount int64) d.MarketEvent {
		return d.MarketEvent{Time: at, Kind: "snapshot", Bids: []d.Level{{Price: generatedDecimal(t, mid-1+amount, 0), Quantity: d.One}}, Asks: []d.Level{{Price: generatedDecimal(t, mid+1+amount, 0), Quantity: d.One}}}
	}
	value.actions = []d.Action{d.PassiveNormal}
	value.orderStates = []string{"cancelled"}
	switch mode {
	case 0:
		// A receipt before or at arrival cannot fill an order whose queue has
		// not been established. One nanosecond later it consumes queue + one lot.
		value.events = append(value.events, trade(c.SubmissionLatency+delta, "arrival-race"), shift(p.Deadline+d.Second/8, 5))
		if delta > 0 {
			value.filled = generatedDecimal(t, units, 3)
		}
		value.endTime = p.Deadline + c.CancelLatency
	case 1:
		// Freeze the receipt-time midpoint at deadline, then move it during
		// settlement. A trade at the cancellation acknowledgement still fills;
		// an identical receipt must not consume queue or fill twice.
		c.SubmissionLatency, c.CancelLatency = 0, d.Second/2
		at := p.Deadline + c.CancelLatency + delta
		value.events = append(value.events, shift(p.Deadline, 2), shift(p.Deadline+d.Second/4, 5), trade(at, "cancel-race"), trade(at, "cancel-race"), d.MarketEvent{Time: p.Deadline + c.CancelLatency + 2, Kind: "gap"})
		value.residual = generatedDecimal(t, mid+2, 0)
		if delta <= 0 {
			value.filled = generatedDecimal(t, units, 3)
		}
		value.endTime = p.Deadline + c.CancelLatency
	case 2:
		// Receipt, arrival, and cancellation share a timestamp. Receipt runs
		// first, arrival establishes the queue, then cancellation removes it.
		c.SubmissionLatency, c.CancelLatency = 3*d.Second/2, d.Second/2
		at := p.Deadline + c.CancelLatency
		value.events = append(value.events, trade(at, "triple-tie"), trade(at+1, "after-cancel"))
		value.endTime = at
	case 3:
		// Repeating an absolute level does not replenish consumed historical
		// liquidity. Explicit deletion and refresh permit exactly one new take.
		p.Deadline = 4 * d.Second
		p.MaxChildQuantity = generatedDecimal(t, units, 3)
		c.SubmissionLatency, c.CancelLatency = 0, 0
		top := ask
		bookSide := "asks"
		if p.Side == "sell" {
			top, bookSide = bid, "bids"
		}
		p.LimitPrice = top
		initial.Bids = append([]d.Level(nil), initial.Bids...)
		initial.Asks = append([]d.Level(nil), initial.Asks...)
		levels := []d.Level{{Price: top, Quantity: generatedDecimal(t, units, 3)}}
		away := top.Add(dec("3"))
		if p.Side == "sell" {
			away = top.Sub(dec("3"))
		}
		levels = append(levels, d.Level{Price: away, Quantity: d.One})
		if bookSide == "asks" {
			initial.Asks = levels
		} else {
			initial.Bids = levels
		}
		value.events[0] = initial
		update := func(at int64, quantity d.Decimal) d.MarketEvent {
			event := d.MarketEvent{Time: at, Kind: "book"}
			if bookSide == "asks" {
				event.Asks = []d.Level{{Price: top, Quantity: quantity}}
			} else {
				event.Bids = []d.Level{{Price: top, Quantity: quantity}}
			}
			return event
		}
		value.events = append(value.events, update(d.Second, generatedDecimal(t, units, 3)), update(3*d.Second/2, d.Zero), update(8*d.Second/5, generatedDecimal(t, units, 3)))
		value.actions = []d.Action{d.AggressiveNormal, d.AggressiveNormal, d.AggressiveNormal, d.AggressiveNormal}
		value.filled = generatedDecimal(t, 2*units, 3)
		value.endTime = p.Deadline
		value.orderStates = []string{"filled", "ioc_expired", "filled", "ioc_expired"}
	case 4:
		// Gap wins a receipt/arrival tie and preserves an invalid trajectory,
		// even when the recording later contains another complete snapshot.
		p.Deadline = 3 * d.Second
		value.events = append(value.events, d.MarketEvent{Time: c.SubmissionLatency, Kind: "gap"})
		value.terminated, value.status = false, "invalid_market_data"
		value.endTime = c.SubmissionLatency
		value.orderStates = []string{"submitting"}
	}
	value.events = append(value.events, shift(p.Deadline+c.SettlementAllowance, 0))
	// Stable sorting preserves intentionally repeated receipts at the same time.
	sort.SliceStable(value.events, func(i, j int) bool { return value.events[i].Time < value.events[j].Time })
	return value
}

func checkGeneratedReplayEconomics(t testing.TB, value generatedReplayCase, engine *Engine, result StepResult, accumulatedReward float64) {
	t.Helper()
	base, quote := generatedRat(t, engine.Plan.BaseBalance), generatedRat(t, engine.Plan.QuoteBudget)
	filled, notional, quoteFees, baseFees, markedFees := new(big.Rat), new(big.Rat), new(big.Rat), new(big.Rat), new(big.Rat)
	identities := make(map[string]bool)
	lastTime := int64(0)
	for _, fill := range engine.Ledger.Fills {
		if identities[fill.ExecutionID] || fill.Time < lastTime || fill.Time > engine.Now || fill.Side != engine.Plan.Side {
			t.Fatal("duplicate execution or invalid receipt ordering/side")
		}
		identities[fill.ExecutionID], lastTime = true, fill.Time
		quantity, price := generatedRat(t, fill.Quantity), generatedRat(t, fill.Price)
		limit := generatedRat(t, engine.Plan.LimitPrice)
		if quantity.Sign() <= 0 || engine.Plan.Side == "buy" && price.Cmp(limit) > 0 || engine.Plan.Side == "sell" && price.Cmp(limit) < 0 {
			t.Fatal("fill violates quantity or hard limit")
		}
		// These scenarios use passive orders throughout, or IOC orders throughout.
		// Derive fee semantics from the fixture, never from the produced fill labels.
		expectedMaker := value.actions[0] == d.PassiveNormal
		if fill.Maker != expectedMaker || fill.FeeCurrency != value.fees.Currency {
			t.Fatal("fill maker status or fee currency differs from the generated scenario")
		}
		rate := generatedRat(t, value.fees.Taker)
		if expectedMaker {
			rate = generatedRat(t, value.fees.Maker)
		}
		fee := new(big.Rat).Mul(quantity, rate)
		amount := new(big.Rat).Mul(quantity, price)
		if value.fees.Currency == "quote" {
			fee.Mul(fee, price)
		}
		generatedRatEqual(t, "independent fill fee", fill.Fee, fee)
		filled.Add(filled, quantity)
		notional.Add(notional, amount)
		if engine.Plan.Side == "buy" {
			base.Add(base, quantity)
			quote.Sub(quote, amount)
		} else {
			base.Sub(base, quantity)
			quote.Add(quote, amount)
		}
		if value.fees.Currency == "quote" {
			quote.Sub(quote, fee)
			quoteFees.Add(quoteFees, fee)
			markedFees.Add(markedFees, fee)
		} else {
			base.Sub(base, fee)
			baseFees.Add(baseFees, fee)
			markedFees.Add(markedFees, new(big.Rat).Mul(fee, price))
		}
	}
	remaining := new(big.Rat).Sub(generatedRat(t, engine.Plan.Quantity), filled)
	if base.Sign() < 0 || quote.Sign() < 0 || remaining.Sign() < 0 {
		t.Fatal("negative balance or parent overfill")
	}
	generatedRatEqual(t, "base conservation", engine.Ledger.Base, base)
	generatedRatEqual(t, "quote conservation", engine.Ledger.Quote, quote)
	generatedRatEqual(t, "filled conservation", engine.Ledger.Filled, filled)
	generatedRatEqual(t, "remaining conservation", engine.Ledger.Remaining(), remaining)
	generatedRatEqual(t, "gross notional", engine.Ledger.Notional, notional)
	generatedRatEqual(t, "quote fee conservation", engine.Ledger.FeesQuote, quoteFees)
	generatedRatEqual(t, "base fee conservation", engine.Ledger.FeesBase, baseFees)
	if result.Terminated && result.Truncated || len(result.Observation) != ObservationSize || math.IsNaN(result.Reward) || math.IsInf(result.Reward, 0) {
		t.Fatal("invalid termination, observation shape, or reward")
	}
	for _, feature := range result.Observation {
		if math.IsNaN(float64(feature)) || math.IsInf(float64(feature), 0) {
			t.Fatal("nonfinite generated observation")
		}
	}
	if engine.Child != nil && (engine.Child.Remaining.Sign() < 0 || engine.Child.Remaining.Cmp(engine.Child.Intent.Quantity) > 0) {
		t.Fatal("invalid child obligation")
	}
	report := engine.Report()
	valid := result.Terminated && !result.Truncated
	if report["economic_score_valid"] != valid || (report["terminal_objective"] != nil) != valid || result.Info.Provisional == valid {
		t.Fatal("economic validity and terminal objective disagree")
	}
	initial, residual := generatedRat(t, engine.InitialMid), generatedRat(t, report["residual_midpoint"].(d.Decimal))
	cost := new(big.Rat).Sub(notional, new(big.Rat).Mul(filled, initial))
	cost.Add(cost, new(big.Rat).Mul(remaining, new(big.Rat).Sub(residual, initial)))
	if engine.Plan.Side == "sell" {
		cost.Neg(cost)
	}
	cost.Add(cost, markedFees)
	generatedRatEqual(t, "independent marked economic cost", report["marked_cost_quote"].(d.Decimal), cost)
	if reported := report["reward_sum"].(float64); math.IsNaN(reported) || math.IsInf(reported, 0) || math.Abs(reported-accumulatedReward) > 1e-10 {
		t.Fatal("reported reward sum differs from accumulated step rewards")
	}
	if valid {
		// Compute the terminal score from rational cashflows and the fixture's
		// residual penalty, independently of the engine's objective/reward code.
		target := generatedRat(t, value.plan.Quantity)
		objective := new(big.Rat).Quo(cost, new(big.Rat).Mul(target, initial))
		objective.Mul(objective, big.NewRat(-10000, 1))
		penalty := new(big.Rat).Mul(new(big.Rat).SetFloat64(value.config.LambdaMiss), new(big.Rat).Quo(remaining, target))
		objective.Sub(objective, penalty)
		want, _ := objective.Float64()
		for _, actual := range []float64{report["terminal_objective"].(float64), accumulatedReward} {
			if math.IsNaN(actual) || math.IsInf(actual, 0) || math.Abs(actual-want) > 1e-10 {
				t.Fatalf("terminal objective or accumulated rewards got %.17g, want independent %.17g", actual, want)
			}
		}
	}
}

func checkGeneratedReplay(t testing.TB, seed uint64) {
	t.Helper()
	value := makeGeneratedReplayCase(t, seed)
	inputBefore, err := json.Marshal(value.events)
	if err != nil {
		t.Fatal(err)
	}
	first, err := New(value.events, value.plan, value.rules, value.fees, value.config)
	if err != nil {
		t.Fatalf("seed=%d New: %v", seed, err)
	}
	second, err := New(value.events, value.plan, value.rules, value.fees, value.config)
	if err != nil {
		t.Fatal(err)
	}
	trajectory := make([]StepResult, 0, len(value.actions))
	accumulatedReward := 0.0
	for index, action := range value.actions {
		previousTime, previousFilled := first.Now, generatedRat(t, first.Ledger.Filled)
		got, err := first.Step(action)
		if err != nil {
			t.Fatalf("seed=%d step=%d: %v", seed, index, err)
		}
		again, err := second.Step(action)
		if err != nil || !reflect.DeepEqual(got, again) || !reflect.DeepEqual(first.Report(), second.Report()) {
			t.Fatalf("seed=%d step=%d nondeterminism: %v", seed, index, err)
		}
		if first.Now < previousTime || generatedRat(t, first.Ledger.Filled).Cmp(previousFilled) < 0 {
			t.Fatal("generated receipt time or fills moved backwards")
		}
		accumulatedReward += got.Reward
		checkGeneratedReplayEconomics(t, value, first, got, accumulatedReward)
		trajectory = append(trajectory, got)
	}
	if first.Terminated != value.terminated || first.Truncated == value.terminated || first.Status != value.status || first.Now != value.endTime {
		t.Fatalf("seed=%d outcome: terminated=%v truncated=%v status=%s now=%d", seed, first.Terminated, first.Truncated, first.Status, first.Now)
	}
	if !first.Ledger.Filled.Equal(value.filled) {
		t.Fatalf("seed=%d filled=%s want=%s", seed, first.Ledger.Filled, value.filled)
	}
	if !first.Report()["residual_midpoint"].(d.Decimal).Equal(value.residual) {
		t.Fatalf("seed=%d residual midpoint changed after its decision-time freeze", seed)
	}
	if len(first.Orders) != len(value.orderStates) {
		t.Fatalf("seed=%d order count=%d want=%d", seed, len(first.Orders), len(value.orderStates))
	}
	for index, state := range value.orderStates {
		if first.Orders[index].State != state {
			t.Fatalf("seed=%d order=%d state=%s want=%s", seed, index, first.Orders[index].State, state)
		}
	}
	terminalReport, _ := json.Marshal(first.Report())
	if _, err := first.Step(d.Hold); err == nil {
		t.Fatal("ended generated episode accepted another step")
	}
	afterRejectedStep, _ := json.Marshal(first.Report())
	if !bytes.Equal(terminalReport, afterRejectedStep) {
		t.Fatal("rejected post-terminal step mutated the episode")
	}
	if _, err := first.Reset(); err != nil {
		t.Fatal(err)
	}
	for index, action := range value.actions {
		got, err := first.Step(action)
		if err != nil || !reflect.DeepEqual(got, trajectory[index]) {
			t.Fatalf("seed=%d reset step=%d changed trajectory: %v", seed, index, err)
		}
	}
	afterReset, _ := json.Marshal(first.Report())
	inputAfter, _ := json.Marshal(value.events)
	if !bytes.Equal(terminalReport, afterReset) || !bytes.Equal(inputBefore, inputAfter) {
		t.Fatal("reset changed terminal report or replay mutated caller-owned fixture")
	}
}

func TestGeneratedReceiptCancellationDebtAndGapSequences(t *testing.T) {
	// Cross all five scenarios with before/at/after ties, both execution sides,
	// and both fee currencies; amounts/prices are independently seed-generated.
	for seed := uint64(0); seed < 60; seed++ {
		t.Run(fmt.Sprint(seed), func(t *testing.T) { checkGeneratedReplay(t, seed) })
	}
}

func FuzzGeneratedReplaySequences(f *testing.F) {
	for _, seed := range []uint64{0, 1, 2, 3, 4, 5, 11, 31, 59} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, seed uint64) { checkGeneratedReplay(t, seed) })
}
