package marketdata

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

const streamBook = `{"channel":"book","type":"snapshot","data":[{"symbol":"BTC/USD","bids":[{"price":"100.0","qty":"0.3000"}],"asks":[{"price":"102.0","qty":"0.2500"}],"checksum":2926945269}]}`
const streamTrades = `{"channel":"trade","type":"update","data":[{"symbol":"BTC/USD","price":"101.0","qty":"0.0200","side":"buy","trade_id":8,"timestamp":"source-1"},{"symbol":"BTC/USD","price":1.01e2,"qty":2.00e-2,"side":"sell","trade_id":"9","timestamp":"source-2"}]}`
const streamBadBook = `{"channel":"book","type":"update","data":[{"symbol":"BTC/USD","bids":[{"price":"100.0","qty":"0.3100"}],"asks":[],"checksum":2926945269}]}`

func streamRow(i uint64, raw string) Receipt {
	return Receipt{Schema: KrakenSchema, SessionID: "stream-session", Depth: 10, ReceiptIndex: i, Elapsed: fmt.Sprint(10 + i), Raw: raw, Valid: true}
}
func TestIncrementalMatchesFileNormalizationAndEconomicGolden(t *testing.T) {
	rows := []Receipt{streamRow(0, `{"channel":"heartbeat"}`), streamRow(1, streamBook), streamRow(2, strings.Replace(streamTrades, `"type":"update"`, `"type":"snapshot"`, 1)), streamRow(3, streamTrades), streamRow(4, streamBadBook), streamRow(5, streamTrades), streamRow(6, streamBook), streamRow(7, streamTrades)}
	rows = append(rows, Receipt{Schema: KrakenSchema, SessionID: "stream-session", Depth: 10, ReceiptIndex: 8, Elapsed: "18", Kind: "gap", Reason: "disconnected"})
	path := filepath.Join(t.TempDir(), "raw.jsonl")
	var text strings.Builder
	normalizer := NewNormalizer()
	var streamed []Event
	for _, row := range rows {
		b, _ := json.Marshal(row)
		text.Write(b)
		text.WriteByte('\n')
		got, err := normalizer.Accept(row)
		if err != nil {
			t.Fatal(err)
		}
		streamed = append(streamed, got...)
	}
	if err := os.WriteFile(path, []byte(text.String()), 0600); err != nil {
		t.Fatal(err)
	}
	full, err := Normalize(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(streamed, full) {
		t.Fatalf("stream/file mismatch:\n%+v\n%+v", streamed, full)
	}
	kinds := []string{}
	for _, event := range streamed {
		kinds = append(kinds, event.Kind)
		if event.ReceiptSessionID != "stream-session" {
			t.Fatal(event)
		}
	}
	if !reflect.DeepEqual(kinds, []string{"snapshot", "trade", "trade", "gap", "snapshot", "trade", "trade", "gap"}) {
		t.Fatal(kinds)
	}
	first, second := streamed[1], streamed[2]
	if first.Time != 2_000_000_000 || first.ReceiptIndex != 1 || second.ReceiptIndex != 2 || first.SourceReceiptIndex != 3 || second.SourceReceiptIndex != 3 || first.Price != "101.0" || second.Price != "101" || second.Quantity != "0.0200" || first.TradeID != "8" || second.TradeID != "9" || first.ExchangeTimestamp != "source-1" {
		t.Fatalf("changed receipt/decimal economic outputs: %+v %+v", first, second)
	}
	// Freeze the original event contract bytes independently of additive session metadata.
	snapshot := streamed[0]
	snapshot.ReceiptSessionID = ""
	b, _ := json.Marshal(snapshot)
	want := `{"kind":"snapshot","time_ns":0,"type":"snapshot","receipt_index":0,"source_receipt_index":1,"symbol":"BTC/USD","valid":true,"bids":[["100.0","0.3000"]],"asks":[["102.0","0.2500"]],"checksum":2926945269}`
	if string(b) != want {
		t.Fatalf("golden changed:\n%s\n%s", b, want)
	}
	if len(normalizer.b.Levels("bids")) > 10 || len(normalizer.b.Levels("asks")) > 10 {
		t.Fatal("book exceeded pinned depth")
	}
}
func TestIncrementalErrorsInvalidateAndRequireFreshSnapshot(t *testing.T) {
	n := NewNormalizer()
	if got, err := n.Accept(streamRow(0, streamBook)); err != nil || len(got) != 1 {
		t.Fatal(got, err)
	}
	row := streamRow(1, `{`)
	if got, err := n.Accept(row); err == nil || len(got) != 0 || n.b.Valid {
		t.Fatal("fatal parse did not invalidate", got, err)
	}
	if got, err := n.Accept(streamRow(2, streamTrades)); err != nil || len(got) != 0 {
		t.Fatal("trade passed invalid book", got, err)
	}
	if got, err := n.Accept(streamRow(3, streamBook)); err != nil || len(got) != 1 || got[0].ReceiptIndex != 1 {
		t.Fatal(got, err)
	}
	backwards := streamRow(4, streamBook)
	backwards.Elapsed = "0"
	if _, err := n.Accept(backwards); err == nil || n.b.Valid {
		t.Fatal("backwards time accepted")
	}
	oversized := streamRow(4, strings.Repeat(" ", maxWireFrame+1))
	if _, err := n.Accept(oversized); err == nil {
		t.Fatal("oversized frame accepted")
	}
}

type fakeCaptureSocket struct {
	frames        []string
	index, writes int
	closed        bool
	readLimit     int64
}

func (s *fakeCaptureSocket) SetReadLimit(v int64) { s.readLimit = v }
func (s *fakeCaptureSocket) Write(ctx context.Context, kind websocket.MessageType, b []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.writes++
	return nil
}
func (s *fakeCaptureSocket) Read(ctx context.Context) (websocket.MessageType, []byte, error) {
	if err := ctx.Err(); err != nil {
		return 0, nil, err
	}
	if s.index >= len(s.frames) {
		return 0, nil, errors.New("fake stream exhausted")
	}
	raw := s.frames[s.index]
	s.index++
	return websocket.MessageText, []byte(raw), nil
}
func (s *fakeCaptureSocket) Close(websocket.StatusCode, string) error { s.closed = true; return nil }
func testCaptureConfig(t *testing.T, frames uint64) CaptureConfig {
	t.Helper()
	return CaptureConfig{Symbol: "BTC/USD", Output: filepath.Join(t.TempDir(), "capture.jsonl"), Duration: time.Second, Depth: 10, MaxFrames: frames, MaxBytes: 1 << 20}
}
func readCaptureRows(t *testing.T, path string) []Receipt {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	scan := bufio.NewScanner(f)
	var rows []Receipt
	for scan.Scan() {
		var row Receipt
		if err := json.Unmarshal(scan.Bytes(), &row); err != nil {
			t.Fatal(err)
		}
		rows = append(rows, row)
	}
	if err := scan.Err(); err != nil {
		t.Fatal(err)
	}
	return rows
}
func TestCaptureCallbackAfterDurabilityMatchesOffline(t *testing.T) {
	cfg := testCaptureConfig(t, 3)
	socket := &fakeCaptureSocket{frames: []string{streamBook, streamTrades, `{"channel":"heartbeat"}`}}
	var delivered []Event
	cfg.OnEvent = func(ctx context.Context, event Event) error {
		if ctx.Err() != nil {
			t.Fatal(ctx.Err())
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("missing callback deadline")
		}
		rows := readCaptureRows(t, cfg.Output)
		if rows[len(rows)-1].ReceiptIndex != event.SourceReceiptIndex {
			t.Fatalf("callback preceded source persistence: %+v %+v", event, rows)
		}
		delivered = append(delivered, event)
		return nil
	}
	manifest, err := capture(context.Background(), cfg, func(context.Context) (captureSocket, error) { return socket, nil })
	if err != nil {
		t.Fatal(err)
	}
	full, err := Normalize(cfg.Output)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(delivered, full) {
		t.Fatalf("capture callbacks differ from offline: %+v %+v", delivered, full)
	}
	if !manifest.BookValidAtEnd || manifest.Status != "frame_limit_reached" || manifest.ValidSnapshots != 1 || manifest.Gaps != 0 || manifest.Frames != 3 || !socket.closed || socket.writes != 2 || socket.readLimit != maxWireFrame {
		t.Fatalf("bad manifest/socket %+v %+v", manifest, socket)
	}
	rows := readCaptureRows(t, cfg.Output)
	if rows[len(rows)-1].Kind != "capture_end" {
		t.Fatal(rows)
	}
}
func TestCaptureCallbackFailureIsTerminalAndRecorded(t *testing.T) {
	cfg := testCaptureConfig(t, 3)
	socket := &fakeCaptureSocket{frames: []string{streamBook, streamTrades, streamBook}}
	var delivered []Event
	consumerError := errors.New("consumer unavailable")
	cfg.OnEvent = func(ctx context.Context, event Event) error {
		delivered = append(delivered, event)
		if event.Kind == "trade" {
			return consumerError
		}
		return nil
	}
	manifest, err := capture(context.Background(), cfg, func(context.Context) (captureSocket, error) { return socket, nil })
	if !errors.Is(err, consumerError) {
		t.Fatal(err)
	}
	if manifest.Status != "incomplete" || manifest.BookValidAtEnd || manifest.Gaps != 1 || manifest.Frames != 2 || socket.index != 2 {
		t.Fatalf("continued after consumer failure: %+v %+v", manifest, socket)
	}
	rows := readCaptureRows(t, cfg.Output)
	if rows[len(rows)-2].Kind != "gap" || rows[len(rows)-2].Reason != "event_delivery_failed" || rows[len(rows)-1].Valid {
		t.Fatal(rows)
	}
	if delivered[len(delivered)-1].Kind != "gap" {
		t.Fatal("terminal gap was not attempted", delivered)
	}
	var onDisk CaptureManifest
	b, e := os.ReadFile(cfg.Output + ".manifest.json")
	if e != nil {
		t.Fatal(e)
	}
	if e = json.Unmarshal(b, &onDisk); e != nil || onDisk.Status != "incomplete" || onDisk.BookValidAtEnd {
		t.Fatal(string(b), e)
	}
}
func TestCaptureChecksumFailureEmitsSameGapsAsFile(t *testing.T) {
	cfg := testCaptureConfig(t, 2)
	socket := &fakeCaptureSocket{frames: []string{streamBook, streamBadBook}}
	var delivered []Event
	cfg.OnEvent = func(ctx context.Context, event Event) error { delivered = append(delivered, event); return nil }
	manifest, err := capture(context.Background(), cfg, func(context.Context) (captureSocket, error) { return socket, nil })
	if err != nil {
		t.Fatal(err)
	}
	full, err := Normalize(cfg.Output)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(delivered, full) || len(full) != 3 || full[1].Kind != "gap" || full[2].Kind != "gap" || manifest.BookValidAtEnd || manifest.Status != "recorded_with_gaps" {
		t.Fatal(manifest, delivered, full)
	}
}
func TestCaptureCallbackContextCancellation(t *testing.T) {
	cfg := testCaptureConfig(t, 1)
	cfg.Duration = 20 * time.Millisecond
	socket := &fakeCaptureSocket{frames: []string{streamBook}}
	cfg.OnEvent = func(ctx context.Context, event Event) error { <-ctx.Done(); return ctx.Err() }
	started := time.Now()
	manifest, err := capture(context.Background(), cfg, func(context.Context) (captureSocket, error) { return socket, nil })
	if !errors.Is(err, context.DeadlineExceeded) || manifest.BookValidAtEnd || manifest.Status != "incomplete" || time.Since(started) > time.Second {
		t.Fatal(manifest, err)
	}
}
func TestCaptureWithoutCallbackRemainsPureRecorder(t *testing.T) {
	cfg := testCaptureConfig(t, 1)
	socket := &fakeCaptureSocket{frames: []string{streamBook}}
	manifest, err := capture(context.Background(), cfg, func(context.Context) (captureSocket, error) { return socket, nil })
	if err != nil || manifest.Status != "frame_limit_reached" {
		t.Fatal(manifest, err)
	}
}
