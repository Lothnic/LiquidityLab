package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	d "github.com/liquiditylab/liquiditylab/internal/domain"
	x "github.com/liquiditylab/liquiditylab/internal/execution"
	rt "github.com/liquiditylab/liquiditylab/internal/runtime"
	pg "github.com/liquiditylab/liquiditylab/internal/store/postgres"
)

const paperTestSnapshot = `{"kind":"snapshot","type":"snapshot","time_ns":0,"receipt_index":0,"source_receipt_index":2,"symbol":"BTC/USD","valid":true,"bids":[["99","2"]],"asks":[["101","2"]]}`
const paperTestBook = `{"kind":"book","type":"book","time_ns":1500000000,"receipt_index":1,"source_receipt_index":3,"symbol":"BTC/USD","valid":true}`

func paperTestFile(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func paperTestOptions(t *testing.T, events string) paperOptions {
	t.Helper()
	plan := d.DefaultPlan()
	plan.Quantity, plan.MaxChildQuantity = d.MustDecimal("0.02"), d.MustDecimal("0.01")
	plan.BaseBalance, plan.QuoteBudget, plan.Deadline = d.Zero, d.MustDecimal("10"), 3*d.Second
	body, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	return paperOptions{
		eventsPath: paperTestFile(t, "events.jsonl", events), planPath: paperTestFile(t, "plan.json", string(body)),
		schema: "ll_paper_unit", account: "test-account", policy: x.BaselineScheduled,
		initialBase: d.Zero, initialQuote: d.MustDecimal("100"),
		duration: 2 * time.Second, maxBytes: 1 << 20, maxEvents: 100, maxLineBytes: 64 << 10,
	}
}

func TestPaperTapeNormalizedWireAndStableIdentity(t *testing.T) {
	trade := `{"kind":"trade","type":"trade","time_ns":0,"receipt_index":1,"source_receipt_index":2,"symbol":"BTC/USD","valid":true,"price":"99","quantity":"0.02","side":"sell"}`
	path := paperTestFile(t, "normalized.jsonl", paperTestSnapshot+"\n"+trade+"\n")
	first, err := paperTape(path, 1<<20, 10, 64<<10)
	if err != nil {
		t.Fatal(err)
	}
	second, err := paperTape(path, 1<<20, 10, 64<<10)
	if err != nil || first.digest != second.digest || len(first.digest) != 64 {
		t.Fatalf("unstable recording identity: first=%s second=%s err=%v", first.digest, second.digest, err)
	}
	if len(first.events) != 2 || first.events[0].ReceiptIndex != 0 || first.events[1].ReceiptIndex != 1 || first.events[1].Time != 0 || first.events[1].TradeID != "" {
		t.Fatalf("equal-time causal order or receipt metadata lost: %+v", first.events)
	}
	if !first.events[0].Bids[0].Price.Equal(d.MustDecimal("99")) || !first.events[1].Quantity.Equal(d.MustDecimal("0.02")) {
		t.Fatalf("normalized string money was not converted exactly: %+v", first.events)
	}
}

func TestPaperTapeRejectsMalformedAndUnboundedInputs(t *testing.T) {
	cases := map[string]string{
		"empty":               "",
		"blank line":          paperTestSnapshot + "\n\n",
		"missing time":        strings.Replace(paperTestSnapshot, `"time_ns":0,`, "", 1),
		"negative time":       strings.Replace(paperTestSnapshot, `"time_ns":0`, `"time_ns":-1`, 1),
		"null time":           strings.Replace(paperTestSnapshot, `"time_ns":0`, `"time_ns":null`, 1),
		"seconds format":      strings.Replace(paperTestSnapshot, `"time_ns":0`, `"time":0`, 1),
		"fractional ns":       strings.Replace(paperTestSnapshot, `"time_ns":0`, `"time_ns":0.5`, 1),
		"excessive time":      strings.Replace(paperTestSnapshot, `"time_ns":0`, `"time_ns":86400000000001`, 1),
		"missing receipt":     strings.Replace(paperTestSnapshot, `"receipt_index":0,`, "", 1),
		"missing validity":    strings.Replace(paperTestSnapshot, `"valid":true,`, "", 1),
		"unsupported symbol":  strings.ReplaceAll(paperTestSnapshot, "BTC/USD", "ETH/USD"),
		"negative level":      strings.Replace(paperTestSnapshot, `["99","2"]`, `["99","-2"]`, 1),
		"crossed snapshot":    strings.Replace(paperTestSnapshot, `["99","2"]`, `["102","2"]`, 1),
		"bad level shape":     strings.Replace(paperTestSnapshot, `["99","2"]`, `["99","2","3"]`, 1),
		"numeric money":       strings.Replace(paperTestSnapshot, `["99","2"]`, `[99,"2"]`, 1),
		"unknown kind":        strings.ReplaceAll(paperTestSnapshot, `"snapshot"`, `"ticker"`),
		"inconsistent type":   strings.Replace(paperTestSnapshot, `"type":"snapshot"`, `"type":"book"`, 1),
		"trailing JSON":       paperTestSnapshot + `{}`,
		"repeated receipt":    paperTestSnapshot + "\n" + strings.Replace(paperTestBook, `"receipt_index":1`, `"receipt_index":0`, 1),
		"missing delta index": paperTestSnapshot + "\n" + strings.Replace(paperTestBook, `"receipt_index":1`, `"receipt_index":2`, 1),
		"missing start index": strings.Replace(paperTestSnapshot, `"receipt_index":0`, `"receipt_index":1`, 1),
		"out of order time":   strings.Replace(paperTestSnapshot, `"time_ns":0`, `"time_ns":2000000000`, 1) + "\n" + paperTestBook,
		"trade before book":   `{"kind":"trade","time_ns":0,"receipt_index":0,"symbol":"BTC/USD","valid":true,"price":"99","quantity":"1","side":"sell"}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			path := paperTestFile(t, "bad.jsonl", body)
			if _, err := paperTape(path, 1<<20, 10, 64<<10); err == nil {
				t.Fatal("malformed recording was accepted")
			}
		})
	}
	path := paperTestFile(t, "bounded.jsonl", paperTestSnapshot+"\n"+paperTestBook)
	for _, limits := range []struct {
		bytes  int64
		events int
		line   int
	}{{10, 10, 64 << 10}, {1 << 20, 1, 64 << 10}, {1 << 20, 10, 128}} {
		if _, err := paperTape(path, limits.bytes, limits.events, limits.line); err == nil {
			t.Fatalf("input limit was ignored: %+v", limits)
		}
	}
}

func TestPaperPlanAndFlagsFailBeforeDatabaseMutation(t *testing.T) {
	opt := paperTestOptions(t, paperTestSnapshot)
	if _, err := paperPlan(opt.planPath, d.Zero, d.MustDecimal("9")); err == nil {
		t.Fatal("plan budget larger than explicit synthetic funds accepted")
	}
	oldPlan := paperTestFile(t, "old-plan.json", `{"side":"buy","quantity":"1","limit_price":"101","deadline":2,"max_child_quantity":"1","quote_budget":"1000","base_balance":"0","symbol":"BTC/USD"}`)
	if _, err := paperPlan(oldPlan, d.Zero, d.MustDecimal("1000")); err == nil {
		t.Fatal("legacy seconds plan accepted as a Go plan")
	}
	for _, schema := range []string{"public", "pg_catalog", "information_schema", "contains-dash", ""} {
		bad := opt
		bad.schema = schema
		if err := bad.validate(); err == nil {
			t.Fatalf("unsafe schema %q accepted", schema)
		}
	}
	var stdout, stderr bytes.Buffer
	if err := paper([]string{"--events", opt.eventsPath, "--plan", opt.planPath, "--schema", opt.schema}, &stdout, &stderr); err == nil || !strings.Contains(err.Error(), "explicit") {
		t.Fatalf("missing explicit balances: %v", err)
	}
	if stdout.Len() != 0 {
		t.Fatal("validation failure emitted a fabricated account summary")
	}
}

func paperTestPostgres(t *testing.T, opt *paperOptions) string {
	t.Helper()
	dsn := os.Getenv("LIQUIDITYLAB_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("LIQUIDITYLAB_TEST_POSTGRES_DSN is not set")
	}
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatal(err)
	}
	opt.schema = "ll_cli_test_" + hex.EncodeToString(suffix[:])
	schema := opt.schema
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		conn, err := pgx.Connect(ctx, dsn)
		if err != nil {
			t.Error("connect to clean owned test schema failed")
			return
		}
		defer conn.Close(ctx)
		if _, err := conn.Exec(ctx, "DROP SCHEMA IF EXISTS "+pgx.Identifier{schema}.Sanitize()+" CASCADE"); err != nil {
			t.Error("drop owned randomized test schema failed")
		}
	})
	return dsn
}

func TestPaperCLIRecordedAutomaticSessionAndExistingAccountRefusal(t *testing.T) {
	opt := paperTestOptions(t, paperTestSnapshot+"\n"+paperTestBook+"\n")
	dsn := paperTestPostgres(t, &opt)
	t.Setenv("LIQUIDITYLAB_DATABASE_URL", dsn)
	var stdout, stderr bytes.Buffer
	start := time.Now()
	err := run([]string{"paper", "--events", opt.eventsPath, "--plan", opt.planPath, "--schema", opt.schema,
		"--account", opt.account, "--initial-base", "0", "--initial-quote", "100", "--duration", "2s"}, &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(start) < 1400*time.Millisecond {
		t.Fatal("recorded events were accelerated instead of paced")
	}
	var summary paperSummary
	if err := json.Unmarshal(stdout.Bytes(), &summary); err != nil {
		t.Fatal(err)
	}
	if summary.Profile != "paper" || summary.Source != "recorded-public-data" || !summary.ActorClosed || !summary.KnownFinal || summary.EventsDelivered != 2 || summary.StopReason != "recording_end" {
		t.Fatalf("incomplete or mislabeled paper session: %+v", summary)
	}
	if summary.State.Parent == nil || summary.State.Parent.Status != "cancelled" || summary.State.Parent.Baseline != x.BaselineScheduled || summary.State.DecisionCount == 0 || len(summary.Fills) != 1 || len(summary.Account.Reservations) != 0 || summary.JournalEntries != summary.Account.Revision {
		t.Fatalf("automatic baseline or exact durable summary missing: parent=%+v decisions=%d fills=%d journal=%d revision=%d", summary.State.Parent, summary.State.DecisionCount, len(summary.Fills), summary.JournalEntries, summary.Account.Revision)
	}
	base, quote := d.Zero, d.MustDecimal("100")
	for _, fill := range summary.Fills {
		base = base.Add(fill.Quantity)
		quote = quote.Sub(fill.Quantity.Mul(fill.Price)).Sub(fill.Fee)
	}
	for _, balance := range summary.Account.Balances {
		want := quote
		if balance.Currency == "BTC" {
			want = base
		}
		if !balance.Total.Equal(want) || !balance.Free.Equal(want) || !balance.Locked.IsZero() {
			t.Fatalf("exact fill accounting mismatch: balance=%+v want=%s", balance, want)
		}
	}
	if !strings.Contains(stderr.String(), "PAPER / RECORDED DATA") || !strings.Contains(summary.ReconciliationEvidence, "SIMULATED") {
		t.Fatal("simulated paper reconciliation was not explicitly labeled")
	}
	stdout.Reset()
	if err := runPaper(context.Background(), opt, dsn, &stdout, &stderr); err == nil || !strings.Contains(err.Error(), "refusing to reset") || stdout.Len() != 0 {
		t.Fatalf("existing account was not refused: err=%v stdout=%s", err, stdout.String())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	store, err := pg.Open(ctx, dsn, opt.schema)
	if err != nil {
		t.Fatal("open verification store failed")
	}
	defer store.Close()
	account, err := store.GetAccount(ctx, summary.Account.Key)
	if err != nil || account.Revision != summary.Account.Revision {
		t.Fatalf("refused rerun changed the existing account: revision=%d want=%d err=%v", account.Revision, summary.Account.Revision, err)
	}
}

func TestPaperRecordedGapStopsAndDurablyInvalidatesReadiness(t *testing.T) {
	gap := `{"kind":"gap","type":"gap","time_ns":1300000000,"receipt_index":1,"symbol":"BTC/USD","valid":false,"reason":"synthetic-test-gap"}`
	opt := paperTestOptions(t, paperTestSnapshot+"\n"+gap)
	dsn := paperTestPostgres(t, &opt)
	var stdout, stderr bytes.Buffer
	err := runPaper(context.Background(), opt, dsn, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "feed gap") {
		t.Fatalf("gap did not fail closed: %v", err)
	}
	var summary paperSummary
	if err := json.Unmarshal(stdout.Bytes(), &summary); err != nil {
		t.Fatal(err)
	}
	if summary.StopReason != "feed_gap" || !summary.ActorClosed || summary.Account.Ready || !summary.State.RecoveryRequired || summary.State.Book.Valid || summary.State.Parent == nil || summary.State.Parent.Status != "cancelled" || len(summary.Account.Intents) != 0 || len(summary.Account.Reservations) != 0 {
		t.Fatalf("gap shutdown failed: stop=%s ready=%v recovery=%v parent=%+v intents=%d reservations=%d", summary.StopReason, summary.Account.Ready, summary.State.RecoveryRequired, summary.State.Parent, len(summary.Account.Intents), len(summary.Account.Reservations))
	}
}

func TestPaperDurationAndCancellationAreBounded(t *testing.T) {
	for _, stop := range []string{"duration", "context"} {
		t.Run(stop, func(t *testing.T) {
			opt := paperTestOptions(t, paperTestSnapshot+"\n"+paperTestBook)
			dsn := paperTestPostgres(t, &opt)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			want := "duration_limit"
			if stop == "duration" {
				opt.duration = 200 * time.Millisecond
			} else {
				want = "signal_or_context_cancelled"
				timer := time.AfterFunc(300*time.Millisecond, cancel)
				defer timer.Stop()
			}
			var stdout, stderr bytes.Buffer
			start := time.Now()
			if err := runPaper(ctx, opt, dsn, &stdout, &stderr); err != nil {
				t.Fatal(err)
			}
			var summary paperSummary
			if err := json.Unmarshal(stdout.Bytes(), &summary); err != nil {
				t.Fatal(err)
			}
			if time.Since(start) > 2*time.Second || summary.StopReason != want || summary.EventsDelivered != 1 || !summary.ActorClosed || !summary.KnownFinal || summary.State.Parent == nil || summary.State.Parent.Status != "cancelled" {
				t.Fatalf("bounded shutdown failed: %+v", summary)
			}
		})
	}
}

func TestPaperDatabaseErrorDoesNotExposeConnectionCredentials(t *testing.T) {
	opt := paperTestOptions(t, paperTestSnapshot)
	var stdout, stderr bytes.Buffer
	err := runPaper(context.Background(), opt, "postgres://secret-user:secret-password@127.0.0.1:1/postgres?connect_timeout=1", &stdout, &stderr)
	if err == nil {
		t.Fatal("unreachable database unexpectedly accepted")
	}
	for _, output := range []string{err.Error(), stdout.String(), stderr.String()} {
		if strings.Contains(output, "secret-user") || strings.Contains(output, "secret-password") || strings.Contains(output, "postgres://") {
			t.Fatal("database connection text leaked")
		}
	}
}

func TestPaperSummaryPreservesInternalExactDecimalPrecision(t *testing.T) {
	preciseAsk := "100.000000000000000000000000000000000001"
	snapshot := strings.Replace(paperTestSnapshot, `["101","2"]`, `["`+preciseAsk+`","2"]`, 1)
	opt := paperTestOptions(t, snapshot+"\n"+paperTestBook)
	dsn := paperTestPostgres(t, &opt)
	var stdout, stderr bytes.Buffer
	if err := runPaper(context.Background(), opt, dsn, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	var wire struct {
		KnownFinal bool            `json:"known_final"`
		State      json.RawMessage `json:"durable_state"`
		Account    struct {
			Balances []struct {
				Currency string `json:"currency"`
				Total    string `json:"total"`
			} `json:"balances"`
		} `json:"account"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &wire); err != nil {
		t.Fatal(err)
	}
	state, err := rt.DecodeSnapshot(wire.State)
	if err != nil || !wire.KnownFinal || len(state.Fills) != 1 || state.ReferenceMid.Scale() <= d.MaxInputScale || state.Fills[0].Fee.Scale() <= d.MaxInputScale {
		t.Fatalf("trusted internal precision lost: err=%v knownFinal=%v fills=%d referenceMid=%s", err, wire.KnownFinal, len(state.Fills), state.ReferenceMid)
	}
	wantQuote := d.MustDecimal("100").Sub(state.Fills[0].Quantity.Mul(state.Fills[0].Price)).Sub(state.Fills[0].Fee)
	for _, balance := range wire.Account.Balances {
		if balance.Currency == "USD" && balance.Total != wantQuote.String() {
			t.Fatalf("exact long balance was truncated: got=%s want=%s", balance.Total, wantQuote)
		}
	}
}
