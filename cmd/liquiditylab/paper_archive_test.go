package main

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/liquiditylab/liquiditylab/internal/marketarchive"
)

func TestPaperArchiveVerifiedNativeCursorsAndLegacyUnbound(t *testing.T) {
	root := t.TempDir()
	base := "../../examples/recordings/kraken-btcusd-20261028/"
	m, err := marketarchive.Import(context.Background(), marketarchive.Config{RawPath: base + "public-capture.jsonl", ManifestPath: base + "public-capture.manifest.json", ArchiveRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	tape, err := paperArchiveTape(context.Background(), root, m.ID, 16<<20, 10000, 256<<10)
	if err != nil {
		t.Fatal(err)
	}
	if tape.sealedSource == nil || tape.sealedSource.ArchiveID != m.ID || tape.sealedSource.Normalized != m.Normalized || tape.sealedSource.Raw != m.Raw || tape.sealedSource.SourceManifest != m.SourceManifest || tape.sealedSource.NormalizationVersion != m.NormalizationVersion || tape.digest != m.Normalized.SHA256 || len(tape.events) != 45 || len(tape.sourceCursors) != 45 {
		t.Fatal("verified archive provenance was lost")
	}
	for i, cursor := range tape.sourceCursors {
		if cursor.ArchiveID != m.ID || cursor.NativeSessionID != m.Source.SessionID || cursor.NormalizedReceiptIndex != tape.events[i].ReceiptIndex || cursor.NormalizedTimeNS != tape.events[i].Time || cursor.NativeElapsedNS != m.FirstSnapshotElapsedNS+tape.events[i].Time {
			t.Fatalf("cursor lost native clock or identity: %+v", cursor)
		}
		if i > 0 && cursor.RawReceiptIndex < tape.sourceCursors[i-1].RawReceiptIndex {
			t.Fatal("raw cursor order lost")
		}
	}
	if tape.sourceCursors[0].RawReceiptIndex != m.FirstSnapshotSourceReceiptIndex || tape.sourceCursors[0].NativeElapsedNS != m.FirstSnapshotElapsedNS {
		t.Fatal("first snapshot cursor does not match manifest")
	}
	legacy, err := paperTape(base+"normalized-events.jsonl", 16<<20, 10000, 256<<10)
	if err != nil {
		t.Fatal(err)
	}
	if legacy.sealedSource != nil || legacy.sourceCursors != nil || legacy.digest != tape.digest {
		t.Fatal("legacy input inferred archive binding")
	}
	b, err := json.Marshal(paperSummary{SealedSource: legacy.sealedSource})
	if err != nil || !strings.Contains(string(b), `"sealed_source":null`) {
		t.Fatal("legacy summary does not explicitly remain unbound")
	}
	for _, limits := range []struct {
		bytes        int64
		events, line int
	}{{1, 10000, 256 << 10}, {16 << 20, 1, 256 << 10}, {16 << 20, 10000, 128}} {
		if _, err := paperArchiveTape(context.Background(), root, m.ID, limits.bytes, limits.events, limits.line); err == nil {
			t.Fatal("archive bypassed existing paper input bounds")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := paperArchiveTape(ctx, root, m.ID, 16<<20, 10000, 256<<10); err == nil {
		t.Fatal("cancelled archive preparation proceeded")
	}
	if err := os.WriteFile(root+"/"+m.ID+"/events.jsonl", []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := paperArchiveTape(context.Background(), root, m.ID, 16<<20, 10000, 256<<10); err == nil {
		t.Fatal("tampered archive reached paper conversion")
	}
}

func TestPaperArchiveSourceArguments(t *testing.T) {
	opt := paperTestOptions(t, paperTestSnapshot+"\n")
	for name, change := range map[string]func(*paperOptions){
		"events and archive": func(o *paperOptions) { o.archiveRoot = t.TempDir(); o.datasetID = strings.Repeat("a", 64) },
		"archive missing id": func(o *paperOptions) { o.eventsPath = ""; o.archiveRoot = t.TempDir() },
		"id missing archive": func(o *paperOptions) { o.eventsPath = ""; o.datasetID = strings.Repeat("a", 64) },
		"unsafe id":          func(o *paperOptions) { o.eventsPath = ""; o.archiveRoot = t.TempDir(); o.datasetID = "../capture" },
	} {
		t.Run(name, func(t *testing.T) {
			copy := opt
			change(&copy)
			if err := copy.validate(); err == nil {
				t.Fatal("invalid paper source selection accepted")
			}
		})
	}
	opt.eventsPath = ""
	opt.archiveRoot = t.TempDir()
	opt.datasetID = strings.Repeat("a", 64)
	if err := opt.validate(); err != nil {
		t.Fatalf("valid archive arguments rejected: %v", err)
	}
}
