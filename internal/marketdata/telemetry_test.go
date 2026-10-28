package marketdata

import (
	"bufio"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/liquiditylab/liquiditylab/internal/telemetry"
)

func TestCaptureTelemetryMonotonicOriginAndOfflineParity(t *testing.T) {
	cfg := testCaptureConfig(t, 3)
	recorder := telemetry.New()
	cfg.Telemetry = recorder
	socket := &fakeCaptureSocket{frames: []string{streamBook, streamTrades, `{"channel":"heartbeat"}`}}
	started := time.Now()
	var delivered []Event
	cfg.OnEvent = func(ctx context.Context, event Event) error {
		origin, ok := SourceReceiptStart(ctx)
		if !ok || origin.Before(started) || origin.After(time.Now()) {
			t.Fatal("instrumented callback lost its local receive origin")
		}
		delivered = append(delivered, event)
		return nil
	}
	_, err := capture(context.Background(), cfg, func(context.Context) (captureSocket, error) { return socket, nil })
	if err != nil {
		t.Fatal(err)
	}
	offline, err := Normalize(cfg.Output)
	if err != nil || !reflect.DeepEqual(offline, delivered) {
		t.Fatal("instrumentation changed normalized event semantics", err)
	}
	s := recorder.Snapshot()
	if s.Counters[telemetry.CaptureFrames].Value != 3 || s.Counters[telemetry.CaptureInputBytes].Value != uint64(len(streamBook)+len(streamTrades)+len(`{"channel":"heartbeat"}`)) || s.Counters[telemetry.CaptureInputBytes].Unit != "bytes" {
		t.Fatal("frame/byte accounting differs from successful wire reads")
	}
	if s.Counters[telemetry.CapturePersistAttempts].Value != 3 || s.Counters[telemetry.CapturePersistErrors].Value != 0 || s.Counters[telemetry.CaptureDeliveryAttempts].Value != uint64(len(delivered)) || s.Counters[telemetry.CaptureDeliveryErrors].Value != 0 {
		t.Fatal("persistence/delivery denominators differ")
	}
	if s.Distributions[telemetry.CapturePersistNS].Count != 3 || s.Distributions[telemetry.CaptureReadToDurableNS].Count != 3 || s.Distributions[telemetry.CaptureReceiptToConsumerNS].Count != uint64(len(delivered)) {
		t.Fatal("receive/durability/delivery latency sample counts differ")
	}
	if _, ok := SourceReceiptStart(context.Background()); ok {
		t.Fatal("synthetic/offline input fabricated a process-local source origin")
	}
}

func TestCaptureTelemetryFailedWriteNeverClaimsDurability(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "closed.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	recorder := telemetry.New()
	sink := captureSink{recorder: NewRecorder(bufio.NewWriter(f), "failed-session", 10), file: f, started: time.Now(), ctx: context.Background(), normalizer: NewNormalizer(), telemetry: recorder,
		onEvent: func(context.Context, Event) error { t.Fatal("callback preceded durable source"); return nil }}
	if err := sink.rawAt(streamBook, true, time.Now()); err == nil {
		t.Fatal("closed source write succeeded")
	}
	s := recorder.Snapshot()
	if s.Counters[telemetry.CapturePersistAttempts].Value != 1 || s.Counters[telemetry.CapturePersistErrors].Value != 1 || s.Counters[telemetry.CaptureDeliveryAttempts].Value != 0 || s.Distributions[telemetry.CapturePersistNS].Count != 1 || s.Distributions[telemetry.CaptureReadToDurableNS].Count != 0 {
		t.Fatal("failed write became durable receipt or callback")
	}
}

func TestCaptureTelemetryConsumerFailureAndTerminalGap(t *testing.T) {
	cfg := testCaptureConfig(t, 1)
	recorder := telemetry.New()
	cfg.Telemetry = recorder
	consumerErr := errors.New("bounded consumer failure")
	cfg.OnEvent = func(context.Context, Event) error { return consumerErr }
	socket := &fakeCaptureSocket{frames: []string{streamBook}}
	manifest, err := capture(context.Background(), cfg, func(context.Context) (captureSocket, error) { return socket, nil })
	if !errors.Is(err, consumerErr) || manifest.Status != "incomplete" {
		t.Fatal("consumer failure did not invalidate capture", err)
	}
	s := recorder.Snapshot()
	if s.Counters[telemetry.CaptureDeliveryAttempts].Value != 2 || s.Counters[telemetry.CaptureDeliveryErrors].Value != 2 || s.Counters[telemetry.CapturePersistAttempts].Value != 2 || s.Distributions[telemetry.CaptureReadToDurableNS].Count != 1 || s.Distributions[telemetry.CaptureReceiptToConsumerNS].Count != 1 {
		t.Fatal("normal and terminal delivery accounting or origin scope differs")
	}
}
