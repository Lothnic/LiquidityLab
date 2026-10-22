// Package kraken implements the public, unauthenticated Kraken v2 market-data
// wire protocol. It contains no order or private-account operations.
package kraken

import (
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"math/big"
	"sort"
	"strings"
)

var ErrBook = errors.New("invalid Kraken book")

type Level struct {
	Price string `json:"price"`
	Qty   string `json:"qty"`
}
type Payload struct {
	Symbol    string          `json:"symbol"`
	Bids      []Level         `json:"bids"`
	Asks      []Level         `json:"asks"`
	Checksum  json.RawMessage `json:"checksum"`
	Timestamp string          `json:"timestamp,omitempty"`
}
type entry struct{ price, qty string }
type Book struct {
	Depth      int
	bids, asks map[string]entry
	Valid      bool
}

func NewBook(depth int) (*Book, error) {
	switch depth {
	case 10, 25, 100, 500, 1000:
	default:
		return nil, fmt.Errorf("Kraken depth must be 10, 25, 100, 500, or 1000")
	}
	return &Book{Depth: depth, bids: map[string]entry{}, asks: map[string]entry{}}, nil
}
func decimal(s string) (*big.Rat, error) {
	r, ok := new(big.Rat).SetString(s)
	if !ok || strings.ContainsAny(s, "NaInf") {
		return nil, fmt.Errorf("invalid book number")
	}
	return r, nil
}
func (b *Book) Levels(side string) [][]string {
	m := b.asks
	if side == "bids" {
		m = b.bids
	}
	out := make([]entry, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool {
		a, _ := decimal(out[i].price)
		c, _ := decimal(out[j].price)
		n := a.Cmp(c)
		if side == "bids" {
			return n > 0
		}
		return n < 0
	})
	result := make([][]string, len(out))
	for i, v := range out {
		result[i] = []string{v.price, v.qty}
	}
	return result
}
func checksumPart(s string) string {
	s = strings.ReplaceAll(s, ".", "")
	return strings.TrimLeft(s, "0")
}
func (b *Book) Checksum() uint32 {
	var parts strings.Builder
	for _, side := range []string{"asks", "bids"} {
		ls := b.Levels(side)
		if len(ls) > 10 {
			ls = ls[:10]
		}
		for _, v := range ls {
			parts.WriteString(checksumPart(v[0]))
			parts.WriteString(checksumPart(v[1]))
		}
	}
	return crc32.ChecksumIEEE([]byte(parts.String()))
}
func (b *Book) Apply(p Payload, snapshot bool) error {
	if !snapshot && !b.Valid {
		return fmt.Errorf("%w: cannot apply updates before valid snapshot", ErrBook)
	}
	if snapshot {
		b.bids = map[string]entry{}
		b.asks = map[string]entry{}
	}
	b.Valid = false
	for _, s := range []string{"bids", "asks"} {
		m := b.asks
		if s == "bids" {
			m = b.bids
		}
		levels := p.Asks
		if s == "bids" {
			levels = p.Bids
		}
		for _, l := range levels {
			pr, e := decimal(l.Price)
			if e != nil {
				return fmt.Errorf("%w: %v", ErrBook, e)
			}
			q, e := decimal(l.Qty)
			if e != nil {
				return fmt.Errorf("%w: %v", ErrBook, e)
			}
			if pr.Sign() <= 0 || q.Sign() < 0 {
				return fmt.Errorf("%w: price must be positive and quantity nonnegative", ErrBook)
			}
			key := pr.RatString()
			if q.Sign() == 0 {
				delete(m, key)
			} else {
				m[key] = entry{l.Price, l.Qty}
			}
		}
		ordered := b.Levels(s)
		keep := map[string]bool{}
		for i, l := range ordered {
			if i < b.Depth {
				r, _ := decimal(l[0])
				keep[r.RatString()] = true
			}
		}
		for k := range m {
			if !keep[k] {
				delete(m, k)
			}
		}
	}
	if len(b.bids) == 0 || len(b.asks) == 0 {
		return fmt.Errorf("%w: empty book", ErrBook)
	}
	bestBid := b.Levels("bids")[0][0]
	bestAsk := b.Levels("asks")[0][0]
	bid, _ := decimal(bestBid)
	ask, _ := decimal(bestAsk)
	if bid.Cmp(ask) >= 0 {
		return fmt.Errorf("%w: crossed book", ErrBook)
	}
	var got uint64
	if len(p.Checksum) == 0 {
		return fmt.Errorf("%w: missing checksum", ErrBook)
	}
	if e := json.Unmarshal(p.Checksum, &got); e != nil {
		var n json.Number
		if e2 := json.Unmarshal(p.Checksum, &n); e2 != nil {
			return fmt.Errorf("%w: invalid checksum", ErrBook)
		}
		v, ok := new(big.Int).SetString(n.String(), 10)
		if !ok || !v.IsUint64() {
			return fmt.Errorf("%w: invalid checksum", ErrBook)
		}
		got = v.Uint64()
	}
	if uint32(got) != b.Checksum() {
		return fmt.Errorf("%w: checksum mismatch", ErrBook)
	}
	b.Valid = true
	return nil
}
