package execution

import (
	"fmt"
	d "github.com/liquiditylab/liquiditylab/internal/domain"
	"sort"
)

type Book struct {
	Bids, Asks map[string]d.Level
	Debt       map[string]map[string]d.Decimal
	Valid      bool
}

func NewBook() *Book {
	return &Book{map[string]d.Level{}, map[string]d.Level{}, map[string]map[string]d.Decimal{"bids": {}, "asks": {}}, false}
}
func (b *Book) Levels(side string) map[string]d.Level {
	if side == "bids" {
		return b.Bids
	}
	return b.Asks
}
func (b *Book) Apply(e d.MarketEvent) error {
	if e.Kind == "gap" || e.Invalid {
		b.Valid = false
		return nil
	}
	if e.Kind != "snapshot" && e.Kind != "book" {
		return nil
	}
	if e.Kind == "snapshot" {
		b.Bids = map[string]d.Level{}
		b.Asks = map[string]d.Level{}
		b.Valid = true
	} else if !b.Valid {
		return nil
	}
	for _, side := range []string{"bids", "asks"} {
		levels := e.Bids
		if side == "asks" {
			levels = e.Asks
		}
		for _, l := range levels {
			if l.Price.Sign() <= 0 || l.Quantity.Sign() < 0 {
				return fmt.Errorf("invalid book level")
			}
			key := l.Price.String()
			if l.Quantity.IsZero() {
				delete(b.Levels(side), key)
				delete(b.Debt[side], key)
			} else {
				b.Levels(side)[key] = l
			}
		}
	}
	if len(b.Bids) == 0 || len(b.Asks) == 0 || b.BestBid().Cmp(b.BestAsk()) >= 0 {
		b.Valid = false
	}
	return nil
}
func (b *Book) BestBid() d.Decimal {
	best := d.Zero
	for _, l := range b.Bids {
		best = d.Max(best, l.Price)
	}
	return best
}
func (b *Book) BestAsk() d.Decimal {
	best := d.Zero
	for _, l := range b.Asks {
		if best.IsZero() || l.Price.Cmp(best) < 0 {
			best = l.Price
		}
	}
	return best
}
func (b *Book) Mid() d.Decimal { return b.BestBid().Add(b.BestAsk()).Quo(d.MustDecimal("2")) }
func (b *Book) Available(side string, p d.Decimal) d.Decimal {
	return d.Max(b.Levels(side)[p.String()].Quantity.Sub(b.Debt[side][p.String()]), d.Zero)
}
func (b *Book) Consume(side string, p, q d.Decimal) {
	key := p.String()
	b.Debt[side][key] = b.Debt[side][key].Add(q)
}
func (b *Book) Visible(side string) d.Decimal {
	q := d.Zero
	for _, l := range b.Levels(side) {
		q = q.Add(b.Available(side, l.Price))
	}
	return q
}
func (b *Book) Prices(side string) []d.Decimal {
	p := make([]d.Decimal, 0, len(b.Levels(side)))
	for _, l := range b.Levels(side) {
		p = append(p, l.Price)
	}
	sort.Slice(p, func(i, j int) bool {
		if side == "bids" {
			return p[i].Cmp(p[j]) > 0
		}
		return p[i].Cmp(p[j]) < 0
	})
	return p
}
