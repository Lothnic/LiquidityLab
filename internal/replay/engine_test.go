package replay

import (
	"encoding/json"
	d "github.com/liquiditylab/liquiditylab/internal/domain"
	"math"
	"os"
	"reflect"
	"testing"
)

func dec(s string) d.Decimal { return d.MustDecimal(s) }

func TestSharedTapeOwnsImmutableEvents(t *testing.T) {
	events := SyntheticEvents()
	tape, err := NewTape(events)
	if err != nil {
		t.Fatal(err)
	}
	events[0].Asks[0].Price = dec("999")
	first, err := NewWithTape(tape, d.DefaultPlan(), d.DefaultRules(), d.DefaultFees(), d.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewWithTape(tape, d.DefaultPlan(), d.DefaultRules(), d.DefaultFees(), d.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if &first.events[0] != &second.events[0] {
		t.Fatal("engines copied the shared tape")
	}
	before := second.Observe()
	if _, err := first.Step(d.AggressiveNormal); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, second.Observe()) {
		t.Fatal("independent engine mutated")
	}
	if !first.events[0].Asks[0].Price.Equal(dec("100.01")) {
		t.Fatal("caller mutated tape")
	}
}
func snapshot(at int64, bid, ask string) d.MarketEvent {
	return d.MarketEvent{Time: at, Kind: "snapshot", Bids: []d.Level{{Price: dec(bid), Quantity: dec("2")}}, Asks: []d.Level{{Price: dec(ask), Quantity: dec("2")}}}
}
func setup(t *testing.T, events []d.MarketEvent, p d.Plan, c d.ReplayConfig) *Engine {
	t.Helper()
	fees := d.DefaultFees()
	fees.Maker = d.Zero
	fees.Taker = d.Zero
	e, err := New(events, p, d.DefaultRules(), fees, c)
	if err != nil {
		t.Fatal(err)
	}
	return e
}
func config() d.ReplayConfig {
	c := d.DefaultConfig()
	c.SubmissionLatency = 0
	c.CancelLatency = 0
	c.MinMessageInterval = 0
	c.SettlementAllowance = d.Second
	return c
}
func plan() d.Plan {
	p := d.DefaultPlan()
	p.Deadline = 2 * d.Second
	p.LimitPrice = dec("101")
	return p
}
func TestReceiptArrivalAndCancellationTies(t *testing.T) {
	p := plan()
	p.Quantity = dec(".2")
	p.MaxChildQuantity = p.Quantity
	p.Deadline = d.Second
	p.LimitPrice = dec("99")
	c := config()
	c.CancelLatency = d.Second / 2
	events := []d.MarketEvent{snapshot(0, "99", "101"), {Time: 3 * d.Second / 2, Kind: "trade", Side: "sell", Price: dec("99"), Quantity: dec("2.1"), TradeID: "race"}, snapshot(7*d.Second/4, "109", "111"), {Time: 18 * d.Second / 10, Kind: "gap"}, snapshot(2*d.Second, "109", "111")}
	e := setup(t, events, p, c)
	result, err := e.Step(d.PassiveNormal)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Terminated || result.Truncated || !e.Ledger.Filled.Equal(dec(".1")) || e.Now != 3*d.Second/2 {
		t.Fatal(result, e.Now, e.Ledger.Filled)
	}
	if !e.Report()["residual_midpoint"].(d.Decimal).Equal(dec("100")) {
		t.Fatal(e.Report())
	}
	if e.Orders[0].State != "cancelled" {
		t.Fatal(e.Orders)
	}
	if _, err = e.Step(d.Hold); err == nil {
		t.Fatal("terminal stepped twice")
	}
}
func TestArrivalReceiptOrdering(t *testing.T) {
	p := plan()
	p.Deadline = d.Second
	c := config()
	c.SubmissionLatency = d.Second / 2
	events := []d.MarketEvent{snapshot(0, "99", "101"), {Time: d.Second / 2, Kind: "trade", Side: "sell", Price: dec("99"), Quantity: dec("3"), TradeID: "same-time"}, snapshot(2*d.Second, "99", "101")}
	e := setup(t, events, p, c)
	if _, err := e.Step(d.PassiveNormal); err != nil {
		t.Fatal(err)
	}
	if !e.Ledger.Filled.IsZero() {
		t.Fatal("receipt at arrival filled before queue existed")
	}
}
func TestUnknownSettlementHasNoScore(t *testing.T) {
	p := plan()
	p.Deadline = d.Second
	p.LimitPrice = dec("99")
	c := config()
	c.CancelLatency = d.Second
	c.SettlementAllowance = d.Second / 4
	e := setup(t, []d.MarketEvent{snapshot(0, "99", "101"), snapshot(2*d.Second, "99", "101")}, p, c)
	r, err := e.Step(d.PassiveNormal)
	if err != nil || !r.Truncated || r.Terminated {
		t.Fatal(r, err)
	}
	if e.Report()["terminal_objective"] != nil || e.Report()["economic_score_valid"] != false {
		t.Fatal(e.Report())
	}
	if e.Child == nil || e.Child.Remaining.IsZero() {
		t.Fatal("lost unresolved reservation")
	}
}
func TestCompletionSkipsLaterGapAndResetsExactly(t *testing.T) {
	p := plan()
	c := config()
	c.SubmissionLatency = d.Second / 20
	events := []d.MarketEvent{snapshot(0, "99", "101"), {Time: d.Second / 2, Kind: "gap"}, snapshot(3*d.Second, "99", "101")}
	e := setup(t, events, p, c)
	first, err := e.Step(d.AggressiveNormal)
	if err != nil || !first.Terminated || first.Truncated || e.Now != d.Second/20 {
		t.Fatal(first, err)
	}
	report := e.Report()
	e.Reset()
	again, err := e.Step(d.AggressiveNormal)
	if err != nil || !reflect.DeepEqual(first, again) || !reflect.DeepEqual(report, e.Report()) {
		t.Fatal("reset changed trajectory", err)
	}
}
func TestFeatureCausalityAndVolatility(t *testing.T) {
	p := plan()
	p.Deadline = 3 * d.Second
	c := config()
	events := []d.MarketEvent{snapshot(0, "99", "101"), snapshot(d.Second, "100", "102"), snapshot(2*d.Second, "102", "104"), snapshot(4*d.Second, "99", "101")}
	e := setup(t, events, p, c)
	if e.Observe()[10] != 0 || e.Observe()[11] != 0 {
		t.Fatal(e.Observe())
	}
	r, _ := e.Step(d.Hold)
	if math.Abs(float64(r.Observation[10])-.01) > 1e-8 || r.Observation[11] != 0 {
		t.Fatal(r.Observation)
	}
	r, _ = e.Step(d.Hold)
	expected := math.Abs(math.Log(101.0/100)-math.Log(103.0/101)) / 2
	if math.Abs(float64(r.Observation[11])-expected) > 1e-8 {
		t.Fatal(r.Observation[11], expected)
	}
}
func TestConsumedDepthPersistsAndIsolation(t *testing.T) {
	p := plan()
	p.Quantity = dec(".3")
	p.MaxChildQuantity = dec(".1")
	p.Deadline = 3 * d.Second
	c := config()
	c.MaxParticipation = d.One
	initial := snapshot(0, "99", "101")
	initial.Asks = []d.Level{{Price: dec("101"), Quantity: dec(".1")}, {Price: dec("102"), Quantity: dec("2")}}
	events := []d.MarketEvent{initial, {Time: d.Second, Kind: "book", Asks: []d.Level{{Price: dec("101"), Quantity: dec(".1")}}}, {Time: 15 * d.Second / 10, Kind: "book", Asks: []d.Level{{Price: dec("101"), Quantity: d.Zero}}}, {Time: 16 * d.Second / 10, Kind: "book", Asks: []d.Level{{Price: dec("101"), Quantity: dec(".1")}}}, snapshot(4*d.Second, "99", "101")}
	first := setup(t, events, p, c)
	second := setup(t, events, p, c)
	for i, want := range []string{".1", ".1", ".2"} {
		if _, err := first.Step(d.AggressiveNormal); err != nil {
			t.Fatal(i, err)
		}
		if !first.Ledger.Filled.Equal(dec(want)) {
			t.Fatal(i, first.Ledger.Filled)
		}
	}
	if !second.Ledger.Filled.IsZero() || !second.Book.Visible("asks").Equal(dec("2.1")) {
		t.Fatal("environments share state")
	}
}
func TestTimerOverflowRejected(t *testing.T) {
	p := plan()
	p.Deadline = math.MaxInt64
	c := config()
	if _, err := New([]d.MarketEvent{snapshot(0, "99", "101"), snapshot(math.MaxInt64, "99", "101")}, p, d.DefaultRules(), d.DefaultFees(), c); err == nil {
		t.Fatal("overflow accepted")
	}
}

// The approved Python fixtures are loaded directly. Decimal formatting and
// version/hash metadata differ by contract; all economic and outcome fields match.
func TestPythonGoldenReports(t *testing.T) {
	data, err := os.ReadFile("../../conformance/golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var golden struct {
		Cases []struct {
			Name  string `json:"name"`
			Input struct {
				Events []struct {
					Time                  int64  `json:"time_ns"`
					Kind                  string `json:"kind"`
					Bids, Asks            [][]string
					Price, Quantity, Side string
					TradeID               string `json:"trade_id"`
					Valid                 *bool
				}
				Plan    map[string]any
				Config  map[string]any
				Fees    d.FeeConfig
				Actions []int
				Policy  *string
			}
			Expected map[string]json.RawMessage `json:"expected_report"`
		}
	}
	if err = json.Unmarshal(data, &golden); err != nil {
		t.Fatal(err)
	}
	for _, g := range golden.Cases {
		t.Run(g.Name, func(t *testing.T) {
			p := d.DefaultPlan()
			b, _ := json.Marshal(g.Input.Plan)
			json.Unmarshal(b, &p)
			if v, ok := g.Input.Plan["deadline"].(float64); ok {
				p.Deadline = int64(v * float64(d.Second))
			}
			c := d.DefaultConfig()
			b, _ = json.Marshal(g.Input.Config)
			json.Unmarshal(b, &c)
			for name, dst := range map[string]*int64{"decision_interval": &c.DecisionInterval, "submission_latency": &c.SubmissionLatency, "cancel_latency": &c.CancelLatency, "settlement_allowance": &c.SettlementAllowance, "min_resting_age": &c.MinRestingAge, "min_message_interval": &c.MinMessageInterval} {
				if v, ok := g.Input.Config[name].(float64); ok {
					*dst = int64(v * float64(d.Second))
				}
			}
			events := []d.MarketEvent{}
			for i, v := range g.Input.Events {
				event := d.MarketEvent{Time: v.Time, Kind: v.Kind, Price: d.Zero, Quantity: d.Zero, Side: v.Side, TradeID: v.TradeID, ReceiptIndex: uint64(i), Invalid: v.Valid != nil && !*v.Valid}
				if v.Price != "" {
					event.Price = dec(v.Price)
				}
				if v.Quantity != "" {
					event.Quantity = dec(v.Quantity)
				}
				for _, l := range v.Bids {
					event.Bids = append(event.Bids, d.Level{Price: dec(l[0]), Quantity: dec(l[1])})
				}
				for _, l := range v.Asks {
					event.Asks = append(event.Asks, d.Level{Price: dec(l[0]), Quantity: dec(l[1])})
				}
				events = append(events, event)
			}
			e, err := New(events, p, d.DefaultRules(), g.Input.Fees, c)
			if err != nil {
				t.Fatal(err)
			}
			if g.Input.Policy != nil {
				_, err = RunPolicy(e, *g.Input.Policy)
			} else {
				for _, legacy := range g.Input.Actions {
					action, _ := d.ActionFromLegacy(legacy)
					_, err = e.Step(action)
					if err != nil {
						break
					}
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			got := e.Report()
			for _, field := range []string{"initial_midpoint", "residual_midpoint", "filled_quantity", "remaining_quantity", "completion_fraction", "average_price", "gross_notional", "fees_quote", "fees_base", "net_base_change", "base_balance", "quote_balance", "marked_cost_quote"} {
				var expected string
				if err = json.Unmarshal(g.Expected[field], &expected); err != nil {
					t.Fatal(field, err)
				}
				if !got[field].(d.Decimal).Equal(dec(expected)) {
					t.Errorf("%s got %s want %s", field, got[field], expected)
				}
			}
			for _, field := range []string{"status", "terminated", "truncated", "economic_score_valid", "events_processed", "time_ns"} {
				var expected any
				json.Unmarshal(g.Expected[field], &expected)
				if !reflect.DeepEqual(got[field], expected) {
					if n, ok := expected.(float64); ok {
						b, _ := json.Marshal(got[field])
						var actual float64
						json.Unmarshal(b, &actual)
						if actual == n {
							continue
						}
					}
					t.Errorf("%s got %v want %v", field, got[field], expected)
				}
			}
			for _, field := range []string{"reward_sum", "terminal_objective"} {
				var expected *float64
				json.Unmarshal(g.Expected[field], &expected)
				if expected == nil {
					if got[field] != nil {
						t.Fatal(field, got[field])
					}
				} else if math.Abs(got[field].(float64)-*expected) > 1e-10 {
					t.Fatal(field, got[field], *expected)
				}
			}
			compareExecutions(t, g.Expected["fills"], e.Ledger.Fills)
			compareOrdersAndDecisions(t, g.Expected["orders"], g.Expected["decisions"], e.Orders, e.Decisions)
			if e.Terminated && math.Abs(e.rewardSum-got["terminal_objective"].(float64)) > 1e-10 {
				t.Fatal("terminal objective does not reconcile")
			}
		})
	}
}
func compareExecutions(t *testing.T, raw json.RawMessage, actual []d.Fill) {
	t.Helper()
	var expected []d.Fill
	if err := json.Unmarshal(raw, &expected); err != nil {
		t.Fatal(err)
	}
	if len(expected) != len(actual) {
		t.Fatal("fill count", len(actual), len(expected))
	}
	for i, f := range expected {
		if !f.Equal(actual[i]) {
			t.Fatal("fill mismatch", f, actual[i])
		}
	}
}

func compareOrdersAndDecisions(t *testing.T, rawOrders, rawDecisions json.RawMessage, orders []*d.ChildOrder, decisions []Decision) {
	t.Helper()
	var expectedOrders []d.ChildOrder
	var expectedDecisions []Decision
	if err := json.Unmarshal(rawOrders, &expectedOrders); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(rawDecisions, &expectedDecisions); err != nil {
		t.Fatal(err)
	}
	if len(expectedOrders) != len(orders) || len(expectedDecisions) != len(decisions) {
		t.Fatal("order or decision count differs")
	}
	for i, w := range expectedOrders {
		g := orders[i]
		if w.OrderID != g.OrderID || !w.Intent.Equal(g.Intent) || w.SubmittedAt != g.SubmittedAt || w.ArrivalAt != g.ArrivalAt || !w.Remaining.Equal(g.Remaining) || w.State != g.State || !w.QueueAhead.Equal(g.QueueAhead) || !reflect.DeepEqual(w.CancelAt, g.CancelAt) {
			t.Fatal("order outcome differs", w, g)
		}
	}
	for i, w := range expectedDecisions {
		w.Action++
		if w != decisions[i] {
			t.Fatal("decision differs", w, decisions[i])
		}
	}
}
