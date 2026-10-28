package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	d "github.com/liquiditylab/liquiditylab/internal/domain"
	ar "github.com/liquiditylab/liquiditylab/internal/runtime"
)

func testConfig() config {
	return config{envelopeConfig: defaultEnvelope(), Development: true, Baseline: "scheduled", DSNEnv: "LIQUIDITYLAB_TEST_POSTGRES_DSN", Schema: "ll_benchmark_test", Output: "report.json", Storage: "test development database", Seed: 42, Warmup: 0, Duration: 100 * time.Millisecond, MarketRate: 50, ReadRate: 50, TradePercent: 50, MaxLag: time.Second, Cadence: 20 * time.Millisecond, Queue: 2, InFlight: 2, Pool: 2, Metrics: true}
}

func TestConfigRejectsUnboundedOrImplicitWorkload(t *testing.T) {
	good := testConfig()
	if e := good.validate(); e != nil {
		t.Fatal(e)
	}
	for name, change := range map[string]func(*config){"development": func(c *config) { c.Development = false }, "schema": func(c *config) { c.Schema = "" }, "output": func(c *config) { c.Output = "" }, "storage": func(c *config) { c.Storage = "" }, "duration": func(c *config) { c.Duration = time.Hour }, "nan": func(c *config) { c.MarketRate = math.NaN() }, "inf": func(c *config) { c.ReadRate = math.Inf(1) }, "negative": func(c *config) { c.ReadRate = -1 }, "count": func(c *config) { c.Duration = 4 * time.Minute; c.MarketRate = 500 }, "inflight": func(c *config) { c.InFlight = 100000 }, "lag": func(c *config) { c.MaxLag = 0 }} {
		t.Run(name, func(t *testing.T) {
			c := good
			change(&c)
			if c.validate() == nil {
				t.Fatal("invalid configuration accepted")
			}
		})
	}
}

func TestScheduleIsSeedDeterministicAndIndependentOfResponses(t *testing.T) {
	c := testConfig()
	a := schedule(c, time.Second, "measurement")
	b := schedule(c, time.Second, "measurement")
	if !reflect.DeepEqual(a, b) || len(a) != 100 {
		t.Fatalf("schedule differs/count=%d", len(a))
	}
	for i, in := range a {
		if i > 0 && in.IntendedNS < a[i-1].IntendedNS {
			t.Fatal("schedule unordered")
		}
	}
	c.Seed++
	if reflect.DeepEqual(a, schedule(c, time.Second, "measurement")) {
		t.Fatal("seed had no effect")
	}
}

type fakeActor struct {
	mu            sync.Mutex
	delay         time.Duration
	err           error
	receipts      []uint64
	inflight, max int
}

func (f *fakeActor) MarketFromSession(ctx context.Context, e d.MarketEvent, _, _ string) error {
	f.mu.Lock()
	f.inflight++
	if f.inflight > f.max {
		f.max = f.inflight
	}
	f.receipts = append(f.receipts, e.ReceiptIndex)
	f.mu.Unlock()
	time.Sleep(f.delay)
	f.mu.Lock()
	f.inflight--
	f.mu.Unlock()
	return f.err
}
func (f *fakeActor) View(context.Context) (ar.View, error) {
	time.Sleep(f.delay)
	return ar.View{}, f.err
}

func TestOpenLoopOverloadRetainsOfferedCountsAndSourceOrder(t *testing.T) {
	c := testConfig()
	c.MarketRate = 500
	c.ReadRate = 0
	c.Queue = 1
	f := &fakeActor{delay: 10 * time.Millisecond}
	p := runPhase(context.Background(), c, "measurement", 100*time.Millisecond, f)
	if p.Valid || p.Market.Offered != 50 || p.Market.Rejected == 0 || p.Market.Completed != p.Market.Admitted || p.Market.Completed+p.Market.Rejected != 50 {
		t.Fatalf("overload accounting: %+v", p.Market)
	}
	if f.max != 1 {
		t.Fatal("source calls ran concurrently")
	}
	for i := 1; i < len(f.receipts); i++ {
		if f.receipts[i] <= f.receipts[i-1] {
			t.Fatal("source order changed")
		}
	}
	for _, s := range p.Samples {
		if s.AdmittedNS == nil && (s.CallNS != nil || s.CompletedNS != nil) {
			t.Fatal("rejected work claimed a call/completion")
		}
	}
	encoded, _ := json.Marshal(p.Manifest)
	hash := sha256.Sum256(encoded)
	if p.ManifestSHA256 != hex.EncodeToString(hash[:]) {
		t.Fatal("manifest hash mismatch")
	}
}

