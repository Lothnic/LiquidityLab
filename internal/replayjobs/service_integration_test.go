package replayjobs

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	d "github.com/liquiditylab/liquiditylab/internal/domain"
	"github.com/liquiditylab/liquiditylab/internal/historicalreplay"
	"github.com/liquiditylab/liquiditylab/internal/marketarchive"
	pg "github.com/liquiditylab/liquiditylab/internal/store/postgres"
)

func TestServicePostgresVerifiedArchiveResultAndFinancialIsolation(t *testing.T) {
	dsn := os.Getenv("LIQUIDITYLAB_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("LIQUIDITYLAB_TEST_POSTGRES_DSN is not configured")
	}
	var suffix [12]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatal(err)
	}
	schema := "ll_replay_service_" + hex.EncodeToString(suffix[:])
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	// This pool belongs only to this random schema and analytical test. It is
	// deliberately separate from any financial actor/daemon connection pool.
	store, err := pg.OpenWithOptions(ctx, dsn, schema, pg.Options{MaxConnections: 2, ApplicationName: "replay-service-test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		store.Close()
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		conn, err := pgx.Connect(cleanup, dsn)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close(cleanup)
		if _, err := conn.Exec(cleanup, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE"); err != nil {
			t.Error(err)
		}
	})
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	key := pg.Key{TenantID: "replay-integration", AccountID: "untouched-account"}
	before, err := store.CreateAccount(ctx, key, []pg.Balance{{Currency: "BTC", Total: d.One, Free: d.One}, {Currency: "USD", Total: d.MustDecimal("1000"), Free: d.MustDecimal("1000")}}, json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	fixture := "../../examples/recordings/kraken-btcusd-20261028/"
	manifest, err := marketarchive.Import(ctx, marketarchive.Config{ArchiveRoot: root, RawPath: fixture + "public-capture.jsonl", ManifestPath: fixture + "public-capture.manifest.json"})
	if err != nil {
		t.Fatal(err)
	}
	service, err := New(Config{Queue: store, ArchiveRoot: root, WorkerID: "integration-worker", Workers: 1, JobTimeout: 3 * time.Second, Admission: pg.ReplayAdmission{PerAccount: 2, Global: 2}})
	if err != nil {
		t.Fatal(err)
	}
	job, duplicate, err := service.Enqueue(ctx, key, "retained-comparison", manifest.ID, specBytes(t))
	if err != nil || duplicate || job.Status != "queued" {
		t.Fatalf("enqueue failed: duplicate=%v err=%v", duplicate, err)
	}
	runCtx, stop := context.WithCancel(ctx)
	runDone := make(chan error, 1)
	go func() { runDone <- service.Run(runCtx) }()
	t.Cleanup(func() {
		stop()
		select {
		case err := <-runDone:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("worker did not stop before schema cleanup")
		}
	})
	var finished pg.ReplayJob
	awaitTest(t, "Postgres replay result", func() bool {
		var err error
		finished, err = service.Get(ctx, key, job.ID)
		if err != nil {
			t.Fatal(err)
		}
		return finished.Status == "succeeded" || finished.Status == "failed"
	})
	if finished.Status != "succeeded" || finished.ErrorCode != "" || finished.Attempts != 1 || finished.ResultSHA256 != testHash(finished.Result) || finished.InputSHA256 != testHash(job.Input) {
		t.Fatalf("unexpected terminal replay: status=%s code=%s attempts=%d", finished.Status, finished.ErrorCode, finished.Attempts)
	}
	expected, err := historicalreplay.Compare(ctx, root, manifest.ID, serviceSpec())
	if err != nil {
		t.Fatal(err)
	}
	expectedBytes, err := historicalreplay.Encode(expected)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(finished.Result, expectedBytes) {
		t.Fatal("stored report differs from the exact default historical comparison")
	}
	var report historicalreplay.Report
	if err := json.Unmarshal(finished.Result, &report); err != nil {
		t.Fatal(err)
	}
	if report.Archive.ID != manifest.ID || !report.EstimateOnly || report.SourceSessionBinding != nil || !report.Reference.Comparable || report.Alternatives[0].EstimatedCostReductionQuote == nil {
		t.Fatal("stored report lost source provenance or estimate scope")
	}
	after, err := store.GetAccount(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("analytical replay job changed financial account state")
	}
	list, err := service.List(ctx, key, 0, 10)
	if err != nil || len(list) != 1 || list[0].Input != nil || list[0].Result != nil || list[0].ResultSHA256 != finished.ResultSHA256 {
		t.Fatal("result metadata list lost hash or exposed payloads", err)
	}
}
