package marketdata

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"

	"github.com/liquiditylab/liquiditylab/internal/venue/kraken"
)

// Normalizer retains a single bounded checksum book and receipt/session cursors.
// Call Accept serially in raw receipt order; it owns no recording or goroutines.
type Normalizer struct {
	b                           *kraken.Book
	origin, last                int64
	haveOrigin                  bool
	symbol, sessionID           string
	depth                       int
	haveSession, haveDepth      bool
	expectedReceipt, eventIndex uint64
}

func NewNormalizer() *Normalizer { return &Normalizer{last: -1} }
func (n *Normalizer) emit(out *[]Event, kind string, t int64, row Receipt, extra Event) {
	if !n.haveOrigin {
		return
	}
	extra.Kind = kind
	extra.Type = kind
	extra.Time = t - n.origin
	extra.ReceiptIndex = n.eventIndex
	n.eventIndex++
	extra.SourceReceiptIndex = row.ReceiptIndex
	extra.ReceiptSessionID = n.sessionID
	extra.Symbol = n.symbol
	extra.Valid = kind != "gap"
	*out = append(*out, extra)
}
func (n *Normalizer) identityGap(out *[]Event, t int64, row Receipt, reason string) {
	if n.b != nil {
		n.b.Valid = false
	}
	n.emit(out, "gap", t, row, Event{Reason: reason})
}

// Accept emits each normalized event exactly once. Before the first verified
// snapshot it emits nothing. A fatal parse/time error invalidates the book and
// discards that receipt's events; the caller must stop or supply a gap followed
// by a fresh snapshot. Checksum and identity discontinuities emit gap events.
func (n *Normalizer) Accept(row Receipt) (out []Event, err error) {
	previousIndex := n.eventIndex
	defer func() {
		if err != nil {
			if n.b != nil {
				n.b.Valid = false
			}
			n.eventIndex = previousIndex
			out = nil
		}
	}()
	if len(row.Raw) > maxWireFrame || len(row.SessionID) > 256 || len(row.Reason) > 4096 {
		return nil, errors.New("receipt exceeds normalization bounds")
	}
	t, e := ns(row.Elapsed)
	if e != nil {
		return nil, e
	}
	if t < n.last {
		return nil, errors.New("noncausal receipt time")
	}
	n.last = t
	identityReason := ""
	if row.Schema != KrakenSchema {
		identityReason = "invalid receipt schema"
	}
	if identityReason == "" && row.ReceiptIndex != n.expectedReceipt {
		identityReason = "noncontiguous receipt index"
	}
	if row.SessionID == "" {
		if identityReason == "" {
			identityReason = "missing receipt session id"
		}
	} else if !n.haveSession {
		n.sessionID, n.haveSession = row.SessionID, true
	} else if row.SessionID != n.sessionID && identityReason == "" {
		identityReason = "receipt session changed"
	}
	if _, err := kraken.NewBook(row.Depth); err != nil {
		if identityReason == "" {
			identityReason = "invalid receipt depth"
		}
	} else if !n.haveDepth {
		n.depth, n.haveDepth = row.Depth, true
	} else if row.Depth != n.depth && identityReason == "" {
		identityReason = "receipt depth changed"
	}
	n.expectedReceipt++
	if identityReason != "" {
		n.identityGap(&out, t, row, identityReason)
		return out, nil
	}
	if row.Kind == "gap" {
		n.identityGap(&out, t, row, row.Reason)
		return out, nil
	}
	if row.Raw == "" {
		return out, nil
	}
	var msg envelope
	if e = json.Unmarshal([]byte(row.Raw), &msg); e != nil {
		return nil, fmt.Errorf("raw: %w", e)
	}
	if msg.Channel == "book" {
		for _, it := range msg.Data {
			if n.symbol == "" {
				n.symbol = it.Symbol
			}
			if it.Symbol != n.symbol {
				return nil, errors.New("a replay recording must contain one symbol")
			}
			if n.b == nil {
				n.b, e = kraken.NewBook(row.Depth)
				if e != nil {
					return nil, e
				}
			}
			previousBids := copyLevels(n.b.Levels("bids"))
			previousAsks := copyLevels(n.b.Levels("asks"))
			bids, e := levels(it.Bids)
			if e != nil {
				return nil, e
			}
			asks, e := levels(it.Asks)
			if e != nil {
				return nil, e
			}
			p := kraken.Payload{Symbol: it.Symbol, Bids: bids, Asks: asks, Checksum: it.Checksum, Timestamp: it.Timestamp}
			snap := msg.Type == "snapshot"
			if e = n.b.Apply(p, snap); e != nil {
				n.emit(&out, "gap", t, row, Event{Reason: e.Error()})
				continue
			}
			if !n.haveOrigin {
				n.origin = t
				n.haveOrigin = true
			}
			var outB, outA [][]string
			if snap {
				outB = n.b.Levels("bids")
				outA = n.b.Levels("asks")
			} else {
				outB = diff(previousBids, n.b.Levels("bids"))
				outA = diff(previousAsks, n.b.Levels("asks"))
			}
			var c uint64
			_ = json.Unmarshal(it.Checksum, &c)
			n.emit(&out, map[bool]string{true: "snapshot", false: "book"}[snap], t, row, Event{Bids: outB, Asks: outA, Checksum: uint32(c), ExchangeTimestamp: it.Timestamp})
		}
	}
	if msg.Channel == "trade" && n.b != nil && n.b.Valid && msg.Type != "snapshot" {
		for _, it := range msg.Data {
			if it.Symbol != n.symbol {
				continue
			}
			price, e := wireString(it.Price)
			if e != nil {
				return nil, e
			}
			qty, e := wireString(it.Qty)
			if e != nil {
				return nil, e
			}
			pr, ok := new(big.Rat).SetString(price)
			if !ok || pr.Sign() <= 0 {
				return nil, errors.New("Invalid trade event")
			}
			q, ok := new(big.Rat).SetString(qty)
			if !ok || q.Sign() <= 0 || (it.Side != "buy" && it.Side != "sell") {
				return nil, errors.New("Invalid trade event")
			}
			id, e := scalarID(it.TradeID)
			if e != nil {
				return nil, e
			}
			n.emit(&out, "trade", t, row, Event{Price: price, Quantity: qty, Side: it.Side, TradeID: id, ExchangeTimestamp: it.Timestamp})
		}
	}
	return out, nil
}