func TestUncertainReturnsNeverCountAsCompleted(t *testing.T) {
	c := testConfig()
	c.MarketRate = 10
	c.ReadRate = 0
	f := &fakeActor{err: ar.ErrOutcomeUnknown}
	p := runPhase(context.Background(), c, "measurement", 100*time.Millisecond, f)
	if p.Valid || p.Market.Completed != 0 || p.Market.Unresolved != 1 || p.Latencies["service_call_success"].Count != 0 {
		t.Fatalf("uncertain accounting: %+v", p)
	}
	if classify(errors.New("database failure")) != "failed" || classify(ar.ErrMailboxFull) != "service_rejected" {
		t.Fatal("failure classification")
	}
}

func TestFreshReportNeverOverwrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.json")
	f, e := freshFile(path)
	if e != nil {
		t.Fatal(e)
	}
	_, _ = f.WriteString("keep")
	_ = f.Close()
	if _, e = freshFile(path); !errors.Is(e, os.ErrExist) {
		t.Fatalf("existing report accepted: %v", e)
	}
	b, _ := os.ReadFile(path)
	if string(b) != "keep" {
		t.Fatal("report overwritten")
	}
}

func TestPostgresRunnerExactFinalStateAndFreshAccounts(t *testing.T) {
	if os.Getenv("LIQUIDITYLAB_TEST_POSTGRES_DSN") == "" {
		t.Skip("set LIQUIDITYLAB_TEST_POSTGRES_DSN for short durable runner check")
	}
	c := testConfig()
	c.Schema = "ll_runtime_bench_test_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	c.Output = filepath.Join(t.TempDir(), "report.json")
	c.Warmup = 100 * time.Millisecond
	c.Duration = 300 * time.Millisecond
	c.MarketRate = 10
	c.ReadRate = 5
	c.Queue = 64
	c.Metrics = true
	if e := execute(c); e != nil {
		b, _ := os.ReadFile(c.Output)
		t.Fatalf("run: %v; report %s", e, b)
	}
	b, e := os.ReadFile(c.Output)
	if e != nil {
		t.Fatal(e)
	}
	var r report
	if e = json.Unmarshal(b, &r); e != nil {
		t.Fatal(e)
	}
	if !r.Valid || r.Measurement.Market.Offered != 3 || r.SourceDebt != 0 || r.Final.State.Parent.Status != "cancelled" || len(r.Final.Account.Reservations) > 0 || len(r.Final.Account.Intents) > 0 {
		t.Fatalf("final workload state invalid: status=%s valid=%v", r.Status, r.Valid)
	}
	base, quote := d.MustDecimal("10"), d.MustDecimal("100000")
	for _, fill := range r.Final.State.Fills {
		if fill.Side != "buy" || fill.FeeCurrency != "quote" {
			t.Fatalf("unexpected fill %+v", fill)
		}
		base = base.Add(fill.Quantity)
		quote = quote.Sub(fill.Quantity.Mul(fill.Price)).Sub(fill.Fee)
	}
	for _, balance := range r.Final.Account.Balances {
		want := quote
		if balance.Currency == "BTC" {
			want = base
		}
		if !balance.Total.Equal(want) || !balance.Free.Equal(want) || !balance.Locked.IsZero() {
			t.Fatalf("exact balance mismatch: %+v want %s", balance, want.String())
		}
	}
	if strings.Contains(string(b), os.Getenv(c.DSNEnv)) {
		t.Fatal("DSN leaked into report")
	}
	first := r.Account.AccountID
	c.Output = filepath.Join(t.TempDir(), "metrics-off.json")
	c.Metrics = false
	if e = execute(c); e != nil {
		t.Fatal(e)
	}
	b, _ = os.ReadFile(c.Output)
	if e = json.Unmarshal(b, &r); e != nil {
		t.Fatal(e)
	}
	if first == r.Account.AccountID {
		t.Fatal("benchmark account reused")
	}
	if len(r.AfterTelemetry.Counters) != 0 {
		t.Fatal("disabled telemetry recorded counters")
	}
	// Deliberately retain both accounts and the schema for audit; no reset/drop.
}
