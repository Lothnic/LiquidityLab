// Command runtime exercises the durable paper actor against an explicitly
// selected development PostgreSQL database. It never resets accounts or schemas.
package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	d "github.com/liquiditylab/liquiditylab/internal/domain"
	x "github.com/liquiditylab/liquiditylab/internal/execution"
	ar "github.com/liquiditylab/liquiditylab/internal/runtime"
	pg "github.com/liquiditylab/liquiditylab/internal/store/postgres"
	"github.com/liquiditylab/liquiditylab/internal/telemetry"
)

const reportVersion = "durable-runtime-benchmark-v2"
const maxInputs = 10000

type config struct {
	envelopeConfig `json:"envelope"`
	Development    bool          `json:"development_database_acknowledged"`
	DSNEnv         string        `json:"-"`
	Schema         string        `json:"schema"`
	Output         string        `json:"-"`
	Storage        string        `json:"storage_description"`
	Baseline       string        `json:"baseline"`
	Seed           uint64        `json:"seed"`
	Warmup         time.Duration `json:"warmup_ns"`
	Duration       time.Duration `json:"measurement_ns"`
	MarketRate     float64       `json:"market_events_per_second"`
	ReadRate       float64       `json:"views_per_second"`
	TradePercent   int           `json:"trade_percent"`
	MaxLag         time.Duration `json:"maximum_generator_lag_ns"`
	Cadence        time.Duration `json:"decision_cadence_ns"`
	Queue          int           `json:"ordered_source_queue_capacity"`
	InFlight       int           `json:"view_in_flight_capacity"`
	Pool           int           `json:"pool_max_connections"`
	Metrics        bool          `json:"metrics_enabled"`
}

func flags() config {
	c := config{envelopeConfig: defaultEnvelope()}
	flag.BoolVar(&c.Development, "development", false, "acknowledge that the selected database is for development benchmarking")
	flag.StringVar(&c.DSNEnv, "dsn-env", "LIQUIDITYLAB_BENCH_POSTGRES_DSN", "environment variable containing explicit development DSN (never written to report)")
	flag.StringVar(&c.Schema, "schema", "", "explicit development schema; migrations only, no reset")
	flag.StringVar(&c.Output, "out", "", "new report pathname; existing files are refused")
	flag.StringVar(&c.Storage, "storage-description", "", "describe database storage and isolation; no secrets")
	flag.StringVar(&c.Baseline, "baseline", "scheduled", "Go baseline: scheduled or liquidity-aware")
	flag.Uint64Var(&c.Seed, "seed", 42, "deterministic source seed")
	flag.DurationVar(&c.Warmup, "warmup", 2*time.Second, "warmup interval (0..1m)")
	flag.DurationVar(&c.Duration, "duration", 10*time.Second, "measurement interval (100ms..4m)")
	flag.Float64Var(&c.MarketRate, "market-rate", 10, "open-loop offered market events/sec (0..500)")
	flag.Float64Var(&c.ReadRate, "view-rate", 2, "open-loop offered actor views/sec (0..500)")
	flag.IntVar(&c.TradePercent, "trade-percent", 50, "deterministic trade percentage (0..100); other inputs are book updates")
	flag.DurationVar(&c.MaxLag, "max-generator-lag", 250*time.Millisecond, "reject source inputs dispatched beyond this lag (1ms..5s)")
	flag.DurationVar(&c.Cadence, "cadence", 100*time.Millisecond, "automatic baseline decision interval (10ms..10s)")
	flag.IntVar(&c.Queue, "source-queue", 64, "ordered source queue slots (1..4096)")
	flag.IntVar(&c.InFlight, "view-in-flight", 4, "concurrent actor view calls (1..64)")
	flag.IntVar(&c.Pool, "pool", 4, "shared store pool connections (1..64)")
	flag.BoolVar(&c.Metrics, "metrics", true, "record internal actor/store telemetry")
	envelopeFlags(&c)
	flag.Parse()
	return c
}

