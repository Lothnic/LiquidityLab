package runtime

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	d "github.com/liquiditylab/liquiditylab/internal/domain"
	pg "github.com/liquiditylab/liquiditylab/internal/store/postgres"
)

func TestPaperTakeoverFencesFreshAndCachedOldTransportEffects(t *testing.T) {
	store, key, owner := paperTestStore(t)
	ctx, cancel := actorTestContext(t)
	defer cancel()
	old := newPaperTransport(store, key, owner)
	submit := Submission{ID: "cached-child", OperationID: "cached-submit", InputDigest: "cached-input", Book: paperTestBook(t), Fees: paperTestFees(), Intent: d.OrderIntent{Side: "buy", Quantity: d.MustDecimal("0.01"), Price: d.MustDecimal("101")}}
	trade := d.MarketEvent{Kind: "trade", Symbol: "BTC/USD", TradeID: "cached-trade", Side: "sell", Price: d.MustDecimal("99"), Quantity: d.One}
	if _, err := old.ObserveTrade(ctx, "source", trade, nil, paperTestFees()); err != nil {
		t.Fatal(err)
	}
	prior, err := old.Handoff(ctx, submit)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = old.Cancel(ctx, "cached-cancel"); err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimPaperOwnership(ctx, key, "next-paper-owner", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	before, err := store.GetAccount(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		act  func() error
	}{
		{"cached handoff", func() error { _, e := old.Handoff(ctx, submit); return e }},
		{"fresh handoff", func() error { next := submit; next.ID = "late-submit"; _, e := old.Handoff(ctx, next); return e }},
		{"cached cancel", func() error { _, e := old.Cancel(ctx, "cached-cancel"); return e }},
		{"fresh cancel", func() error { _, e := old.Cancel(ctx, "late-cancel"); return e }},
		{"cached trade", func() error { _, e := old.ObserveTrade(ctx, "source", trade, nil, paperTestFees()); return e }},
		{"fresh trade", func() error {
			next := trade
			next.TradeID = "late-trade"
			_, e := old.ObserveTrade(ctx, "source", next, nil, paperTestFees())
			return e
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.act(); !errors.Is(err, pg.ErrOwnership) {
				t.Fatalf("stale transport result: %v", err)
			}
			after, err := store.GetAccount(ctx, key)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("old transport changed retained state: %v", err)
			}
		})
	}
	next := newPaperTransport(store, key, claimed.Ownership)
	got, err := next.Handoff(context.Background(), submit)
	if err != nil || !reflect.DeepEqual(got, prior) {
		t.Fatalf("new owner could not discover committed outcome: %+v %v", got, err)
	}
	after, err := store.GetAccount(ctx, key)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("owned retry appended or changed history: %v", err)
	}
}
