package marketdata

import (
	"encoding/json"
	"os"
	"testing"
)

func TestDepthTruncationEmitsDeletionAndDoesNotResurrectLevel(t *testing.T) {
	data, err := os.ReadFile("../../conformance/kraken-checksums.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Book map[string]any `json:"book"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	frame := func(kind string, book map[string]any) string {
		t.Helper()
		b, err := json.Marshal(map[string]any{"channel": "book", "type": kind, "data": []any{book}})
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	fixture.Book["checksum"] = uint32(3310070434)
	n := NewNormalizer()
	events, err := n.Accept(streamRow(0, frame("snapshot", fixture.Book)))
	if err != nil || len(events) != 1 || len(events[0].Bids) != 10 || !n.b.Valid {
		t.Fatalf("initial depth: events=%+v err=%v", events, err)
	}
	update := func(index uint64, qty string, checksum uint32) []Event {
		t.Helper()
		book := map[string]any{"symbol": "BTC/USD", "bids": []any{map[string]string{"price": "45284.0", "qty": qty}}, "asks": []any{}, "checksum": checksum}
		got, err := n.Accept(streamRow(index, frame("update", book)))
		if err != nil || len(got) != 1 || got[0].Kind != "book" || got[0].Checksum != checksum || !n.b.Valid {
			t.Fatalf("update: events=%+v err=%v", got, err)
		}
		return got
	}
	// CRC constants are independently calculated from the published ten-level
	// fixture using decimal sorting and zlib, not from the production book.
	events = update(1, "0.2500", 930746968)
	changes := map[string]string{}
	for _, level := range events[0].Bids {
		changes[level[0]] = level[1]
	}
	if len(changes) != 2 || changes["45284.0"] != "0.2500" || changes["45276.6"] != "0" || len(n.b.Levels("bids")) != 10 {
		t.Fatalf("trimmed depth must emit explicit deletion: %+v", events)
	}
	events = update(2, "0", 3973302209)
	if len(events[0].Bids) != 1 || len(events[0].Bids[0]) != 2 || events[0].Bids[0][0] != "45284.0" || events[0].Bids[0][1] != "0" || len(n.b.Levels("bids")) != 9 {
		t.Fatalf("deletion resurrected a truncated level: %+v levels=%+v", events, n.b.Levels("bids"))
	}
}
