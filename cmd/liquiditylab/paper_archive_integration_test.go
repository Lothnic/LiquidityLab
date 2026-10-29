package main

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/liquiditylab/liquiditylab/internal/marketarchive"
	rt "github.com/liquiditylab/liquiditylab/internal/runtime"
	pg "github.com/liquiditylab/liquiditylab/internal/store/postgres"
)

func TestPaperArchivePersistsVerifiedSessionAndDecisionCursors(t *testing.T) {
	opt := paperTestOptions(t, paperTestSnapshot+"\n")
	dsn := paperTestPostgres(t, &opt)
	root := t.TempDir()
	fixture := "../../examples/recordings/kraken-btcusd-20261028/"
	m, err := marketarchive.Import(context.Background(), marketarchive.Config{RawPath: fixture + "public-capture.jsonl", ManifestPath: fixture + "public-capture.manifest.json", ArchiveRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	opt.eventsPath, opt.archiveRoot, opt.datasetID = "", root, m.ID
	var stdout, stderr bytes.Buffer
	if err := runPaper(context.Background(), opt, dsn, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	var summary paperSummary
	if err := json.Unmarshal(stdout.Bytes(), &summary); err != nil {
		t.Fatal(err)
	}
	if !summary.KnownFinal || summary.EventsDelivered != 45 || summary.SealedSource == nil || summary.SealedSource.ArchiveID != m.ID || summary.State.Parent == nil || summary.State.Parent.SourceStartCursor == nil || summary.State.LastDecisionSourceCursor == nil {
		t.Fatalf("missing final bound session/decision: final=%v delivered=%d", summary.KnownFinal, summary.EventsDelivered)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	store, err := pg.Open(ctx, dsn, opt.schema)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	saved, err := store.Session(ctx, summary.Account.Key, summary.State.Parent.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	state, err := rt.DecodeSnapshot(saved.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(state.SealedSource, summary.SealedSource) || !reflect.DeepEqual(state.Parent.SealedSource, state.SealedSource) || state.SourceCursor == nil || state.SourceCursor.NormalizedReceiptIndex != 44 || state.SourceCursor.RawReceiptIndex == 0 || state.Parent.SourceStartCursor.NormalizedReceiptIndex != 0 || state.LastDecisionSourceCursor.NormalizedReceiptIndex > state.SourceCursor.NormalizedReceiptIndex {
		t.Fatal("revision-pinned database session lost immutable source identity or ordered native cursors")
	}
	if state.Parent.SourceStartCursor.NativeElapsedNS != m.FirstSnapshotElapsedNS || state.SourceCursor.NativeElapsedNS != m.FirstSnapshotElapsedNS+state.SourceCursor.NormalizedTimeNS {
		t.Fatal("native archive elapsed time changed into the account clock")
	}
}