func (c config) validate() error {
	if e := c.validateEnvelope(); e != nil {
		return e
	}
	if e := x.ValidateBaselinePolicy(c.Baseline); e != nil {
		return e
	}
	if !c.Development || !regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`).MatchString(c.Schema) || c.Schema == "public" || c.Schema == "information_schema" || strings.HasPrefix(c.Schema, "pg_") || c.Output == "" || c.Storage == "" || len(c.Storage) > 1024 {
		return errors.New("require --development, explicit --schema, new --out, and --storage-description (<=1024 bytes)")
	}
	if c.Warmup < 0 || c.Warmup > time.Minute || c.Duration < 100*time.Millisecond || c.Duration > 4*time.Minute || c.Warmup+c.Duration > 5*time.Minute || c.MaxLag < time.Millisecond || c.MaxLag > 5*time.Second || c.Cadence < 10*time.Millisecond || c.Cadence > 10*time.Second {
		return errors.New("duration, warmup, or cadence outside bounded limits")
	}
	if c.Queue < 1 || c.Queue > 4096 || c.InFlight < 1 || c.InFlight > 64 || c.Pool < 1 || c.Pool > 64 || c.TradePercent < 0 || c.TradePercent > 100 {
		return errors.New("queue, concurrency, pool, or mix outside bounded limits")
	}
	for _, rate := range []float64{c.MarketRate, c.ReadRate} {
		if math.IsNaN(rate) || math.IsInf(rate, 0) || rate < 0 || rate > 500 {
			return errors.New("rates must be finite and within 0..500")
		}
	}
	if float64(c.Warmup+c.Duration)/float64(time.Second)*(c.MarketRate+c.ReadRate) > maxInputs-4 {
		return errors.New("offered input count exceeds 10000 bound")
	}
	if c.MarketRate+c.ReadRate == 0 {
		return errors.New("at least one offered rate must be positive")
	}
	return nil
}

type input struct {
	ReadRoute  string        `json:"read_route,omitempty"`
	Kind       string        `json:"kind"`
	IntendedNS int64         `json:"intended_ns"`
	Event      d.MarketEvent `json:"event"`
	ID         string        `json:"id"`
}
type sample struct {
	ServiceBoundary string `json:"service_boundary"`
	ReadRoute       string `json:"read_route,omitempty"`
	HTTPStatus      int    `json:"http_status,omitempty"`
	ResponseBytes   int64  `json:"response_bytes,omitempty"`
	Kind            string `json:"kind"`
	ID              string `json:"id"`
	IntendedNS      int64  `json:"intended_ns"`
	AttemptNS       int64  `json:"generator_attempt_ns"`
	AdmittedNS      *int64 `json:"runner_admitted_ns"`
	CallNS          *int64 `json:"service_call_started_ns"`
	CompletedNS     *int64 `json:"service_call_returned_ns"`
	Outcome         string `json:"outcome"`
}
type summary struct {
	Offered    uint64 `json:"offered"`
	Admitted   uint64 `json:"runner_admitted"`
	Completed  uint64 `json:"completed"`
	Rejected   uint64 `json:"rejected"`
	Failed     uint64 `json:"failed"`
	Unresolved uint64 `json:"unresolved"`
}
type phase struct {
	ReadBoundary      string             `json:"read_service_boundary"`
	ReadResponseBytes int64              `json:"read_response_bytes"`
	RouteCounts       map[string]summary `json:"http_route_counts,omitempty"`
	Name              string             `json:"name"`
	Started           time.Time          `json:"started_utc"`
	ElapsedNS         int64              `json:"elapsed_including_drain_ns"`
	Valid             bool               `json:"source_delivery_valid"`
	Market            summary            `json:"market"`
	Views             summary            `json:"views"`
	ManifestSHA256    string             `json:"input_manifest_sha256"`
	InputBytes        int                `json:"source_event_json_bytes"`
	Manifest          []input            `json:"input_manifest"`
	MarketLatencies   map[string]latency `json:"market_latencies"`
	ViewLatencies     map[string]latency `json:"view_latencies"`
	Latencies         map[string]latency `json:"latencies"`
	Samples           []sample           `json:"samples"`
}

// schedule uses integer offsets from rate and index, never prior completion.
// splitmix64 gives deterministic input content without a dependency on rand APIs.
func schedule(c config, duration time.Duration, prefix string) []input {
	items := make([]input, 0)
	state := c.Seed
	next := func() uint64 {
		state += 0x9e3779b97f4a7c15
		z := state
		z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
		z = (z ^ (z >> 27)) * 0x94d049bb133111eb
		return z ^ (z >> 31)
	}
	for _, kind := range []string{"market", "view"} {
		rate := c.MarketRate
		if kind == "view" {
			rate = c.ReadRate
		}
		if rate == 0 {
			continue
		}
		for i := 0; ; i++ {
			ns := int64(float64(i) * float64(time.Second) / rate)
			if ns >= int64(duration) {
				break
			}
			id := fmt.Sprintf("%s-%s-%d", prefix, kind, i)
			v := input{Kind: kind, IntendedNS: ns, ID: id}
			if kind == "view" {
				v.ReadRoute = c.readRoute(i)
			}
			if kind == "market" {
				r := next()
				quantity := d.MustDecimal([]string{"0.01", "0.02", "0.03", "0.04"}[r%4])
				v.Event = d.MarketEvent{Time: ns, Kind: "book", Symbol: "BTC/USD", ReceiptIndex: uint64(i + 1), Bids: []d.Level{{Price: d.MustDecimal("99"), Quantity: d.MustDecimal("1")}}, Asks: []d.Level{{Price: d.MustDecimal("101"), Quantity: d.MustDecimal("1")}}}
				if int((r>>8)%100) < c.TradePercent {
					v.Event = d.MarketEvent{Time: ns, Kind: "trade", Symbol: "BTC/USD", ReceiptIndex: uint64(i + 1), Price: d.MustDecimal("99"), Quantity: quantity, Side: "sell", TradeID: id}
				}
			}
			items = append(items, v)
		}
	}
	// Stable merge keeps deterministic market/view ties, including time zero.
	sortInputs(items)
	return items
}

type phaseActor interface {
	MarketFromSession(context.Context, d.MarketEvent, string, string) error
	View(context.Context) (ar.View, error)
}

func runPhase(ctx context.Context, c config, name string, duration time.Duration, a phaseActor, readers ...readFunc) phase {
	read := func(ctx context.Context, in input) (readResult, error) {
		_, err := a.View(ctx)
		return readResult{}, err
	}
	if len(readers) > 0 && readers[0] != nil {
		read = readers[0]
	}
	inputs := schedule(c, duration, name)
	b, _ := json.Marshal(inputs)
	hash := sha256.Sum256(b)
	p := phase{Name: name, Started: time.Now().UTC(), Valid: true, ManifestSHA256: hex.EncodeToString(hash[:]), Manifest: inputs, Samples: make([]sample, len(inputs))}
	p.ReadBoundary = "actor.View completion"
	if c.ReadMode == "http" {
		p.ReadBoundary = "HTTP response fully consumed and JSON validated; HTTP polling, no React rendering"
	}
	start := time.Now()
	queue := make(chan int, c.Queue)
	slots := make(chan struct{}, c.InFlight)
	var worker sync.WaitGroup
	worker.Add(1)
	go func() {
		defer worker.Done()
		for idx := range queue {
			s := &p.Samples[idx]
			t := time.Since(start).Nanoseconds()
			s.CallNS = &t
			callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := a.MarketFromSession(callCtx, inputs[idx].Event, inputs[idx].ID, name)
			cancel()
			end := time.Since(start).Nanoseconds()
			s.CompletedNS = &end
			s.Outcome = classify(err)
		}
	}()
	var reads sync.WaitGroup
	for idx, in := range inputs {
		wait := time.Duration(in.IntendedNS) - time.Since(start)
		if wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-timer.C:
			case <-ctx.Done():
			}
			timer.Stop()
		}
		s := &p.Samples[idx]
		*s = sample{Kind: in.Kind, ID: in.ID, ReadRoute: in.ReadRoute, IntendedNS: in.IntendedNS, AttemptNS: time.Since(start).Nanoseconds(), Outcome: "runner_rejected"}
		s.ServiceBoundary = "actor.MarketFromSession"
		if in.Kind == "view" {
			s.ServiceBoundary = "actor.View"
			if c.ReadMode == "http" {
				s.ServiceBoundary = "HTTP full response and JSON validation"
			}
		}
		if in.Kind == "market" {
			encoded, _ := json.Marshal(in.Event)
			p.InputBytes += len(encoded)
		}
		if s.AttemptNS-in.IntendedNS > int64(c.MaxLag) || s.AttemptNS >= int64(duration) {
			s.Outcome = "generator_late"
			continue
		}
		if ctx.Err() != nil {
			s.Outcome = "not_attempted_cancelled"
			continue
		}
		admitted := time.Since(start).Nanoseconds()
		if in.Kind == "market" {
			// Assign before publishing the index, so the consumer sees complete metadata.
			s.AdmittedNS = &admitted
			select {
			case queue <- idx:
			default:
				s.AdmittedNS = nil
			}
		} else {
			select {
			case slots <- struct{}{}:
				s.AdmittedNS = &admitted
				reads.Add(1)
				go func(s *sample, in input) {
					defer reads.Done()
					defer func() { <-slots }()
					t := time.Since(start).Nanoseconds()
					s.CallNS = &t
					callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
					out, err := read(callCtx, in)
					s.HTTPStatus = out.Status
					s.ResponseBytes = out.Bytes
					cancel()
					end := time.Since(start).Nanoseconds()
					s.CompletedNS = &end
					s.Outcome = classify(err)
				}(s, in)
			default:
			}
		}
	}
	// The configured interval includes the time after the final scheduled arrival.
	if remaining := duration - time.Since(start); remaining > 0 {
		timer := time.NewTimer(remaining)
		select {
		case <-timer.C:
		case <-ctx.Done():
		}
		timer.Stop()
	}
	close(queue)
	worker.Wait()
	reads.Wait()
	p.ElapsedNS = time.Since(start).Nanoseconds()
	for _, s := range p.Samples {
		target := &p.Market
		if s.Kind == "view" {
			target = &p.Views
		}
		target.Offered++
		if s.AdmittedNS != nil {
			target.Admitted++
		}
		switch s.Outcome {
		case "completed":
			target.Completed++
		case "runner_rejected", "service_rejected", "not_attempted_cancelled", "generator_late":
			target.Rejected++
		case "outcome_unknown":
			target.Unresolved++
		default:
			target.Failed++
		}
		if s.Kind == "market" && s.Outcome != "completed" {
			p.Valid = false
		}
	}
	p.RouteCounts = map[string]summary{}
	for _, s := range p.Samples {
		p.ReadResponseBytes += s.ResponseBytes
		if s.ReadRoute != "" {
			counts := p.RouteCounts[s.ReadRoute]
			counts.Offered++
			if s.AdmittedNS != nil {
				counts.Admitted++
			}
			switch s.Outcome {
			case "completed":
				counts.Completed++
			case "runner_rejected", "service_rejected", "generator_late", "not_attempted_cancelled":
				counts.Rejected++
			case "outcome_unknown":
				counts.Unresolved++
			default:
				counts.Failed++
			}
			p.RouteCounts[s.ReadRoute] = counts
		}
	}
	p.Latencies = latencies(p.Samples)
	var marketSamples, viewSamples []sample
	for _, s := range p.Samples {
		if s.Kind == "market" {
			marketSamples = append(marketSamples, s)
		} else {
			viewSamples = append(viewSamples, s)
		}
	}
	p.MarketLatencies = latencies(marketSamples)
	p.ViewLatencies = latencies(viewSamples)
	return p
}

func classify(err error) string {
	if err == nil {
		return "completed"
	}
	if errors.Is(err, ar.ErrOutcomeUnknown) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) || errors.Is(err, ar.ErrClosed) {
		return "outcome_unknown"
	}
	if errors.Is(err, ar.ErrMailboxFull) || errors.Is(err, errReadRejected) {
		return "service_rejected"
	}
	return "failed"
}

type resources struct {
	Wall          time.Time `json:"wall_utc"`
	UserCPUUS     int64     `json:"user_cpu_us"`
	SystemCPUUS   int64     `json:"system_cpu_us"`
	TotalAlloc    uint64    `json:"total_alloc_bytes"`
	Mallocs       uint64    `json:"mallocs"`
	NumGC         uint32    `json:"gc_cycles"`
	GCPauseNS     uint64    `json:"gc_pause_ns"`
	GCCPUFraction float64   `json:"gc_cpu_fraction_cumulative"`
	HeapAlloc     uint64    `json:"heap_alloc_bytes"`
	RSS           int64     `json:"rss_bytes"`
	PeakRSS       int64     `json:"peak_rss_bytes"`
	Goroutines    int       `json:"goroutines"`
}

func resourceSnapshot() resources {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	var u syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &u)
	r := resources{Wall: time.Now().UTC(), UserCPUUS: u.Utime.Sec*1000000 + u.Utime.Usec, SystemCPUUS: u.Stime.Sec*1000000 + u.Stime.Usec, TotalAlloc: m.TotalAlloc, Mallocs: m.Mallocs, NumGC: m.NumGC, GCPauseNS: m.PauseTotalNs, GCCPUFraction: m.GCCPUFraction, HeapAlloc: m.HeapAlloc, PeakRSS: u.Maxrss * 1024, Goroutines: runtime.NumGoroutine()}
	if b, e := os.ReadFile("/proc/self/statm"); e == nil {
		f := strings.Fields(string(b))
		if len(f) > 1 {
			n, _ := strconv.ParseInt(f[1], 10, 64)
			r.RSS = n * int64(os.Getpagesize())
		}
	}
	return r
}

func freshFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
}

func main() {
	c := flags()
	if err := execute(c); err != nil {
		fmt.Fprintln(os.Stderr, "runtime benchmark:", err)
		os.Exit(1)
	}
}

func execute(c config) (resultErr error) {
	if err := c.validate(); err != nil {
		return err
	}
	dsn := os.Getenv(c.DSNEnv)
	if dsn == "" {
		return errors.New("explicit development DSN environment variable is empty")
	}
	f, err := freshFile(c.Output)
	if err != nil {
		return fmt.Errorf("create fresh report: %w", err)
	}
	defer f.Close()
	// Reserve the output before database mutation. Even setup failures leave a
	// sanitized, immutable failure report at that pathname.
	r := report{Version: reportVersion, Config: c, Started: time.Now().UTC(), Valid: false, Limitations: limitations, Host: hostInfo()}
	r.Validation = map[string]string{"financial": "exact-cashflow-terminal-v2", "http": "not-requested"}
	if c.ReadMode == "http" {
		r.Validation["http"] = "scoped-account-plan-v1"
	}
	defer func() {
		if r.Status == "" {
			r.Status = "setup_failed"
		}
		if resultErr != nil {
			r.Failure = resultErr.Error()
		}
		data, e := json.MarshalIndent(r, "", "  ")
		if e != nil {
			resultErr = fmt.Errorf("encode report: %w", e)
			return
		}
		if len(data) > 32<<20 {
			resultErr = errors.New("report exceeded 32 MiB limit")
			return
		}
		if _, e = f.Write(append(data, '\n')); e != nil {
			resultErr = fmt.Errorf("write report: %w", e)
			return
		}
		if e = f.Sync(); e != nil {
			resultErr = fmt.Errorf("sync report: %w", e)
		}
	}()
	prof, err := reserveProfile(c)
	if err != nil {
		return err
	}
	r.Profile = prof.report
	defer func() { prof.Close(); r.Profile = prof.report }()
	ctx, cancel := context.WithTimeout(context.Background(), c.Warmup+c.Duration+2*time.Minute)
	defer cancel()
	var rec *telemetry.Recorder
	if c.Metrics {
		rec = telemetry.New()
	}
	r.Policy = policyReport{Kind: "none"}
	if c.PolicyTarget != "" || c.MockPolicyDelay >= 0 {
		r.Policy = policyReport{Kind: "external_local_worker", SHA256: c.PolicySHA256, Profile: c.PolicyProfile, Fallback: c.PolicyFallback, TimeoutNS: int64(c.PolicyTimeout), Quiescence: "not_started", Caveat: "worker verification not completed; no account created"}
		if c.MockPolicyDelay >= 0 {
			r.Policy.Kind = "controlled_local_grpc"
			r.Policy.SHA256 = mockPolicySHA256()
		}
	}
	policy, err := startPolicy(ctx, c, rec)
	if err != nil {
		return err
	}
	r.Policy = policy.report
	defer func() {
		if e := policy.Close(); e != nil {
			r.Valid = false
			r.Policy.Quiescence = "unresolved"
			if resultErr == nil {
				resultErr = e
			}
		}
		r.Policy.Quiescent = policy.worker.Snapshot()
	}()
	storeTag := ""
	if c.DBActivityInterval != 0 {
		var tagBytes [12]byte
		if _, err := rand.Read(tagBytes[:]); err != nil {
			return errors.New("create database activity scope failed")
		}
		storeTag = "llbench-" + hex.EncodeToString(tagBytes[:])
		r.Validation["postgres_activity"] = dbActivityContract
		r.DBActivity = &dbActivityReport{Contract: dbActivityContract, Status: "not_started"}
	}
	store, err := pg.OpenWithOptions(ctx, dsn, c.Schema, pg.Options{Telemetry: rec, MaxConnections: int32(c.Pool), ApplicationName: storeTag})
	if err != nil {
		return errors.New("open development database failed (details withheld to protect DSN)")
	}
	defer store.Close()
	if err = store.Migrate(ctx); err != nil {
		return errors.New("development schema migration failed")
	}
	observer, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return errors.New("open benchmark database observer failed")
	}
	defer observer.Close(context.Background())
	r.Database, err = databaseInfo(ctx, observer, c.Schema)
	if err != nil {
		return errors.New("read database configuration failed")
	}
	dbObserver, err := prepareDBActivity(ctx, observer, storeTag, c.DBActivityInterval, c.Pool)
	if err != nil {
		r.DBActivity.Status = "unavailable"
		return err
	}
	var raw [12]byte
	if _, err = rand.Read(raw[:]); err != nil {
		return err
	}
	id := "bench-" + hex.EncodeToString(raw[:])
	key := pg.Key{TenantID: "runtime-benchmark", AccountID: id}
	r.Account = key
	balances := []pg.Balance{{Currency: "BTC", Total: d.MustDecimal("10"), Free: d.MustDecimal("10")}, {Currency: "USD", Total: d.MustDecimal("100000"), Free: d.MustDecimal("100000")}}
	r.InitialBalances = balances
	if _, err = store.CreateAccount(ctx, key, balances, json.RawMessage(`{}`)); err != nil {
		return errors.New("create unique benchmark account failed")
	}
	cfg := ar.DefaultConfig(store, key)
	cfg.Telemetry = rec
	cfg.Policy = policy.Config(c)
	cfg.AutoExecute = true
	cfg.DataSource = "synthetic"
	cfg.Execution.DecisionInterval = int64(c.Cadence)
	cfg.OwnerID = id
	cfg.Lease = 5 * time.Minute
	cfg.MailboxCapacity = 64
	cfg.ControlCapacity = 16
	cfg.FactCapacity = 64
	cfg.Baseline = c.Baseline
	r.ActorConfig = cfgInfo(cfg)
	a, err := ar.New(ctx, cfg)
	if err != nil {
		return errors.New("bootstrap benchmark actor failed")
	}
	defer func() {
		closeCtx, cc := context.WithTimeout(context.Background(), 15*time.Second)
		defer cc()
		_ = a.Close(closeCtx)
	}()
	if err = a.Market(ctx, d.MarketEvent{Kind: "snapshot", Symbol: "BTC/USD", Bids: []d.Level{{Price: d.MustDecimal("99"), Quantity: d.MustDecimal("1")}}, Asks: []d.Level{{Price: d.MustDecimal("101"), Quantity: d.MustDecimal("1")}}}, "initial-snapshot"); err != nil {
		return errors.New("initial synthetic snapshot failed")
	}
	if err = a.RecoverPaper(ctx); err != nil {
		return errors.New("initial paper recovery/readiness failed")
	}

	plan := d.Plan{Side: "buy", Quantity: d.MustDecimal("10"), LimitPrice: d.MustDecimal("102"), Deadline: int64(c.Warmup + c.Duration + 30*time.Second), MaxChildQuantity: d.MustDecimal("0.01"), QuoteBudget: d.MustDecimal("100000"), BaseBalance: d.MustDecimal("10"), Symbol: "BTC/USD"}
	r.Plan = plan
	if err = command(ctx, a, ar.Command{ID: "benchmark-create", Kind: "create", PlanID: "benchmark-plan", Plan: &plan, Policy: c.Baseline}); err != nil {
		return errors.New("create baseline plan failed")
	}
	if err = command(ctx, a, ar.Command{ID: "benchmark-start", Kind: "start"}); err != nil {
		return errors.New("start baseline plan failed")
	}
	var poller *httpPoller
	var read readFunc
	if c.ReadMode == "http" {
		poller, err = startHTTPPoller(c, key, a, store, store)
		if err != nil {
			return err
		}
		defer func() {
			if e := poller.Close(); e != nil {
				r.Valid = false
				if resultErr == nil {
					resultErr = errors.New("HTTP polling shutdown incomplete")
				}
			}
		}()
		read = poller.Read
	}
	if c.Warmup > 0 {
		r.Warmup = runPhase(ctx, c, "warmup", c.Warmup, a, read)
	} else {
		r.Warmup = phase{Name: "warmup", Valid: true}
	}
	r.Policy.Before = policy.worker.Snapshot()
	r.BeforeTelemetry = rec.Snapshot()
	r.BeforePool = store.PoolStats()
	r.BeforeResources = resourceSnapshot()
	r.BeforeDB = dbSnapshot(ctx, observer, c.Schema)
	if err = prof.StartMeasurement(); err != nil {
		return err
	}
	dbObserver.Start(ctx)
	defer dbObserver.Stop()
	r.Measurement = runPhase(ctx, c, "measurement", c.Duration, a, read)
	prof.StopSampling()
	r.DBActivity = dbObserver.Stop()
	r.AfterResources = resourceSnapshot()
	r.AfterPool = store.PoolStats()
	r.AfterTelemetry = rec.Snapshot()
	r.AfterDB = dbSnapshot(ctx, observer, c.Schema)
	r.Policy.After = policy.worker.Snapshot()
	if err = prof.FinishMeasurement(); err != nil {
		return err
	}
	r.Profile = prof.report
	r.ResourceDelta = resourceDelta(r.BeforeResources, r.AfterResources)
	var wal string
	if r.BeforeDB.WALLSN != "" && r.AfterDB.WALLSN != "" {
		if e := observer.QueryRow(ctx, "SELECT pg_wal_lsn_diff($1::pg_lsn,$2::pg_lsn)::text", r.AfterDB.WALLSN, r.BeforeDB.WALLSN).Scan(&wal); e == nil {
			r.ClusterWALBytes = wal
		}
	}
	r.Valid = r.Warmup.Valid && r.Measurement.Valid && ctx.Err() == nil && (r.DBActivity == nil || r.DBActivity.Complete)
	// A lost source fact makes this run invalid. Stop ingress permanently, then
	// cancel the bounded synthetic parent so it cannot continue after reporting.
	cleanupStart := time.Now()
	if err = command(ctx, a, ar.Command{ID: "benchmark-stop", Kind: "cancel"}); err != nil {
		r.Cleanup = "cancel_unresolved"
		r.Valid = false
	} else {
		r.Cleanup = "cancel_acknowledged"
	}
	r.ControlCleanupNS = time.Since(cleanupStart).Nanoseconds()
	if poller != nil {
		if e := poller.Close(); e != nil {
			r.Valid = false
		}
	}
	closeCtx, closeCancel := context.WithTimeout(context.Background(), 15*time.Second)
	err = a.Close(closeCtx)
	closeCancel()
	if err != nil {
		r.Valid = false
		r.Status = "shutdown_unresolved"
		return errors.New("actor shutdown did not become quiescent")
	}
	if err = policy.Close(); err != nil {
		r.Valid = false
		r.Policy.Quiescence = "unresolved"
	} else {
		r.Policy.Quiescence = "client_calls_drained"
	}
	r.Policy.Quiescent = policy.worker.Snapshot()
	account, err := store.GetAccount(ctx, key)
	if err != nil {
		r.Valid = false
		r.Status = "final_view_unresolved"
		return errors.New("final durable account unavailable")
	}
	state, err := ar.DecodeSnapshot(account.Snapshot)
	if err != nil {
		r.Valid = false
		r.Status = "final_view_unresolved"
		return errors.New("final durable state could not be decoded")
	}
	v := ar.View{Account: account, State: state}
	r.Final = v
	r.FinancialAudit = validateFinancial(ctx, observer, c.Schema, r.InitialBalances, v)
	if !r.FinancialAudit.Valid {
		r.Valid = false
	}
	r.QuiescentTelemetry = rec.Snapshot()
	r.QuiescentPool = store.PoolStats()
	for _, g := range r.QuiescentTelemetry.Gauges {
		if g.Value != 0 || g.Overflow {
			r.Valid = false
		}
	}

	r.SourceDebt = v.Account.ObservedSequence - v.Account.ProcessedSequence
	if v.State.Parent == nil || v.State.Parent.Status != "cancelled" && v.State.Parent.Status != "completed" && v.State.Parent.Status != "expired" || v.State.RecoveryRequired || len(v.State.UnresolvedFacts) > 0 || r.SourceDebt != 0 || len(v.Account.Reservations) > 0 || len(v.Account.Intents) > 0 || r.Measurement.Market.Unresolved+r.Measurement.Views.Unresolved+r.Warmup.Market.Unresolved+r.Warmup.Views.Unresolved > 0 || r.Measurement.Views.Failed+r.Warmup.Views.Failed > 0 || v.State.PolicyPending != nil {
		r.Valid = false
	}
	r.Status = "complete"
	if !r.Valid {
		r.Status = "invalid"
		return errors.New("workload invalid; inspect report source delivery, unresolved work, optional diagnostics, and final state")
	}
	return nil
}

func command(ctx context.Context, a *ar.Actor, c ar.Command) error {
	for attempt := 0; attempt < 32; attempt++ {
		v, e := a.View(ctx)
		if e != nil {
			return e
		}
		if c.Kind == "cancel" && v.State.Parent != nil && (v.State.Parent.Status == "cancelled" || v.State.Parent.Status == "completed" || v.State.Parent.Status == "expired") {
			return nil
		}
		c.ExpectedRevision = v.Account.Revision
		authority := v.AuthorityRevision()
		c.ExpectedAuthorityRevision = &authority
		_, e = a.Do(ctx, c)
		if !errors.Is(e, pg.ErrStaleRevision) {
			return e
		}
	}
	return pg.ErrStaleRevision
}
