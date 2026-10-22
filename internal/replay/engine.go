// Package replay implements deterministic simulation without clocks, I/O or shared state.
package replay

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	d "github.com/liquiditylab/liquiditylab/internal/domain"
	x "github.com/liquiditylab/liquiditylab/internal/execution"
	"math"
	"strconv"
)

const ObservationVersion = "compact-v1"
const ObservationSize = 20

var ObservationFields = []string{"remaining_fraction", "time_remaining_fraction", "filled_fraction", "average_fill_to_mid_ratio", "reserved_fraction", "free_quote_fraction", "signed_limit_distance", "spread_bps", "bid_depth_to_target", "ask_depth_to_target", "recent_mid_return", "recent_log_return_std", "cumulative_trade_flow_to_target", "signed_child_price_distance", "child_age_fraction", "child_remaining_fraction", "has_child", "market_valid", "simulated_account_valid", "child_pending"}

type Report map[string]any
type Info struct {
	Status      string    `json:"status"`
	Reason      string    `json:"reason"`
	Time        int64     `json:"time_ns"`
	Filled      d.Decimal `json:"filled"`
	Remaining   d.Decimal `json:"remaining"`
	Provisional bool      `json:"provisional"`
	Report      *Report   `json:"report,omitempty"`
}
type StepResult struct {
	Observation           []float32
	Reward                float64
	Terminated, Truncated bool
	Info                  Info
}
type Decision struct {
	Time   int64    `json:"time_ns"`
	Action d.Action `json:"action"`
	Reason string   `json:"reason"`
}
type Engine struct {
	Plan                             d.Plan
	Rules                            d.InstrumentRules
	Fees                             d.FeeConfig
	Config                           d.ReplayConfig
	Book                             *x.Book
	Ledger                           *x.Ledger
	Child                            *d.ChildOrder
	Now                              int64
	Terminated, Truncated            bool
	Status, Reason                   string
	InitialMid                       d.Decimal
	events                           []d.MarketEvent
	DatasetHash                      string
	Orders                           []*d.ChildOrder
	Decisions                        []Decision
	index, orderCounter, fillCounter int
	seenTrades                       map[string]bool
	lastMessage                      int64
	hasMessage                       bool
	previousCost, lastMid, tradeFlow d.Decimal
	frozenMid                        *d.Decimal
	rewardSum                        float64
	decisionMids                     []d.Decimal
}

// Tape holds one private, immutable copy of market events shared by independent
// engines. Its contents are never exposed to callers or mutated during replay.
type Tape struct {
	events []d.MarketEvent
	hash   string
}

func New(events []d.MarketEvent, plan d.Plan, rules d.InstrumentRules, fees d.FeeConfig, config d.ReplayConfig) (*Engine, error) {
	tape, err := NewTape(events)
	if err != nil {
		return nil, err
	}
	return NewWithTape(tape, plan, rules, fees, config)
}

// NewTape validates and copies one recording. Plan-specific coverage and symbol
// checks are performed by NewWithTape before an engine becomes usable.
func NewTape(events []d.MarketEvent) (*Tape, error) {
	if len(events) == 0 || events[0].Time != 0 || events[0].Kind != "snapshot" {
		return nil, errors.New("episode must begin with snapshot at receipt time zero")
	}
	previous := int64(0)
	copied := make([]d.MarketEvent, len(events))
	for i, event := range events {
		if event.Time < previous || event.Time < 0 {
			return nil, errors.New("events must preserve nonnegative receipt order")
		}
		if event.Kind != "snapshot" && event.Kind != "book" && event.Kind != "trade" && event.Kind != "gap" {
			return nil, errors.New("unsupported market event kind")
		}
		for _, levels := range [][]d.Level{event.Bids, event.Asks} {
			for _, level := range levels {
				if level.Price.Sign() <= 0 || level.Quantity.Sign() < 0 {
					return nil, errors.New("invalid book level")
				}
			}
		}
		if event.Kind == "trade" && !event.Invalid && (event.Quantity.Sign() <= 0 || event.Price.Sign() <= 0 || (event.Side != "buy" && event.Side != "sell")) {
			return nil, errors.New("invalid trade")
		}
		if i > 0 && event.ReceiptIndex == 0 {
			event.ReceiptIndex = uint64(i)
		}
		event.Bids = append([]d.Level(nil), event.Bids...)
		event.Asks = append([]d.Level(nil), event.Asks...)
		copied[i] = event
		previous = event.Time
	}
	b, err := json.Marshal(copied)
	if err != nil {
		return nil, err
	}
	h := sha256.Sum256(b)
	return &Tape{events: copied, hash: hex.EncodeToString(h[:])}, nil
}

