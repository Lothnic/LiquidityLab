package api

import (
	"time"

	d "github.com/liquiditylab/liquiditylab/internal/domain"
	x "github.com/liquiditylab/liquiditylab/internal/execution"
	rt "github.com/liquiditylab/liquiditylab/internal/runtime"
	pg "github.com/liquiditylab/liquiditylab/internal/store/postgres"
)

type AccountSummary struct {
	TenantID          string     `json:"tenant_id"`
	AccountID         string     `json:"account_id"`
	Profile           rt.Profile `json:"profile"`
	DataSource        string     `json:"data_source"`
	Revision          int64      `json:"revision"`
	AuthorityRevision int64      `json:"authority_revision"`
	Ready             bool       `json:"ready"`
	RecoveryRequired  bool       `json:"recovery_required"`
	PlanStatus        string     `json:"plan_status,omitempty"`
}
type MarketDTO struct {
	Valid              bool      `json:"valid"`
	SnapshotID         string    `json:"snapshot_id"`
	InstrumentRevision string    `json:"instrument_revision"`
	BestBid            d.Decimal `json:"best_bid"`
	BestAsk            d.Decimal `json:"best_ask"`
}
type PlanDTO struct {
	ID                string     `json:"id"`
	Revision          int64      `json:"revision"`
	Status            string     `json:"status"`
	Started           bool       `json:"started"`
	Start             time.Time  `json:"start"`
	Deadline          time.Time  `json:"deadline"`
	Spec              d.Plan     `json:"spec"`
	Policy            string     `json:"policy"`
	Filled            d.Decimal  `json:"filled"`
	Remaining         d.Decimal  `json:"remaining"`
	AveragePrice      d.Decimal  `json:"average_price"`
	ArrivalMid        d.Decimal  `json:"arrival_mid"`
	BenchmarkValid    bool       `json:"benchmark_valid"`
	ExecutedCostQuote *d.Decimal `json:"executed_cost_quote"`
	FeesBase          d.Decimal  `json:"fees_base"`
	FeesQuote         d.Decimal  `json:"fees_quote"`
}
type ChildDTO struct {
	ID              string        `json:"id"`
	Intent          d.OrderIntent `json:"intent"`
	Remaining       d.Decimal     `json:"remaining"`
	State           string        `json:"state"`
	KnownFinal      bool          `json:"known_final"`
	CancelRequested bool          `json:"cancel_requested"`
}
type AccountDTO struct {
	PolicyRuntime *PolicyRuntimeDTO `json:"policy_runtime,omitempty"`
	PolicyConfig  *PolicyConfigDTO  `json:"policy_config,omitempty"`
	AccountSummary
	BaseCurrency       string            `json:"base_currency"`
	QuoteCurrency      string            `json:"quote_currency"`
	ClockDiscontinuity bool              `json:"clock_discontinuity"`
	Market             MarketDTO         `json:"market"`
	Rules              d.InstrumentRules `json:"rules"`
	Fees               d.FeeConfig       `json:"fees"`
	Balances           []pg.Balance      `json:"balances"`
	// Reserved is the unreflected local exposure only; reflected holds are
	// already represented by the venue-style locked balance.
	Reserved       []ReservedDTO `json:"reserved"`
	Plan           *PlanDTO      `json:"plan"`
	Child          *ChildDTO     `json:"child"`
	Fills          []d.Fill      `json:"fills"`
	FillCount      int           `json:"fill_count"`
	FillsTruncated bool          `json:"fills_truncated"`
}
type ReservedDTO struct {
	Currency string    `json:"currency"`
	Amount   d.Decimal `json:"amount"`
}

func summary(v rt.View) AccountSummary {
	out := AccountSummary{TenantID: v.Account.Key.TenantID, AccountID: v.Account.Key.AccountID, Profile: v.State.Profile, DataSource: v.State.DataSource, Revision: v.Account.Revision, AuthorityRevision: v.AuthorityRevision(), Ready: v.Account.Ready, RecoveryRequired: v.State.RecoveryRequired}
	if v.State.Parent != nil {
		out.PlanStatus = v.State.Parent.Status
	}
	return out
}
func market(v rt.View) MarketDTO {
	out := MarketDTO{Valid: v.Account.Market.Valid && v.State.Book != nil && v.State.Book.Valid, SnapshotID: v.State.MarketID, InstrumentRevision: v.State.InstrumentRevision}
	if v.State.Book != nil {
		out.BestBid = v.State.Book.BestBid()
		out.BestAsk = v.State.Book.BestAsk()
	}
	return out
}
func project(v rt.View) (out AccountDTO, err error) {
	defer func() {
		if recover() != nil {
			out = AccountDTO{}
			err = rt.ErrInvalid
		}
	}()
	if len(v.Account.Balances) > 128 || len(v.Account.Reservations) > 2048 || len(v.State.Fills) > 16384 {
		return out, rt.ErrInvalid
	}
	out = AccountDTO{AccountSummary: summary(v), BaseCurrency: v.State.BaseCurrency, QuoteCurrency: v.State.QuoteCurrency, ClockDiscontinuity: v.State.ClockDiscontinuity, Market: market(v), Rules: v.State.Rules, Fees: v.State.Fees, Balances: append([]pg.Balance{}, v.Account.Balances...), Reserved: []ReservedDTO{}, Fills: []d.Fill{}, FillCount: len(v.State.Fills)}
	for _, balance := range out.Balances {
		reserved := d.Zero
		for _, entry := range v.Account.Reservations {
			if entry.Currency == balance.Currency && !entry.Reflected && entry.State != "released" {
				reserved = reserved.Add(entry.Amount)
			}
		}
		out.Reserved = append(out.Reserved, ReservedDTO{Currency: balance.Currency, Amount: reserved})
	}
	out.PolicyRuntime = policyProjection(v.State)
	out.PolicyConfig = policyConfigProjection(v.State)
	if p := v.State.Parent; p != nil {
		ledger := x.NewLedger(p.Spec)
		for _, fill := range v.State.Fills {
			if _, err = ledger.Apply(fill); err != nil {
				return AccountDTO{}, err
			}
		}
		out.Plan = &PlanDTO{ArrivalMid: p.ArrivalMid, BenchmarkValid: p.ArrivalMid.Sign() > 0, ID: p.ID, Revision: p.Revision, Status: p.Status, Started: p.Started, Start: p.Start, Deadline: p.Deadline, Spec: p.Spec, Policy: p.Baseline, Filled: ledger.Filled, Remaining: ledger.Remaining(), AveragePrice: ledger.AveragePrice(), FeesBase: ledger.FeesBase, FeesQuote: ledger.FeesQuote}
		if out.Plan.BenchmarkValid {
			cost := ledger.MarkedCost(p.ArrivalMid, p.ArrivalMid)
			out.Plan.ExecutedCostQuote = &cost
		}
	}
	if c := v.State.Child; c != nil {
		out.Child = &ChildDTO{ID: c.ID, Intent: c.Order.Intent, Remaining: c.Order.Remaining, State: c.State, KnownFinal: c.KnownFinal, CancelRequested: c.State == "cancel_requested"}
	}
	start := max(0, len(v.State.Fills)-2048)
	out.Fills = append(out.Fills, v.State.Fills[start:]...)
	out.FillsTruncated = start > 0
	return out, nil
}
