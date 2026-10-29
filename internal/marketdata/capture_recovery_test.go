package marketdata

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"reflect"
	"testing"
	"time"
)

func TestCaptureReconnectRequiresSnapshotBeforeTradeDelivery(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		name := "disconnect"
		if corrupt {
			name = "checksum_failure"
		}
		t.Run(name, func(t *testing.T) {
			first := &fakeCaptureSocket{frames: []string{streamBook}}
			if corrupt {
				first.frames = append(first.frames, streamBadBook)
			}
			// A trade arrives on the replacement socket before its snapshot.
			// It must not be delivered against the prior connection's book.
			second := &fakeCaptureSocket{frames: []string{streamTrades, streamBook, streamTrades}}
			cfg := testCaptureConfig(t, uint64(len(first.frames)+len(second.frames)))
			cfg.Duration = 3 * time.Second
			calls := 0
			var delivered []Event
			cfg.OnEvent = func(_ context.Context, event Event) error { delivered = append(delivered, event); return nil }
			manifest, err := capture(context.Background(), cfg, func(context.Context) (captureSocket, error) {
				calls++
				switch calls {
				case 1:
					return first, nil
				case 2:
					return second, nil
				default:
					return nil, errors.New("unexpected third connection")
				}
			})
			if err != nil {
				t.Fatal(err)
			}
			if calls != 2 || manifest.Connections != 2 || manifest.ValidSnapshots != 2 || manifest.Gaps != 1 || manifest.Frames != cfg.MaxFrames || !manifest.BookValidAtEnd || manifest.Status != "recorded_with_gaps" {
				t.Fatalf("reconnect/resnapshot contract failed: calls=%d manifest=%+v", calls, manifest)
			}
			if !first.closed || !second.closed || first.writes != 2 || second.writes != 2 || first.readLimit != maxWireFrame || second.readLimit != maxWireFrame {
				t.Fatal("reconnect did not close/resubscribe bounded sockets")
			}
			offline, err := Normalize(cfg.Output)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(delivered, offline) {
				t.Fatal("live callbacks and retained recovery tape disagree")
			}
			rows := readCaptureRows(t, cfg.Output)
			var lastSnapshot uint64
			trades, gaps, snapshots := 0, 0, 0
			for _, event := range delivered {
				switch event.Kind {
				case "snapshot":
					snapshots++
					lastSnapshot = event.SourceReceiptIndex
				case "gap":
					gaps++
				case "trade":
					trades++
					if snapshots != 2 || event.SourceReceiptIndex <= lastSnapshot || rows[event.SourceReceiptIndex].Raw != streamTrades {
						t.Fatal("trade delivered before replacement snapshot")
					}
				}
			}
			wantGaps := 1
			if corrupt {
				wantGaps = 2
			} // bad raw row plus durable explicit gap
			if snapshots != 2 || trades != 2 || gaps != wantGaps {
				t.Fatalf("recovery tape coverage changed: snapshots=%d trades=%d gaps=%d", snapshots, trades, gaps)
			}
			raw, err := os.ReadFile(cfg.Output)
			if err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(raw)
			if manifest.SHA256 != hex.EncodeToString(sum[:]) || manifest.Bytes != int64(len(raw)) {
				t.Fatal("reconnected capture manifest does not bind exact persisted bytes")
			}
		})
	}
}