func NewWithTape(tape *Tape, plan d.Plan, rules d.InstrumentRules, fees d.FeeConfig, config d.ReplayConfig) (*Engine, error) {
	for _, err := range []error{plan.Validate(), rules.Validate(), fees.Validate(), config.Validate()} {
		if err != nil {
			return nil, err
		}
	}
	if plan.Symbol != rules.Symbol {
		return nil, errors.New("plan and instrument symbol differ")
	}
	if config.SettlementAllowance > math.MaxInt64-plan.Deadline {
		return nil, errors.New("deadline plus settlement allowance overflows")
	}
	end := plan.Deadline + config.SettlementAllowance
	if config.SubmissionLatency > math.MaxInt64-end || config.CancelLatency > math.MaxInt64-end {
		return nil, errors.New("timer addition overflows")
	}
	if tape == nil || len(tape.events) == 0 {
		return nil, errors.New("empty tape")
	}
	for _, event := range tape.events {
		if event.Symbol != "" && event.Symbol != plan.Symbol {
			return nil, errors.New("recording contains another symbol")
		}
	}
	if tape.events[len(tape.events)-1].Time < end {
		return nil, errors.New("recording must cover deadline plus settlement allowance")
	}
	e := &Engine{Plan: plan, Rules: rules, Fees: fees, Config: config, events: tape.events, DatasetHash: tape.hash}
	_, err := e.Reset()
	return e, err
}
func (e *Engine) done() bool { return e.Terminated || e.Truncated }
func (e *Engine) Reset() (StepResult, error) {
	e.Book = x.NewBook()
	e.Ledger = x.NewLedger(e.Plan)
	e.Child = nil
	e.Now = 0
	e.index = 0
	e.orderCounter = 0
	e.fillCounter = 0
	e.seenTrades = map[string]bool{}
	e.hasMessage = false
	e.previousCost = d.Zero
	e.rewardSum = 0
	e.lastMid = d.Zero
	e.frozenMid = nil
	e.Terminated = false
	e.Truncated = false
	e.Status = "running"
	e.Reason = "reset"
	e.Decisions = []Decision{}
	e.Orders = []*d.ChildOrder{}
	e.tradeFlow = d.Zero
	if err := e.advance(0, false, false); err != nil {
		return StepResult{}, err
	}
	if !e.Book.Valid || e.Truncated {
		return StepResult{}, errors.New("initial snapshot is invalid or crossed")
	}
	e.InitialMid = e.Book.Mid()
	e.lastMid = e.InitialMid
	e.decisionMids = []d.Decimal{e.InitialMid}
	return e.result(0), nil
}
func (e *Engine) event(event d.MarketEvent) error {
	if event.Kind != "trade" {
		if err := e.Book.Apply(event); err != nil {
			return err
		}
		if e.Book.Valid {
			e.lastMid = e.Book.Mid()
		} else {
			e.truncate("invalid_market_data")
		}
		return nil
	}
	if event.Invalid {
		e.truncate("invalid_market_data")
		return nil
	}
	identity := event.TradeID
	if identity == "" {
		identity = "receipt:" + strconv.FormatUint(event.ReceiptIndex, 10)
	}
	if e.seenTrades[identity] {
		return nil
	}
	e.seenTrades[identity] = true
	if event.Side == "buy" {
		e.tradeFlow = e.tradeFlow.Add(event.Quantity)
	} else {
		e.tradeFlow = e.tradeFlow.Sub(event.Quantity)
	}
	c := e.Child
	if c == nil || (c.State != "open" && c.State != "cancel_requested") {
		return nil
	}
	qualifying := c.Intent.Side == "buy" && event.Side == "sell" || c.Intent.Side == "sell" && event.Side == "buy"
	if !qualifying || !event.Price.Equal(c.Intent.Price) {
		return nil
	}
	ahead := d.Min(event.Quantity, c.QueueAhead)
	c.QueueAhead = c.QueueAhead.Sub(ahead)
	volume := event.Quantity.Sub(ahead)
	if volume.Sign() > 0 {
		return e.fill(d.Min(volume, c.Remaining), c.Intent.Price, true)
	}
	return nil
}
func (e *Engine) fill(q, p d.Decimal, maker bool) error {
	c := e.Child
	if c == nil || q.Sign() <= 0 {
		return nil
	}
	rate := e.Fees.Taker
	if maker {
		rate = e.Fees.Maker
	}
	fee := q.Mul(rate)
	if e.Fees.Currency == "quote" {
		fee = fee.Mul(p)
	}
	e.fillCounter++
	f := d.Fill{ExecutionID: fmt.Sprintf("sim-%d", e.fillCounter), OrderID: c.OrderID, Side: e.Plan.Side, Quantity: q, Price: p, Fee: fee, FeeCurrency: e.Fees.Currency, Time: e.Now, Maker: maker}
	if _, err := e.Ledger.Apply(f); err != nil {
		return err
	}
	c.Remaining = c.Remaining.Sub(q)
	if c.Remaining.IsZero() {
		c.State = "filled"
		e.Child = nil
	}
	return nil
}
func (e *Engine) arrive() error {
	c := e.Child
	if c == nil {
		return nil
	}
	if !e.Book.Valid {
		c.State = "rejected"
		e.Child = nil
		return nil
	}
	intent := c.Intent
	if intent.Passive {
		crosses := intent.Side == "buy" && intent.Price.Cmp(e.Book.BestAsk()) >= 0 || intent.Side == "sell" && intent.Price.Cmp(e.Book.BestBid()) <= 0
		if crosses {
			c.State = "rejected_post_only"
			e.Child = nil
			return nil
		}
		side := "asks"
		if intent.Side == "buy" {
			side = "bids"
		}
		c.QueueAhead = e.Book.Available(side, intent.Price)
		c.State = "open"
		if c.CancelAt != nil {
			c.State = "cancel_requested"
		}
		return nil
	}
	side := "bids"
	if intent.Side == "buy" {
		side = "asks"
	}
	for _, p := range e.Book.Prices(side) {
		if intent.Side == "buy" && p.Cmp(intent.Price) > 0 || intent.Side == "sell" && p.Cmp(intent.Price) < 0 {
			break
		}
		q := d.Min(c.Remaining, e.Book.Available(side, p))
		if err := e.fill(q, p, false); err != nil {
			return err
		}
		e.Book.Consume(side, p, q)
		if e.Child == nil {
			break
		}
	}
	if e.Child != nil {
		e.Child.State = "ioc_expired"
		e.Child = nil
	}
	return nil
}
func (e *Engine) advance(target int64, stopSettled, stopComplete bool) error {
	for !e.done() {
		if stopSettled && e.Child == nil || stopComplete && e.Ledger.Remaining().IsZero() && e.Child == nil {
			return nil
		}
		market, arrival, cancel := int64(math.MaxInt64), int64(math.MaxInt64), int64(math.MaxInt64)
		if e.index < len(e.events) {
			market = e.events[e.index].Time
		}
		c := e.Child
		if c != nil {
			if c.State == "submitting" || c.State == "cancel_requested" && e.Now < c.ArrivalAt {
				arrival = c.ArrivalAt
			}
			if c.CancelAt != nil {
				cancel = *c.CancelAt
			}
		}
		next := min(market, arrival, cancel)
		if next > target || next == math.MaxInt64 {
			break
		}
		e.Now = next
		if market == next {
			event := e.events[e.index]
			e.index++
			if err := e.event(event); err != nil {
				return err
			}
		} else if arrival == next {
			if err := e.arrive(); err != nil {
				return err
			}
		} else if e.Child != nil {
			e.Child.State = "cancelled"
			e.Child = nil
		}
	}
	if !e.done() {
		e.Now = target
	}
	return nil
}
func (e *Engine) cancel() {
	if e.Child != nil && e.Child.CancelAt == nil {
		at := e.Now + e.Config.CancelLatency
		e.Child.CancelAt = &at
		if e.Child.State != "submitting" {
			e.Child.State = "cancel_requested"
		}
		e.lastMessage = e.Now
		e.hasMessage = true
	}
}
func (e *Engine) truncate(reason string) { e.Truncated = true; e.Status = reason; e.Reason = reason }
func (e *Engine) settle(reason string) error {
	mid := e.lastMid
	e.frozenMid = &mid
	e.Status = "settling"
	e.cancel()
	if e.Child != nil {
		if err := e.advance(e.Now+e.Config.SettlementAllowance, true, false); err != nil {
			return err
		}
	}
	if e.Truncated {
		return nil
	}
	if e.Child != nil {
		e.truncate("unresolved_settlement")
		return nil
	}
	e.Terminated = true
	e.Status = "deadline_partial"
	if e.Ledger.Remaining().IsZero() {
		e.Status = "completed"
	}
	e.Reason = reason
	return nil
}
func (e *Engine) Step(action d.Action) (StepResult, error) {
	if e.done() {
		return StepResult{}, errors.New("episode has ended; call reset")
	}
	compiled, err := x.Compile(action, e.Plan, e.Rules, e.Fees, e.Config, e.Ledger, e.Book, e.Child, e.Now)
	if err != nil {
		return StepResult{}, err
	}
	if (compiled.Cancel || compiled.Intent != nil) && e.hasMessage && e.Now-e.lastMessage < e.Config.MinMessageInterval {
		e.Reason = "message_budget"
	} else {
		e.Reason = compiled.Reason
		if compiled.Cancel {
			e.cancel()
		} else if compiled.Intent != nil {
			e.orderCounter++
			e.Child = &d.ChildOrder{OrderID: fmt.Sprintf("child-%d", e.orderCounter), Intent: *compiled.Intent, SubmittedAt: e.Now, ArrivalAt: e.Now + e.Config.SubmissionLatency, Remaining: compiled.Intent.Quantity, State: "submitting"}
			e.Orders = append(e.Orders, e.Child)
			e.lastMessage = e.Now
			e.hasMessage = true
		}
	}
	e.Decisions = append(e.Decisions, Decision{e.Now, action, e.Reason})
	target := e.Plan.Deadline
	if e.Config.DecisionInterval < e.Plan.Deadline-e.Now {
		target = e.Now + e.Config.DecisionInterval
	}
	if err = e.advance(target, false, true); err != nil {
		return StepResult{}, err
	}
	if !e.done() && (e.Ledger.Remaining().IsZero() || e.Now >= e.Plan.Deadline) {
		reason := "deadline"
		if e.Ledger.Remaining().IsZero() {
			reason = "target_filled"
		}
		if err = e.settle(reason); err != nil {
			return StepResult{}, err
		}
	}
	mid := e.lastMid
	if e.frozenMid != nil {
		mid = *e.frozenMid
	}
	if !e.Truncated {
		e.decisionMids = append(e.decisionMids, mid)
		if len(e.decisionMids) > 11 {
			e.decisionMids = e.decisionMids[len(e.decisionMids)-11:]
		}
	}
	cost := e.Ledger.MarkedCost(e.InitialMid, mid)
	reward := -10000 * cost.Sub(e.previousCost).Quo(e.Plan.Quantity.Mul(e.InitialMid)).Float64()
	if e.Terminated {
		reward -= e.Config.LambdaMiss * e.Ledger.Remaining().Quo(e.Plan.Quantity).Float64()
	}
	e.previousCost = cost
	e.rewardSum += reward
	return e.result(reward), nil
}
func (e *Engine) result(reward float64) StepResult {
	return StepResult{e.Observe(), reward, e.Terminated, e.Truncated, e.Info()}
}
func (e *Engine) Info() Info {
	i := Info{Status: e.Status, Reason: e.Reason, Time: e.Now, Filled: e.Ledger.Filled, Remaining: e.Ledger.Remaining(), Provisional: !e.Terminated}
	if e.done() {
		r := e.Report()
		i.Report = &r
	}
	return i
}
