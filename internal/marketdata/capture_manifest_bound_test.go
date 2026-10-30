package marketdata

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCaptureStorageExhaustionRetainsIncompleteRecording(t *testing.T) {
	cfg := testCaptureConfig(t, 3)
	cfg.MaxBytes = 1024
	socket := &fakeCaptureSocket{frames: []string{streamBook, strings.Repeat(" ", 2048) + streamTrades, streamBook}}
	var delivered []Event
	cfg.OnEvent = func(_ context.Context, event Event) error { delivered = append(delivered, event); return nil }
	manifest, err := capture(context.Background(), cfg, func(context.Context) (captureSocket, error) { return socket, nil })
	if !errors.Is(err, ErrCaptureStorageLimit) || manifest.Status != "incomplete" || manifest.BookValidAtEnd {
		t.Fatalf("exhaustion lost invalid/incomplete state: %+v %v", manifest, err)
	}
	info, err := os.Stat(cfg.Output)
	if err != nil || info.Size() > cfg.MaxBytes || info.Size() != manifest.Bytes {
		t.Fatalf("capture byte accounting: %v %+v", err, manifest)
	}
	if socket.index != 2 || len(delivered) != 1 || delivered[0].Kind != "snapshot" {
		t.Fatalf("delivery after failed persistence: %+v reads=%d", delivered, socket.index)
	}
	if _, err := os.Stat(cfg.Output + ".manifest.json"); err != nil {
		t.Fatal(err)
	}
}

func TestCaptureManifestByteBoundBeforeCreatingFiles(t *testing.T) {
	root := t.TempDir()
	if err := writeManifest(filepath.Join(root, "raw.jsonl"), CaptureManifest{Error: strings.Repeat("x", MaxManifestBytes)}); err == nil {
		t.Fatal("oversized manifest admitted")
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("oversized manifest created output: %v %v", entries, err)
	}
}
