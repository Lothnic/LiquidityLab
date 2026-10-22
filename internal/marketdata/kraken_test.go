package marketdata

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/liquiditylab/liquiditylab/internal/venue/kraken"
)

func TestNormalizeReceiptOrderDropsHistoricalTradesAndInvalidatesOnGap(t *testing.T) {
	// CRC32 of asks then bids: 1020250010003000.
	const book = `{"channel":"book","type":"snapshot","data":[{"symbol":"BTC/USD","bids":[{"price":"100.0","qty":"0.3000"}],"asks":[{"price":"102.0","qty":"0.2500"}],"checksum":2926945269}]}`
	trade := `{"channel":"trade","type":"snapshot","data":[{"symbol":"BTC/USD","price":"101.0","qty":"0.0200","side":"buy","trade_id":7}]}`
	flow := `{"channel":"trade","type":"update","data":[{"symbol":"BTC/USD","price":"101.0","qty":"0.0200","side":"buy","trade_id":8}]}`
	rows := receiptLine(0, "10", book, "", "") + "\n" +
		receiptLine(1, "11", "", "gap", "disconnect") + "\n" +
		receiptLine(2, "12", flow, "", "") + "\n" +
		receiptLine(3, "13", book, "", "") + "\n" +
		receiptLine(4, "14", trade, "", "") + "\n" +
		receiptLine(5, "15", flow, "", "") + "\n"
	p := filepath.Join(t.TempDir(), "recording.jsonl")
	if e := os.WriteFile(p, []byte(rows), 0600); e != nil {
		t.Fatal(e)
	}
	events, e := Normalize(p)
	if e != nil {
		t.Fatal(e)
	}
	if len(events) != 4 || events[0].Kind != "snapshot" || events[1].Kind != "gap" || events[2].Kind != "snapshot" || events[3].Kind != "trade" {
		t.Fatalf("unexpected events: %+v", events)
	}
	if events[3].Time != 5_000_000_000 || events[3].Quantity != "0.0200" || events[3].TradeID != "8" {
		t.Fatalf("trade precision/time lost: %+v", events[3])
	}
}

func quote(s string) string { b, _ := json.Marshal(s); return string(b) }
func TestElapsedUsesIntegerNanoseconds(t *testing.T) {
	v, e := ns("1.000000001")
	if e != nil || v != 1_000_000_001 {
		t.Fatalf("got %d, %v", v, e)
	}
	if _, e = ns("-1"); e == nil {
		t.Fatal("accepted negative time")
	}
}
func TestNormalizeRejectsRegressingReceiptTime(t *testing.T) {
	p := filepath.Join(t.TempDir(), "recording.jsonl")
	data := receiptLine(0, "2", "{}", "", "") + "\n" + receiptLine(1, "1", "{}", "", "") + "\n"
	if e := os.WriteFile(p, []byte(data), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e := Normalize(p); e == nil {
		t.Fatal("accepted noncausal receipt sequence")
	}
}

func TestNormalizeIdentityDiscontinuityEmitsGapAndInvalidatesBook(t *testing.T) {
	const book = `{"channel":"book","type":"snapshot","data":[{"symbol":"BTC/USD","bids":[{"price":"100.0","qty":"0.3000"}],"asks":[{"price":"102.0","qty":"0.2500"}],"checksum":2926945269}]}`
	const trade = `{"channel":"trade","type":"update","data":[{"symbol":"BTC/USD","price":"101.0","qty":"0.0200","side":"buy","trade_id":8}]}`
	rows := receiptLine(0, "1", book, "", "") + "\n" +
		receiptLine(2, "2", trade, "", "") + "\n" +
		receiptLine(2, "3", trade, "", "") + "\n"
	p := filepath.Join(t.TempDir(), "recording.jsonl")
	if e := os.WriteFile(p, []byte(rows), 0600); e != nil {
		t.Fatal(e)
	}
	events, e := Normalize(p)
	if e != nil {
		t.Fatal(e)
	}
	if len(events) != 2 || events[0].Kind != "snapshot" || events[1].Kind != "gap" || events[1].Reason != "noncontiguous receipt index" {
		t.Fatalf("identity discontinuity was not preserved as a gap: %+v", events)
	}
}

func TestNormalizeReceiptMetadataMismatchesBecomeGaps(t *testing.T) {
	const book = `{"channel":"book","type":"snapshot","data":[{"symbol":"BTC/USD","bids":[{"price":"100.0","qty":"0.3000"}],"asks":[{"price":"102.0","qty":"0.2500"}],"checksum":2926945269}]}`
	cases := []struct {
		name   string
		mutate func(*Receipt)
		want   string
	}{
		{"schema", func(r *Receipt) { r.Schema = "other" }, "invalid receipt schema"},
		{"session", func(r *Receipt) { r.SessionID = "other-session" }, "receipt session changed"},
		{"depth", func(r *Receipt) { r.Depth = 25 }, "receipt depth changed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bad := Receipt{Schema: KrakenSchema, SessionID: "test-session", Depth: 10, ReceiptIndex: 1, Elapsed: "2", Raw: book, Valid: true}
			tc.mutate(&bad)
			badJSON, _ := json.Marshal(bad)
			rows := receiptLine(0, "1", book, "", "") + "\n" + string(badJSON) + "\n" + receiptLine(2, "3", book, "", "") + "\n"
			p := filepath.Join(t.TempDir(), "recording.jsonl")
			if err := os.WriteFile(p, []byte(rows), 0600); err != nil {
				t.Fatal(err)
			}
			events, err := Normalize(p)
			if err != nil {
				t.Fatal(err)
			}
			if len(events) != 3 || events[1].Kind != "gap" || events[1].Reason != tc.want || events[2].Kind != "snapshot" {
				t.Fatalf("metadata mismatch did not produce recoverable gap: %+v", events)
			}
		})
	}
}

