package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/liquiditylab/liquiditylab/internal/api"
	d "github.com/liquiditylab/liquiditylab/internal/domain"
	"github.com/liquiditylab/liquiditylab/internal/marketdata"
	rt "github.com/liquiditylab/liquiditylab/internal/runtime"
	pg "github.com/liquiditylab/liquiditylab/internal/store/postgres"
)

const daemonTestToken = "test-only-daemon-bearer-01234567890123456789"

func daemonWebDir(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	if err := os.Mkdir(filepath.Join(directory, "assets"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"index.html": `<html><body>PAPER test<script src="/assets/app.js"></script></body></html>`, "assets/app.js": "console.log('paper');", "assets/app.js.map": "secret source map", ".env": "secret credentials", "raw.jsonl": "secret capture"} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return directory
}

func daemonOptions(t *testing.T) paperServerOptions {
	t.Helper()
	return paperServerOptions{listen: "127.0.0.1:0", schema: "ll_daemon_unit", account: "synthetic-test-account", source: "synthetic", webDir: daemonWebDir(t),
		initialBase: d.Zero, initialQuote: d.MustDecimal("20"), duration: 2 * time.Second, maxFrames: 100, maxCaptureBytes: 1 << 20}
}

func daemonPostgres(t *testing.T, opt *paperServerOptions) string {
	t.Helper()
	dsn := os.Getenv("LIQUIDITYLAB_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("LIQUIDITYLAB_TEST_POSTGRES_DSN is not set")
	}
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatal(err)
	}
	opt.schema = "ll_daemon_test_" + hex.EncodeToString(suffix[:])
	schema := opt.schema
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		conn, err := pgx.Connect(ctx, dsn)
		if err != nil {
			t.Error("cannot connect to drop owned randomized schema")
			return
		}
		defer conn.Close(ctx)
		if _, err := conn.Exec(ctx, "DROP SCHEMA IF EXISTS "+pgx.Identifier{schema}.Sanitize()+" CASCADE"); err != nil {
			t.Error("cannot drop owned randomized schema")
		}
	})
	return dsn
}

func TestPaperServerOptionsAndLiteralLoopback(t *testing.T) {
	opt := daemonOptions(t)
	if err := opt.validate(); err != nil {
		t.Fatal(err)
	}
	for _, address := range []string{"0.0.0.0:8080", "[::]:8080", "localhost:8080", "192.0.2.1:8080", "unix:/tmp/server.sock", ":8080", "127.0.0.1:http", "127.0.0.1:65536", "127.0.0.1:"} {
		bad := opt
		bad.listen = address
		if err := bad.validate(); err == nil {
			t.Fatalf("nonliteral/nonloopback listener accepted: %s", address)
		}
	}
	for _, schema := range []string{"", "public", "pg_catalog", "information_schema", "invalid-schema"} {
		bad := opt
		bad.schema = schema
		if err := bad.validate(); err == nil {
			t.Fatalf("unisolated schema accepted: %s", schema)
		}
	}
	for _, account := range []string{"", ".", "..", "../account", "has space", strings.Repeat("x", 129)} {
		bad := opt
		bad.account = account
		if err := bad.validate(); err == nil {
			t.Fatalf("invalid account accepted: %s", account)
		}
	}
	for _, duration := range []time.Duration{0, -1, 25 * time.Hour} {
		bad := opt
		bad.duration = duration
		if err := bad.validate(); err == nil {
			t.Fatal("unbounded duration accepted")
		}
	}
	opt.source = "kraken"
	if err := opt.validate(); err == nil {
		t.Fatal("kraken source accepted without new raw output")
	}
	opt.captureOutput = filepath.Join(t.TempDir(), "raw.jsonl")
	if err := opt.validate(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(opt.captureOutput, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := opt.validate(); err == nil {
		t.Fatal("existing raw capture was accepted")
	}
}

func TestPaperStaticAllowlistAndImmutableSnapshot(t *testing.T) {
	directory := daemonWebDir(t)
	handler, err := paperStatic(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, route := range []string{"/", "/index.html", "/assets/app.js"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest("GET", route, nil))
		if response.Code != 200 || response.Header().Get("Content-Security-Policy") == "" || response.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("built asset %s unavailable or missing security headers: %d", route, response.Code)
		}
	}
	for _, route := range []string{"/assets", "/assets/", "/.env", "/raw.jsonl", "/assets/app.js.map", "/../index.html", "/assets/../index.html", "/%2eenv", "/assets%2fapp.js", "/assets\\app.js", "/missing"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest("GET", route, nil))
		if response.Code != 404 || strings.Contains(response.Body.String(), "secret") {
			t.Fatalf("unsafe static route %s was served: %d %s", route, response.Code, response.Body.String())
		}
	}
	if err := os.WriteFile(filepath.Join(directory, "assets/app.js"), []byte("changed after startup"), 0o600); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest("GET", "/assets/app.js", nil))
	if strings.Contains(response.Body.String(), "changed after startup") {
		t.Fatal("static handler reads mutable files after startup")
	}
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest("POST", "/", nil))
	if response.Code != 405 {
		t.Fatal("static handler accepts writes")
	}
}

