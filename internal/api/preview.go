package api

import (
	"net/http"
	"time"

	d "github.com/liquiditylab/liquiditylab/internal/domain"
	x "github.com/liquiditylab/liquiditylab/internal/execution"
	rt "github.com/liquiditylab/liquiditylab/internal/runtime"
	pg "github.com/liquiditylab/liquiditylab/internal/store/postgres"
)

type previewRequest struct {
	Plan   *d.Plan `json:"plan"`
	Policy string  `json:"policy,omitempty"`
}
type AmountDTO struct {
	Currency string    `json:"currency"`
	Amount   d.Decimal `json:"amount"`
}
type PreviewDTO struct {
	Revision    int64          `json:"revision"`
	Profile     rt.Profile     `json:"profile"`
	ObservedAt  time.Time      `json:"observed_at"`
	Market      MarketDTO      `json:"market"`
	Policy      string         `json:"policy"`
	Ready       bool           `json:"ready"`
	Reason      string         `json:"reason"`
	Spread      d.Decimal      `json:"spread"`
	VisibleBids d.Decimal      `json:"visible_bids"`
	VisibleAsks d.Decimal      `json:"visible_asks"`
	Intent      *d.OrderIntent `json:"intent"`
	Reservation *AmountDTO     `json:"reservation"`
	MakerFee    *AmountDTO     `json:"estimated_maker_fee"`
	TakerFee    *AmountDTO     `json:"estimated_taker_fee"`
	FeeSource   string         `json:"fee_source"`
	// This projection describes one proposal, never a promise of completion.
	CompletionGuaranteed bool `json:"completion_guaranteed"`
}

func (h *Handler) preview(w http.ResponseWriter, r *http.Request, key pg.Key, a Authority) {
	var body previewRequest
	if !h.decode(w, r, &body) {
		return
	}
	if body.Plan == nil || !validPlan(*body.Plan) {
		fail(w, 400, "invalid_plan")
		return
	}
	if body.Policy == "" {
		body.Policy = x.BaselineScheduled
	}
	if x.ValidateBaselinePolicy(body.Policy) != nil {
		fail(w, 400, "invalid_policy")
		return
	}
	view, err := a.View(r.Context())
	if err != nil {
		backendError(w, err)
		return
	}
	if view.Account.Key != key {
		fail(w, 503, "unavailable")
		return
	}
	out, err := preview(view, *body.Plan, body.Policy)
	if err != nil {
		backendError(w, err)
		return
	}
	writeJSON(w, 200, out)
}
func preview(v rt.View, p d.Plan, policy string) (out PreviewDTO, err error) {
	defer func() {
		if recover() != nil {
			out = PreviewDTO{}
			err = rt.ErrInvalid
		}
	}()
	if p.Symbol != v.State.Rules.Symbol || v.State.Rules.Validate() != nil || v.State.Fees.Validate() != nil || v.State.Execution.Validate() != nil {
		return out, rt.ErrInvalid
	}
	if len(v.Account.Balances) > 128 || len(v.Account.Reservations) > 2048 {
		return out, rt.ErrInvalid
	}
	baseFound, quoteFound := false, false
	for _, balance := range v.Account.Balances {
		available := balance.Free
		for _, entry := range v.Account.Reservations {
			if entry.Currency == balance.Currency && !entry.Reflected && entry.State == "held" {
				available = available.Sub(entry.Amount)
			}
		}
		available = d.Max(d.Zero, available)
		if balance.Currency == v.State.BaseCurrency {
			baseFound = true
			if p.BaseBalance.Cmp(available) > 0 {
				return out, pg.ErrInsufficientFunds
			}
		}
		if balance.Currency == v.State.QuoteCurrency {
			quoteFound = true
			if p.QuoteBudget.Cmp(available) > 0 {
				return out, pg.ErrInsufficientFunds
			}
		}
	}
	if !baseFound || !quoteFound {
		return out, pg.ErrInsufficientFunds
	}
	out = PreviewDTO{Revision: v.Account.Revision, Profile: v.State.Profile, ObservedAt: time.Now().UTC(), Market: market(v), Policy: policy, FeeSource: v.State.Fees.Source, Reason: "market_invalid"}
	out.Ready = v.Account.Ready && out.Market.Valid && !v.State.RecoveryRequired && !v.State.ClockDiscontinuity
	if v.State.Parent != nil && (v.State.Parent.Status == "active" || v.State.Parent.Status == "paused" || v.State.Parent.Status == "cancel_requested") || v.State.Child != nil {
		out.Ready = false
		out.Reason = "creation_unavailable"
		return out, nil
	}
	if v.State.Book == nil || !out.Market.Valid {
		return out, nil
	}
	out.Spread = out.Market.BestAsk.Sub(out.Market.BestBid)
	out.VisibleBids = v.State.Book.Visible("bids")
	out.VisibleAsks = v.State.Book.Visible("asks")

	if !out.Ready {
		out.Reason = "recovery_required"
		return out, nil
	}
	action, _, err := x.BaselineAction(policy, p, 0, d.Zero, 0)
	if err != nil {
		return out, rt.ErrInvalid
	}
	compiled, err := x.Compile(action, p, v.State.Rules, v.State.Fees, v.State.Execution, x.NewLedger(p), v.State.Book, nil, 0)
	if err != nil {
		return out, rt.ErrInvalid
	}
	out.Intent = compiled.Intent
	out.Reason = compiled.Reason
	if compiled.Intent == nil {
		return out, nil
	}
	kind, amount, err := x.ReservationFor(*compiled.Intent, v.State.Fees)
	if err != nil {
		return out, rt.ErrInvalid
	}
	currency := v.State.BaseCurrency
	if kind == "quote" {
		currency = v.State.QuoteCurrency
	}
	out.Reservation = &AmountDTO{Currency: currency, Amount: amount}
	feeCurrency := v.State.BaseCurrency
	if v.State.Fees.Currency == "quote" {
		feeCurrency = v.State.QuoteCurrency
	}
	maker, err := x.FeeFor(compiled.Intent.Quantity, compiled.Intent.Price, v.State.Fees.Maker, v.State.Fees.Currency)
	if err != nil {
		return out, rt.ErrInvalid
	}
	taker, err := x.FeeFor(compiled.Intent.Quantity, compiled.Intent.Price, v.State.Fees.Taker, v.State.Fees.Currency)
	if err != nil {
		return out, rt.ErrInvalid
	}
	out.MakerFee = &AmountDTO{Currency: feeCurrency, Amount: maker}
	out.TakerFee = &AmountDTO{Currency: feeCurrency, Amount: taker}
	return out, nil
}