func TestNumericInputBoundsBeforeParsing(t *testing.T) {
	if _, err := wireString(json.RawMessage(`1e33`)); err == nil {
		t.Fatal("accepted excessive venue exponent")
	}
	if _, err := ns("1" + strings.Repeat("0", maxDecimalDigits+1)); err == nil {
		t.Fatal("accepted excessive elapsed precision")
	}
	if got, err := wireString(json.RawMessage(`"0.00000001"`)); err != nil || got != "0.00000001" {
		t.Fatalf("lost exact decimal source: %q, %v", got, err)
	}
	if got, err := wireString(json.RawMessage(`1.20e-2`)); err != nil || got != "0.0120" {
		t.Fatalf("scientific decimal was not converted exactly: %q, %v", got, err)
	}
}

func TestValidateFrameChecksumFailureRequiresResnapshot(t *testing.T) {
	const good = `{"channel":"book","type":"snapshot","data":[{"symbol":"BTC/USD","bids":[{"price":"100.0","qty":"0.3000"}],"asks":[{"price":"102.0","qty":"0.2500"}],"checksum":2926945269}]}`
	const bad = `{"channel":"book","type":"update","data":[{"symbol":"BTC/USD","bids":[{"price":"100.0","qty":"0.3100"}],"asks":[],"checksum":2926945269}]}`
	b, err := kraken.NewBook(10)
	if err != nil {
		t.Fatal(err)
	}
	valid, _, snapshots, err := validateFrame(good, "BTC/USD", b)
	if err != nil || !valid || snapshots != 1 {
		t.Fatalf("valid snapshot failed: valid=%t snapshots=%d err=%v", valid, snapshots, err)
	}
	valid, _, _, err = validateFrame(bad, "BTC/USD", b)
	if err == nil || valid || b.Valid {
		t.Fatalf("bad checksum did not invalidate book: valid=%t book=%t err=%v", valid, b.Valid, err)
	}
}

func receiptLine(index uint64, elapsed, raw, kind, reason string) string {
	row := Receipt{Schema: KrakenSchema, SessionID: "test-session", Depth: 10, ReceiptIndex: index, Elapsed: elapsed, Raw: raw, Kind: kind, Reason: reason, Valid: true}
	b, _ := json.Marshal(row)
	return string(b)
}
