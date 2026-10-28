package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/liquiditylab/liquiditylab/internal/marketdata"
	rt "github.com/liquiditylab/liquiditylab/internal/runtime"
	"github.com/liquiditylab/liquiditylab/internal/telemetry"
)

func TestPaperMetricsDisabledAndIncompleteMarker(t *testing.T) {
	r, f, err := openPaperMetrics("")
	if r != nil || f != nil || err != nil {
		t.Fatal("disabled metrics allocated diagnostics")
	}
	path := filepath.Join(t.TempDir(), "metrics.json")
	r, f, err = openPaperMetrics(path)
	if err != nil || r == nil || f == nil {
		t.Fatalf("open diagnostics: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var initial paperMetricsReport
	if err := json.Unmarshal(data, &initial); err != nil || initial.CaptureState != "incomplete" || initial.KnownFinal || initial.Metrics != nil {
		t.Fatalf("incomplete marker misrepresented: %s (%v)", data, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("diagnostics permissions: %v %v", info, err)
	}
	opt := paperServerOptions{metricsOutput: path}
	_, err = newPaperDaemon(context.Background(), opt, "invalid-dsn-that-must-not-be-opened", daemonTestToken, http.NotFoundHandler(), io.Discard, syntheticPaperSource)
	if err == nil || !strings.Contains(err.Error(), "new writable local output") {
		t.Fatalf("existing diagnostics did not fail before database work: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(data) != string(after) {
		t.Fatal("existing diagnostics were overwritten")
	}
}

func TestPaperMetricsShutdownSnapshotIsLocalAndRedacted(t *testing.T) {
	opt := daemonOptions(t)
	opt.metricsOutput = filepath.Join(t.TempDir(), "shutdown.json")
	dsn := daemonPostgres(t, &opt)
	source := func(ctx context.Context, deliver func(context.Context, marketdata.Event) error) error {
		if err := deliver(ctx, daemonSnapshot()); err != nil {
			return err
		}
		<-ctx.Done()
		return ctx.Err()
	}
	daemon := daemonNewTest(t, opt, dsn, source)
	daemonWait(t, daemon, func(v rt.View) bool { return v.Account.Ready })
	for _, route := range []string{"/metrics", "/v1/metrics"} {
		response := daemonRequest(t, daemon.handler, "GET", route, daemonTestToken, nil)
		if response.Code != http.StatusNotFound {
			t.Fatalf("process metrics accessible through %s: %d", route, response.Code)
		}
	}
	if err := daemon.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(opt.metricsOutput)
	if err != nil {
		t.Fatal(err)
	}
	frames := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(frames) != 2 {
		t.Fatalf("expected initial marker plus final snapshot, got %d frames", len(frames))
	}
	var report paperMetricsReport
	if err := json.Unmarshal([]byte(frames[1]), &report); err != nil || report.Version != "paper-metrics-v1" || report.CaptureState != "shutdown_snapshot" || !report.KnownFinal || report.Metrics == nil || report.Pool == nil {
		t.Fatalf("shutdown diagnostics invalid: %v", err)
	}
	if report.Metrics.Counters[telemetry.ActorAdmitted].Value == 0 || report.Metrics.Counters[telemetry.DBTransactionsCommitted].Value == 0 || report.Metrics.Gauges[telemetry.ActorQueued].Value != 0 || report.Metrics.Gauges[telemetry.ActorHandling].Value != 0 {
		t.Fatal("shutdown diagnostics missed real actor/store work or did not drain")
	}
	if report.Metrics.Counters[telemetry.PublicIngressRequests].Value != 1 || report.Metrics.Counters[telemetry.PublicReceiptOriginMissing].Value != 1 || report.Metrics.Counters[telemetry.PublicIngressErrors].Value != 0 || report.Metrics.Distributions[telemetry.PublicIngressNS].Count != 1 || report.Metrics.Distributions[telemetry.PublicReceiptToBookNS].Count != 0 {
		t.Fatal("synthetic daemon ingress fabricated a native source latency")
	}
	for _, secret := range []string{dsn, daemonTestToken, opt.account, opt.schema, "SELECT ", "INSERT INTO"} {
		if strings.Contains(string(data), secret) {
			t.Fatal("diagnostics contain account, credentials, DSN or SQL")
		}
	}
	if err := daemon.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(opt.metricsOutput)
	if err != nil || string(after) != string(data) {
		t.Fatal("repeated close rewrote the snapshot")
	}
}

func TestPaperMetricsOutputCollisionsRejectedBeforeAccountWork(t *testing.T) {
	directory := t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(directory, alias); err != nil {
		t.Fatal(err)
	}
	opt := daemonOptions(t)
	opt.source = "kraken"
	opt.captureOutput = filepath.Join(directory, "raw.jsonl")
	for _, suffix := range []string{"", ".manifest.json", ".manifest.json.tmp"} {
		for _, parent := range []string{directory, alias} {
			opt.metricsOutput = filepath.Join(parent, "raw.jsonl"+suffix)
			if err := opt.validate(); err == nil || !strings.Contains(err.Error(), "must differ") {
				t.Fatalf("output collision accepted: suffix %q (%v)", suffix, err)
			}
			_, err := newPaperDaemon(context.Background(), opt, "invalid-dsn-must-not-be-opened", daemonTestToken, http.NotFoundHandler(), io.Discard, syntheticPaperSource)
			if err == nil || !strings.Contains(err.Error(), "must differ") {
				t.Fatalf("collision took account startup path: %v", err)
			}
		}
	}
	opt.metricsOutput = filepath.Join(directory, "metrics.jsonl")
	if err := opt.validate(); err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(directory, "child")
	if err := os.Mkdir(child, 0o700); err != nil {
		t.Fatal(err)
	}
	childAlias := filepath.Join(t.TempDir(), "child-alias")
	if err := os.Symlink(child, childAlias); err != nil {
		t.Fatal(err)
	}
	opt.captureOutput = childAlias + string(filepath.Separator) + ".." + string(filepath.Separator) + "raw.jsonl"
	opt.metricsOutput = filepath.Join(directory, "raw.jsonl")
	if err := opt.validate(); err == nil || !strings.Contains(err.Error(), "must differ") {
		t.Fatalf("symlink followed by parent component bypassed comparison: %v", err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 1 || entries[0].Name() != "child" || !entries[0].IsDir() {
		t.Fatal("collision validation created files")
	}
}

func TestPaperMetricsPartialAppendPreservesInitialMarker(t *testing.T) {
	path := filepath.Join(t.TempDir(), "partial.jsonl")
	_, f, err := openPaperMetrics(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"capture_state":"shutdown_snapshot","metrics":`); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	frames := strings.Split(string(data), "\n")
	var marker paperMetricsReport
	if len(frames) != 2 || json.Unmarshal([]byte(frames[0]), &marker) != nil || marker.CaptureState != "incomplete" || json.Valid([]byte(frames[1])) {
		t.Fatal("partial append destroyed marker or fabricated a complete snapshot")
	}
}
