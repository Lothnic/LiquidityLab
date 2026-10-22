package domain

import (
	"errors"
	"math"
)

const Second int64 = 1_000_000_000
const EnvironmentVersion = "LiquidityLab-SpotExecution-v1"

type Action int32

const (
	Unspecified Action = iota
	Hold
	CancelChild
	PassiveSmall
	PassiveNormal
	AggressiveSmall
	AggressiveNormal
)

func (a Action) Validate() error {
	if a < Hold || a > AggressiveNormal {
		return errors.New("unknown semantic action")
	}
	return nil
}
func ActionFromLegacy(v int) (Action, error) { a := Action(v + 1); return a, a.Validate() }

type InstrumentRules struct {
	Symbol      string  `json:"symbol"`
	TickSize    Decimal `json:"tick_size"`
	LotSize     Decimal `json:"lot_size"`
	MinQuantity Decimal `json:"min_quantity"`
	MinNotional Decimal `json:"min_notional"`
}

func DefaultRules() InstrumentRules {
	return InstrumentRules{"BTC/USD", MustDecimal(".01"), MustDecimal(".001"), MustDecimal(".001"), MustDecimal(".01")}
}
func (r InstrumentRules) Validate() error {
	if r.Symbol == "" || Min(r.TickSize, r.LotSize, r.MinQuantity, r.MinNotional).Sign() <= 0 {
		return errors.New("invalid instrument rules")
	}
	return nil
}

type FeeConfig struct {
	Maker    Decimal `json:"maker"`
	Taker    Decimal `json:"taker"`
	Currency string  `json:"currency"`
	Source   string  `json:"source"`
}

func DefaultFees() FeeConfig {
	return FeeConfig{MustDecimal(".001"), MustDecimal(".002"), "quote", "synthetic-assumption"}
}
func (f FeeConfig) Validate() error {
	if Min(f.Maker, f.Taker).Sign() < 0 || Max(f.Maker, f.Taker).Cmp(One) >= 0 || (f.Currency != "base" && f.Currency != "quote") {
		return errors.New("invalid fee configuration")
	}
	return nil
}

type Plan struct {
	Side             string  `json:"side"`
	Quantity         Decimal `json:"quantity"`
	LimitPrice       Decimal `json:"limit_price"`
	Deadline         int64   `json:"deadline_ns"`
	MaxChildQuantity Decimal `json:"max_child_quantity"`
	QuoteBudget      Decimal `json:"quote_budget"`
	BaseBalance      Decimal `json:"base_balance"`
	Symbol           string  `json:"symbol"`
}

func DefaultPlan() Plan {
	return Plan{"buy", MustDecimal(".1"), MustDecimal("102"), 30 * Second, MustDecimal(".1"), MustDecimal("1000"), One, "BTC/USD"}
}
func (p Plan) Validate() error {
	if (p.Side != "buy" && p.Side != "sell") || Min(p.Quantity, p.LimitPrice, p.MaxChildQuantity).Sign() <= 0 || Min(p.QuoteBudget, p.BaseBalance).Sign() < 0 || p.Deadline <= 0 || p.Symbol == "" {
		return errors.New("invalid execution plan")
	}
	return nil
}

type ReplayConfig struct {
	DecisionInterval    int64   `json:"decision_interval_ns"`
	SubmissionLatency   int64   `json:"submission_latency_ns"`
	CancelLatency       int64   `json:"cancel_latency_ns"`
	SettlementAllowance int64   `json:"settlement_allowance_ns"`
	LambdaMiss          float64 `json:"lambda_miss"`
	MaxParticipation    Decimal `json:"max_participation"`
	MinRestingAge       int64   `json:"min_resting_age_ns"`
	MinMessageInterval  int64   `json:"min_message_interval_ns"`
	FillModel           string  `json:"fill_model"`
}

func DefaultConfig() ReplayConfig {
	return ReplayConfig{Second, 50_000_000, 50_000_000, 30 * Second, 100, MustDecimal(".25"), 2 * Second, 100_000_000, "conservative-v0"}
}
func (c ReplayConfig) Validate() error {
	if c.DecisionInterval <= 0 || c.SubmissionLatency < 0 || c.CancelLatency < 0 || c.SettlementAllowance < 0 || c.MinRestingAge < 0 || c.MinMessageInterval < 0 || c.LambdaMiss < 0 || math.IsNaN(c.LambdaMiss) || math.IsInf(c.LambdaMiss, 0) || c.MaxParticipation.Sign() <= 0 || c.MaxParticipation.Cmp(One) > 0 {
		return errors.New("invalid replay configuration")
	}
	return nil
}

type OrderIntent struct {
	Side     string  `json:"side"`
	Quantity Decimal `json:"quantity"`
	Price    Decimal `json:"price"`
	Passive  bool    `json:"passive"`
}

func (i OrderIntent) Equal(j OrderIntent) bool {
	return i.Side == j.Side && i.Quantity.Equal(j.Quantity) && i.Price.Equal(j.Price) && i.Passive == j.Passive
}

type ChildOrder struct {
	OrderID     string      `json:"order_id"`
	Intent      OrderIntent `json:"intent"`
	SubmittedAt int64       `json:"submitted_at_ns"`
	ArrivalAt   int64       `json:"arrival_at_ns"`
	Remaining   Decimal     `json:"remaining"`
	State       string      `json:"state"`
	QueueAhead  Decimal     `json:"queue_ahead"`
	CancelAt    *int64      `json:"cancel_at_ns"`
}
type Fill struct {
	ExecutionID string  `json:"execution_id"`
	OrderID     string  `json:"order_id"`
	Side        string  `json:"side"`
	Quantity    Decimal `json:"quantity"`
	Price       Decimal `json:"price"`
	Fee         Decimal `json:"fee"`
	FeeCurrency string  `json:"fee_currency"`
	Time        int64   `json:"time_ns"`
	Maker       bool    `json:"maker"`
}

func (f Fill) Validate() error {
	if f.ExecutionID == "" || f.OrderID == "" || (f.Side != "buy" && f.Side != "sell") || (f.FeeCurrency != "base" && f.FeeCurrency != "quote") || Min(f.Quantity, f.Price).Sign() <= 0 || f.Fee.Sign() < 0 || f.Time < 0 {
		return errors.New("invalid fill")
	}
	return nil
}
func (f Fill) Equal(g Fill) bool {
	return f.ExecutionID == g.ExecutionID && f.OrderID == g.OrderID && f.Side == g.Side && f.Quantity.Equal(g.Quantity) && f.Price.Equal(g.Price) && f.Fee.Equal(g.Fee) && f.FeeCurrency == g.FeeCurrency && f.Time == g.Time && f.Maker == g.Maker
}

type Level struct {
	Price    Decimal `json:"price"`
	Quantity Decimal `json:"quantity"`
}
type MarketEvent struct {
	Time         int64   `json:"time_ns"`
	Kind         string  `json:"kind"`
	Symbol       string  `json:"symbol,omitempty"`
	ReceiptIndex uint64  `json:"receipt_index"`
	Invalid      bool    `json:"invalid,omitempty"`
	Bids         []Level `json:"bids,omitempty"`
	Asks         []Level `json:"asks,omitempty"`
	Price        Decimal `json:"price,omitempty"`
	Quantity     Decimal `json:"quantity,omitempty"`
	Side         string  `json:"side,omitempty"`
	TradeID      string  `json:"trade_id,omitempty"`
}
