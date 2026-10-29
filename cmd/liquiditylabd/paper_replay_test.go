package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/liquiditylab/liquiditylab/internal/api"
	"github.com/liquiditylab/liquiditylab/internal/historicalreplay"
	"github.com/liquiditylab/liquiditylab/internal/marketarchive"
	"github.com/liquiditylab/liquiditylab/internal/marketdata"
	pg "github.com/liquiditylab/liquiditylab/internal/store/postgres"
)

func TestPaperReplayArchiveOptions(t *testing.T) {
	opt := daemonOptions(t)
	opt.replayArchive = "relative/archive"
	if opt.validate() == nil {
		t.Fatal("relative replay archive accepted")
	}
	opt.replayArchive = t.TempDir()
	if err := opt.validate(); err != nil {
		t.Fatal(err)
	}
	if service, store, err := openPaperReplayJobs(context.Background(), paperServerOptions{}, "unused"); err != nil || service != nil || store != nil {
		t.Fatal("disabled replay opened resources", err)
	}
	opt.replayArchive += "/missing"
	if _, _, err := openPaperReplayJobs(context.Background(), opt, "unused"); err == nil {
		t.Fatal("missing archive accepted")
	}
}

func TestPaperDaemonReplayHTTPWorkerAndScopedDurableResult(t *testing.T) {
	opt := daemonOptions(t)
	dsn := daemonPostgres(t, &opt)
	opt.replayArchive = t.TempDir()
	fixture := "../../examples/recordings/kraken-btcusd-20261028/"
	m, err := marketarchive.Import(context.Background(), marketarchive.Config{RawPath: fixture + "public-capture.jsonl", ManifestPath: fixture + "public-capture.manifest.json", ArchiveRoot: opt.replayArchive})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("../../examples/replay/retained-short-compare.json")
	if err != nil {
		t.Fatal(err)
	}
	source := func(ctx context.Context, emit func(context.Context, marketdata.Event) error) error {
		<-ctx.Done()
		return ctx.Err()
	}
	daemon := daemonNewTest(t, opt, dsn, source)
	before := daemonView(t, daemon)
	route := "/v1/accounts/" + daemon.key.TenantID + "/" + daemon.key.AccountID + "/replays"
	body := map[string]any{"id": "real-archive-comparison", "archive_id": m.ID, "spec": json.RawMessage(raw)}
	response := daemonRequest(t, daemon.handler, http.MethodPost, route, daemonTestToken, body)
	if response.Code != http.StatusAccepted {
		t.Fatalf("enqueue HTTP %d: %s", response.Code, response.Body.String())
	}
	if duplicate := daemonRequest(t, daemon.handler, http.MethodPost, route, daemonTestToken, body); duplicate.Code != http.StatusOK || !strings.Contains(duplicate.Body.String(), `"duplicate":true`) {
		t.Fatal("exact accepted HTTP retry was not discovered", duplicate.Body.String())
	}
	var detail api.ReplayDetailDTO
	until := time.Now().Add(5 * time.Second)
	for {
		response = daemonRequest(t, daemon.handler, http.MethodGet, route+"/real-archive-comparison", daemonTestToken, nil)
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &detail) != nil {
			t.Fatalf("get HTTP %d: %s", response.Code, response.Body.String())
		}
		if detail.Status == "succeeded" || detail.Status == "failed" {
			break
		}
		if time.Now().After(until) {
			t.Fatal("worker did not publish durable result")
		}
		time.Sleep(10 * time.Millisecond)
	}
	var report historicalreplay.Report
	if detail.Status != "succeeded" || detail.Attempts != 1 || detail.ResultSHA256 == "" || json.Unmarshal(detail.Result, &report) != nil || report.Archive.ID != m.ID || report.SourceSessionBinding != nil || !report.EstimateOnly || !report.Reference.Comparable {
		t.Fatalf("wrong replay result status=%s error=%s", detail.Status, detail.ErrorCode)
	}
	download := daemonRequest(t, daemon.handler, http.MethodGet, route+"/real-archive-comparison/result", daemonTestToken, nil)
	digest := sha256.Sum256(download.Body.Bytes())
	if download.Code != http.StatusOK || download.Header().Get("X-Content-SHA256") != detail.ResultSHA256 || hex.EncodeToString(digest[:]) != detail.ResultSHA256 || !strings.HasSuffix(download.Body.String(), "\n") {
		t.Fatal("HTTP export did not preserve the exact stored report bytes/hash")
	}
	list := daemonRequest(t, daemon.handler, http.MethodGet, route, daemonTestToken, nil)
	if list.Code != http.StatusOK || strings.Contains(list.Body.String(), `"resolved_spec"`) || strings.Contains(list.Body.String(), `"input":`) {
		t.Fatal("metadata history exposed report bodies", list.Body.String())
	}
	other := strings.Replace(route, daemon.key.AccountID, "another-account", 1)
	if denied := daemonRequest(t, daemon.handler, http.MethodGet, other+"/real-archive-comparison", daemonTestToken, nil); denied.Code != http.StatusNotFound {
		t.Fatal("other account's replay was visible")
	}
	after := daemonView(t, daemon)
	if before.Account.Revision != after.Account.Revision || !reflect.DeepEqual(before.Account.Balances, after.Account.Balances) || !reflect.DeepEqual(before.State, after.State) {
		t.Fatal("analysis HTTP flow changed financial account")
	}
	if err := daemon.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-daemon.replayDone:
	default:
		t.Fatal("daemon closed without joining replay worker")
	}
	// A fresh independent pool can still read the completed job after shutdown.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	store, err := pg.OpenWithOptions(ctx, dsn, opt.schema, pg.Options{MaxConnections: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	job, err := store.Replay(ctx, daemon.key, "real-archive-comparison")
	if err != nil || job.Status != "succeeded" || job.ResultSHA256 != detail.ResultSHA256 {
		t.Fatal("shutdown lost durable result", err)
	}
}