func TestPaperStaticRejectsSymlinkAndMissingBuild(t *testing.T) {
	for _, parentLink := range []bool{false, true} {
		directory := daemonWebDir(t)
		if parentLink {
			link := filepath.Join(t.TempDir(), "linked-dist")
			if err := os.Symlink(directory, link); err != nil {
				t.Fatal(err)
			}
			directory = link
		} else if err := os.Symlink(filepath.Join(directory, ".env"), filepath.Join(directory, "assets", "leaked.js")); err != nil {
			t.Fatal(err)
		}
		if _, err := paperStatic(directory); err == nil {
			t.Fatal("built asset symlink accepted")
		}
	}
	if _, err := paperStatic(t.TempDir()); err == nil {
		t.Fatal("missing index build accepted")
	}
}

func daemonView(t *testing.T, daemon *paperDaemon) rt.View {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	view, err := daemon.actor.View(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return view
}

func daemonWait(t *testing.T, daemon *paperDaemon, condition func(rt.View) bool) rt.View {
	t.Helper()
	until := time.Now().Add(4 * time.Second)
	for time.Now().Before(until) {
		view := daemonView(t, daemon)
		if condition(view) {
			return view
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("paper authority condition was not reached")
	return rt.View{}
}

func daemonRequest(t *testing.T, handler http.Handler, method, route, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var encoded []byte
	var err error
	if body != nil {
		encoded, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	request := httptest.NewRequest(method, route, bytes.NewReader(encoded))
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func daemonNewTest(t *testing.T, opt paperServerOptions, dsn string, source paperSource) *paperDaemon {
	t.Helper()
	static, err := paperStatic(opt.webDir)
	if err != nil {
		t.Fatal(err)
	}
	daemon, err := newPaperDaemon(context.Background(), opt, dsn, daemonTestToken, static, io.Discard, source)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := daemon.Close(); err != nil {
			t.Error(err)
		}
	})
	return daemon
}

func TestPaperDaemonHTTPActualActorPreviewControlAndShutdown(t *testing.T) {
	opt := daemonOptions(t)
	dsn := daemonPostgres(t, &opt)
	daemon := daemonNewTest(t, opt, dsn, syntheticPaperSource)
	ready := daemonWait(t, daemon, func(v rt.View) bool { return v.Account.Ready })
	if ready.State.Parent != nil || ready.State.DataSource != "synthetic" || ready.State.Fees.Source != "synthetic-assumption" {
		t.Fatal("bootstrap auto-created a plan or concealed synthetic assumptions")
	}
	if response := daemonRequest(t, daemon.handler, "GET", "/v1/accounts", "", nil); response.Code != 401 {
		t.Fatal("trader API does not require bearer authentication")
	}
	route := "/v1/accounts/" + daemon.key.TenantID + "/" + daemon.key.AccountID
	if response := daemonRequest(t, daemon.handler, "GET", "/v1/accounts/another/account", daemonTestToken, nil); response.Code != 404 {
		t.Fatal("bearer scope escaped its account")
	}
	plan := d.DefaultPlan()
	plan.BaseBalance, plan.QuoteBudget = d.Zero, d.MustDecimal("20")
	plan.Quantity, plan.MaxChildQuantity = d.MustDecimal("0.02"), d.MustDecimal("0.01")
	plan.Deadline = 10 * d.Second
	response := daemonRequest(t, daemon.handler, "POST", route+"/preview", daemonTestToken, map[string]any{"plan": plan, "policy": "scheduled"})
	var preview api.PreviewDTO
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &preview) != nil || !preview.Ready || preview.Intent == nil || preview.CompletionGuaranteed {
		t.Fatalf("actual-actor preview failed: %d %s", response.Code, response.Body.String())
	}
	if view := daemonView(t, daemon); view.Account.Revision != ready.Account.Revision || view.State.Parent != nil {
		t.Fatal("preview mutated the paper account")
	}
	response = daemonRequest(t, daemon.handler, "POST", route+"/commands", daemonTestToken, map[string]any{"id": "http-test-create", "kind": "create", "expected_revision": ready.Account.Revision, "plan_id": "http-test-plan", "plan": plan, "policy": "scheduled"})
	if response.Code != 200 {
		t.Fatalf("create plan: %d %s", response.Code, response.Body.String())
	}
	paused := daemonView(t, daemon)
	response = daemonRequest(t, daemon.handler, "POST", route+"/commands", daemonTestToken, map[string]any{"id": "http-test-start", "kind": "start", "expected_revision": paused.Account.Revision})
	if response.Code != 200 {
		t.Fatalf("start plan: %d %s", response.Code, response.Body.String())
	}
	filled := daemonWait(t, daemon, func(v rt.View) bool { return len(v.State.Fills) == 1 })
	if filled.State.DecisionCount == 0 || filled.State.Parent.Status != "active" {
		t.Fatal("actual automatic scheduler did not produce a partial simulated fill")
	}
	response = daemonRequest(t, daemon.handler, "GET", route, daemonTestToken, nil)
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"data_source":"synthetic"`) || !strings.Contains(response.Body.String(), `"profile":"paper"`) {
		t.Fatal("account HTTP DTO does not identify simulated data")
	}
	static, _ := paperStatic(opt.webDir)
	if _, err := newPaperDaemon(context.Background(), opt, dsn, daemonTestToken, static, io.Discard, syntheticPaperSource); err == nil || !strings.Contains(err.Error(), "refusing reset") {
		t.Fatal("existing account reset/takeover was not refused")
	}
	if err := daemon.Close(); err != nil {
		t.Fatal(err)
	}
	response = daemonRequest(t, daemon.handler, "POST", route+"/commands", daemonTestToken, map[string]any{"id": "after-close", "kind": "resume", "expected_revision": filled.Account.Revision})
	if response.Code != 503 {
		t.Fatal("shutdown admits new account mutations")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	store, err := pg.Open(ctx, dsn, opt.schema)
	if err != nil {
		t.Fatal("open durable shutdown verification failed")
	}
	defer store.Close()
	account, err := store.GetAccount(ctx, daemon.key)
	if err != nil {
		t.Fatal("read durable shutdown account failed")
	}
	state, err := rt.DecodeSnapshot(account.Snapshot)
	if err != nil || state.Parent == nil || state.Parent.Status != "cancelled" || len(account.Intents) != 0 || len(account.Reservations) != 0 || account.ObservedSequence != account.ProcessedSequence {
		t.Fatalf("shutdown left unresolved paper exposure: parent=%+v err=%v", state.Parent, err)
	}
}

func daemonSnapshot() marketdata.Event {
	return marketdata.Event{Kind: "snapshot", Type: "snapshot", Symbol: "BTC/USD", Valid: true, ReceiptSessionID: "stable-daemon-test-session", Bids: [][]string{{"99.99", "2"}}, Asks: [][]string{{"100.01", "2"}}}
}

func TestPaperDaemonSourceGapNeverAutoReconcilesLaterSnapshot(t *testing.T) {
	opt := daemonOptions(t)
	dsn := daemonPostgres(t, &opt)
	source := func(ctx context.Context, deliver func(context.Context, marketdata.Event) error) error {
		first := daemonSnapshot()
		if err := deliver(ctx, first); err != nil {
			return err
		}
		gap := marketdata.Event{Kind: "gap", Symbol: "BTC/USD", ReceiptSessionID: first.ReceiptSessionID, ReceiptIndex: 1}
		// Event.Valid is false: explicit source gap, then a good fresh snapshot.
		if err := deliver(ctx, gap); err != nil {
			return err
		}
		first.ReceiptIndex = 2
		if err := deliver(ctx, first); err != nil {
			return err
		}
		<-ctx.Done()
		return ctx.Err()
	}
	daemon := daemonNewTest(t, opt, dsn, source)
	view := daemonWait(t, daemon, func(v rt.View) bool { return v.State.RecoveryRequired && v.State.Book.Valid })
	if view.Account.Ready || view.State.Parent != nil || !daemon.reconciled {
		t.Fatal("later resnapshot silently restored readiness or created a plan")
	}
	route := "/v1/accounts/" + daemon.key.TenantID + "/" + daemon.key.AccountID
	response := daemonRequest(t, daemon.handler, "GET", route, daemonTestToken, nil)
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"recovery_required":true`) {
		t.Fatal("source gap account no longer inspectable")
	}
}

func TestPaperDaemonSourceFailureInvalidatesAndKeepsInspection(t *testing.T) {
	opt := daemonOptions(t)
	dsn := daemonPostgres(t, &opt)
	source := func(ctx context.Context, deliver func(context.Context, marketdata.Event) error) error {
		if err := deliver(ctx, daemonSnapshot()); err != nil {
			return err
		}
		return errors.New("sensitive upstream details deliberately not logged")
	}
	daemon := daemonNewTest(t, opt, dsn, source)
	view := daemonWait(t, daemon, func(v rt.View) bool { return v.State.RecoveryRequired })
	if view.Account.Ready || view.State.Book.Valid {
		t.Fatal("source failure did not durably invalidate readiness")
	}
	response := daemonRequest(t, daemon.handler, "GET", "/v1/accounts", daemonTestToken, nil)
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"recovery_required":true`) {
		t.Fatal("source failure did not preserve authenticated inspection")
	}
}

func TestPaperServeDurationAndContextShutdown(t *testing.T) {
	for _, mode := range []string{"duration", "context"} {
		t.Run(mode, func(t *testing.T) {
			opt := daemonOptions(t)
			dsn := daemonPostgres(t, &opt)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "duration" {
				opt.duration = 200 * time.Millisecond
			} else {
				timer := time.AfterFunc(300*time.Millisecond, cancel)
				defer timer.Stop()
			}
			start := time.Now()
			if err := servePaper(ctx, opt, dsn, daemonTestToken, io.Discard); err != nil {
				t.Fatal(err)
			}
			if time.Since(start) > 2*time.Second {
				t.Fatal("paper server did not obey bounded lifetime")
			}
		})
	}
}
