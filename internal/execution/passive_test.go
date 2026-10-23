package execution

import (
	"testing"

	d "github.com/liquiditylab/liquiditylab/internal/domain"
)

func TestMatchPassiveTrade(t *testing.T) {
	for _, tc := range []struct {
		name, side, state, tradeSide, price, volume, ahead, remaining, wantQueue, wantFill string
	}{
		{"depletes queue", "buy", "open", "sell", "99", "0.4", "0.7", "0.5", "0.3", "0"},
		{"queue exhausted exactly", "buy", "open", "sell", "99", "0.7", "0.7", "0.5", "0", "0"},
		{"partial maker", "buy", "open", "sell", "99", "0.9", "0.7", "0.5", "0", "0.2"},
		{"cap remaining", "buy", "open", "sell", "99", "3", "0.7", "0.5", "0", "0.5"},
		{"cancel race", "buy", "cancel_requested", "sell", "99", "0.9", "0.7", "0.5", "0", "0.2"},
		{"sell maker", "sell", "open", "buy", "99", "0.9", "0.7", "0.5", "0", "0.2"},
		{"same aggressor side", "buy", "open", "buy", "99", "3", "0.7", "0.5", "0.7", "0"},
		{"different price", "buy", "open", "sell", "98", "3", "0.7", "0.5", "0.7", "0"},
		{"not arrived", "buy", "submitting", "sell", "99", "3", "0.7", "0.5", "0.7", "0"},
		{"final child", "buy", "filled", "sell", "99", "3", "0.7", "0.5", "0.7", "0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			child := d.ChildOrder{Intent: d.OrderIntent{Side: tc.side, Price: d.MustDecimal("99"), Passive: true}, State: tc.state, QueueAhead: d.MustDecimal(tc.ahead), Remaining: d.MustDecimal(tc.remaining)}
			trade := d.MarketEvent{Kind: "trade", Side: tc.tradeSide, Price: d.MustDecimal(tc.price), Quantity: d.MustDecimal(tc.volume)}
			match := MatchPassiveTrade(child, trade)
			if !match.QueueAhead.Equal(d.MustDecimal(tc.wantQueue)) || !match.Quantity.Equal(d.MustDecimal(tc.wantFill)) {
				t.Fatalf("match=%+v; want queue=%s fill=%s", match, tc.wantQueue, tc.wantFill)
			}
			if !child.QueueAhead.Equal(d.MustDecimal(tc.ahead)) || !child.Remaining.Equal(d.MustDecimal(tc.remaining)) {
				t.Fatal("matcher changed caller's child")
			}
			for _, invalid := range []d.MarketEvent{{Kind: "book", Side: trade.Side, Price: trade.Price, Quantity: trade.Quantity}, {Kind: "trade", Invalid: true, Side: trade.Side, Price: trade.Price, Quantity: trade.Quantity}} {
				ignored := MatchPassiveTrade(child, invalid)
				if !ignored.Quantity.IsZero() || !ignored.QueueAhead.Equal(child.QueueAhead) {
					t.Fatal("non-trade or invalid input depleted queue")
				}
			}
		})
	}
}
