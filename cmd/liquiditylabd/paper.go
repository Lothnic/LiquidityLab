package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"mime"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/liquiditylab/liquiditylab/internal/api"
	d "github.com/liquiditylab/liquiditylab/internal/domain"
	"github.com/liquiditylab/liquiditylab/internal/inference"
	"github.com/liquiditylab/liquiditylab/internal/marketdata"
	rt "github.com/liquiditylab/liquiditylab/internal/runtime"
	pg "github.com/liquiditylab/liquiditylab/internal/store/postgres"
	"github.com/liquiditylab/liquiditylab/internal/telemetry"
)

const paperDaemonTenant = "synthetic-paper-daemon"

var paperDaemonSchema = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)
var paperDaemonAccount = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)

type paperServerOptions struct {
	metricsOutput                                           string
	replayArchive                                           string
	resume                                                  bool
	policyTarget, policyHash, policyFallback, policyProfile string
	policyAllowUnvalidated                                  bool
	policyTimeout                                           time.Duration

	listen, schema, account, source, webDir, captureOutput string
	initialBase, initialQuote                              d.Decimal
	duration                                               time.Duration
	maxFrames                                              uint64
	maxCaptureBytes                                        int64
}

type paperSource func(context.Context, func(context.Context, marketdata.Event) error) error

type paperDaemon struct {
	metrics       *telemetry.Recorder
	metricsOutput *os.File
	policyClient  *inference.Client
	replayStore   *pg.Store
	stopReplay    context.CancelFunc
	replayDone    chan struct{}
	store         *pg.Store
	actor         *rt.Actor
	key           pg.Key
	handler       http.Handler
	log           io.Writer
	stopSource    context.CancelFunc
	sourceDone    chan struct{}
	closing       atomic.Bool
	ingress       sync.Mutex
	reconciled    bool
	gapSeen       bool
	closeOnce     sync.Once
	closeResult   error
}

func parsePaperOptions(args []string) (paperServerOptions, error) {
	flags := flag.NewFlagSet("paper", flag.ContinueOnError)
	var opt paperServerOptions
	if len(args) > 0 && (args[0] == "create" || args[0] == "resume") {
		opt.resume = args[0] == "resume"
		args = args[1:]
	}
	flags.StringVar(&opt.policyTarget, "policy-target", os.Getenv("LIQUIDITYLAB_POLICY_TARGET"), "opt-in local Python policy RPC target (loopback IP:port or unix:/absolute/path)")
	flags.StringVar(&opt.policyHash, "policy-sha256", os.Getenv("LIQUIDITYLAB_POLICY_SHA256"), "exact pinned policy bundle SHA-256")
	flags.StringVar(&opt.policyFallback, "policy-fallback", os.Getenv("LIQUIDITYLAB_POLICY_FALLBACK"), "explicit policy failure fallback: baseline or pause")
	flags.StringVar(&opt.policyProfile, "policy-profile", "paper", "paper executes accepted proposals; shadow records proposals and executes the pinned baseline")
	flags.BoolVar(&opt.policyAllowUnvalidated, "policy-allow-unvalidated", os.Getenv("LIQUIDITYLAB_POLICY_ALLOW_UNVALIDATED") == "true", "explicit opt-in to unevaluated policy in paper/shadow only")
	flags.DurationVar(&opt.policyTimeout, "policy-timeout", 100*time.Millisecond, "policy response budget (0 < timeout <= 200ms)")
	flags.StringVar(&opt.listen, "listen", "127.0.0.1:8080", "literal loopback HTTP address")
	flags.StringVar(&opt.schema, "schema", "", "explicit isolated PostgreSQL schema (required)")
	flags.StringVar(&opt.account, "account", "", "synthetic account ID (create: new; resume: existing)")
	flags.StringVar(&opt.source, "source", "kraken", "kraken public data or synthetic deterministic demo")
	flags.StringVar(&opt.webDir, "web-dir", "apps/web/dist", "built React index.html/assets directory")
	flags.StringVar(&opt.captureOutput, "capture-output", "", "new raw JSONL recording path (required for kraken)")
	flags.StringVar(&opt.metricsOutput, "metrics-output", "", "optional new local JSONL path for shutdown metrics (never served over HTTP)")
	flags.StringVar(&opt.replayArchive, "replay-archive", "", "optional absolute immutable public archive root; enables bounded analytical replay jobs")
	base := flags.String("initial-base", "", "explicit initial synthetic BTC balance (required)")
	quote := flags.String("initial-quote", "", "explicit initial synthetic USD balance (required)")
	flags.DurationVar(&opt.duration, "duration", 30*time.Minute, "maximum server lifetime (0 < duration <= 24h)")
	flags.Uint64Var(&opt.maxFrames, "max-frames", 100000, "Kraken capture frame cap (1..1000000)")
	flags.Int64Var(&opt.maxCaptureBytes, "max-capture-bytes", 64<<20, "Kraken raw recording cap (1024..1GiB)")
	if err := flags.Parse(args); err != nil {
		return opt, err
	}
	if flags.NArg() != 0 {
		return opt, errors.New("paper accepts create/resume followed by flags")
	}
	balancesProvided := false
	flags.Visit(func(f *flag.Flag) {
		if f.Name == "initial-base" || f.Name == "initial-quote" {
			balancesProvided = true
		}
	})
	if opt.resume && balancesProvided {
		return opt, errors.New("paper resume rejects initial balances; retained balances are authoritative")
	}
	if !opt.resume {
		if *base == "" || *quote == "" {
			return opt, errors.New("paper create requires explicit --initial-base and --initial-quote balances")
		}
		var err error
		if opt.initialBase, err = d.ParseDecimal(*base); err != nil || opt.initialBase.Sign() < 0 {
			return opt, errors.New("initial-base must be a nonnegative exact decimal")
		}
		if opt.initialQuote, err = d.ParseDecimal(*quote); err != nil || opt.initialQuote.Sign() < 0 {
			return opt, errors.New("initial-quote must be a nonnegative exact decimal")
		}
	}
	if err := opt.validate(); err != nil {
		return opt, err
	}
	return opt, nil
}

func paperServer(args []string) error {
	opt, err := parsePaperOptions(args)
	if err != nil {
		return err
	}
	dsn, token := os.Getenv("LIQUIDITYLAB_DATABASE_URL"), os.Getenv("LIQUIDITYLAB_API_TOKEN")
	if dsn == "" || !paperTokenValid(token) {
		return errors.New("paper requires LIQUIDITYLAB_DATABASE_URL and LIQUIDITYLAB_API_TOKEN (32..4096 bytes without whitespace)")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return servePaper(ctx, opt, dsn, token, os.Stderr)
}

func paperTokenValid(token string) bool {
	return len(token) >= 32 && len(token) <= 4096 && !strings.ContainsAny(token, " \t\r\n")
}

func (o paperServerOptions) validate() error {
	if o.replayArchive != "" && !filepath.IsAbs(o.replayArchive) {
		return errors.New("replay-archive requires an absolute operator-selected archive root")
	}
	if o.policyTarget != "" {
		hash, err := hex.DecodeString(o.policyHash)
		if err != nil || len(hash) != 32 || strings.ToLower(o.policyHash) != o.policyHash || !o.policyAllowUnvalidated || (o.policyFallback != "baseline" && o.policyFallback != "pause") || (o.policyProfile != "paper" && o.policyProfile != "shadow") || o.policyTimeout <= 0 || o.policyTimeout > inference.MaxValidity {
			return errors.New("policy requires pinned lowercase SHA-256, explicit allow-unvalidated, baseline/pause fallback, paper/shadow profile and timeout up to 200ms")
		}
	} else if o.policyHash != "" || o.policyFallback != "" || o.policyAllowUnvalidated {
		return errors.New("policy configuration requires a local --policy-target")
	}
	host, port, err := net.SplitHostPort(o.listen)
	ip := net.ParseIP(host)
	if err != nil || ip == nil || !ip.IsLoopback() {
		return errors.New("paper HTTP listener requires a literal loopback IP and port")
	}
	if _, err := strconv.ParseUint(port, 10, 16); err != nil {
		return errors.New("paper HTTP listener port must be numeric and in 0..65535")
	}
	for _, digit := range port {
		if digit < '0' || digit > '9' {
			return errors.New("paper HTTP listener port must be numeric and in 0..65535")
		}
	}
	if !paperDaemonSchema.MatchString(o.schema) || o.schema == "public" || o.schema == "information_schema" || strings.HasPrefix(o.schema, "pg_") {
		return errors.New("paper requires an isolated lowercase --schema excluding public and system schemas")
	}
	if !paperDaemonAccount.MatchString(o.account) || o.account == "." || o.account == ".." {
		return errors.New("paper requires a new --account of 1..128 letters, digits, underscores, dots or hyphens")
	}
	if o.duration <= 0 || o.duration > 24*time.Hour || o.initialBase.Sign() < 0 || o.initialQuote.Sign() < 0 {
		return errors.New("paper duration must be positive and at most 24h; synthetic balances must be nonnegative")
	}
	if o.source != "kraken" && o.source != "synthetic" || o.webDir == "" {
		return errors.New("paper --source must be kraken or synthetic and --web-dir must contain built assets")
	}
	if o.maxFrames == 0 || o.maxFrames > 1000000 || o.maxCaptureBytes < 1024 || o.maxCaptureBytes > 1<<30 {
		return errors.New("paper capture bounds must be 1..1000000 frames and 1024..1GiB bytes")
	}
	if o.source == "kraken" {
		if o.captureOutput == "" {
			return errors.New("kraken source requires a new --capture-output raw JSONL path")
		}
		for _, name := range []string{o.captureOutput, o.captureOutput + ".manifest.json", o.captureOutput + ".manifest.json.tmp"} {
			if _, err := os.Lstat(name); !errors.Is(err, os.ErrNotExist) {
				return errors.New("capture-output or manifest path already exists or cannot be inspected")
			}
		}
	}
	return validatePaperOutputPaths(o)
}

func paperServerOperation() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 7*time.Second)
}

func paperServerError(stage string, err error) error {
	reason := "operation failed; connection details withheld"
	for _, kind := range []struct {
		err error
		why string
	}{{rt.ErrOutcomeUnknown, "accepted outcome unknown; retain identities for operator recovery"}, {pg.ErrOwnership, "ownership or lease lost; operator recovery required"}, {pg.ErrIsolationRequired, "prior sender isolation and reconciliation required"}, {pg.ErrStaleRevision, "stale revision"}, {pg.ErrConflict, "durable identity conflict"}, {rt.ErrRecoveryRequired, "recovery required"}, {rt.ErrHistoryLimit, "bounded runtime history exhausted"}, {context.DeadlineExceeded, "deadline exceeded; accepted outcome may be unknown"}} {
		if errors.Is(err, kind.err) {
			reason = kind.why
			break
		}
	}
	return fmt.Errorf("%s: %s", stage, reason)
}

func servePaper(ctx context.Context, opt paperServerOptions, dsn, token string, output io.Writer) error {
	if err := opt.validate(); err != nil {
		return err
	}
	// Validate and preload immutable assets before creating any account.
	static, err := paperStatic(opt.webDir)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", opt.listen)
	if err != nil {
		return errors.New("paper loopback HTTP listener could not be opened")
	}
	defer listener.Close()
	lifetime, cancel := context.WithTimeout(ctx, opt.duration)
	defer cancel()
	daemon, err := newPaperDaemon(lifetime, opt, dsn, token, static, output, productionPaperSource(opt))
	if err != nil {
		return err
	}
	server := &http.Server{
		Handler: daemon.handler, ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 10 * time.Second,
		WriteTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10,
		ErrorLog: log.New(io.Discard, "", 0),
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()
	fmt.Fprintf(output, "PAPER server http://%s account=%s/%s source=%s; balances, rules, fees and fills are SYNTHETIC; no exchange orders\n", listener.Addr(), daemon.key.TenantID, daemon.key.AccountID, opt.source)
	var serveErr error
	select {
	case <-lifetime.Done():
	case err := <-serveDone:
		if !errors.Is(err, http.ErrServerClosed) {
			serveErr = errors.New("paper HTTP server stopped unexpectedly")
		}
	}
	// Refuse new mutations before cancelling the parent and draining the actor.
	daemon.closing.Store(true)
	cancel()
	// Stop account execution before spending the HTTP grace period on slow
	// clients. Already admitted actor requests remain revision checked and drain
	// before the close completes; subsequent HTTP operations see unavailable.
	closeErr := daemon.Close()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	httpErr := server.Shutdown(shutdownCtx)
	shutdownCancel()
	if httpErr != nil {
		_ = server.Close()
	}
	if serveErr != nil {
		return serveErr
	}
	if httpErr != nil {
		return errors.New("paper HTTP shutdown exceeded its deadline; inspect durable account outcomes")
	}
	return closeErr
}

func newPaperDaemon(ctx context.Context, opt paperServerOptions, dsn, token string, static http.Handler, output io.Writer, source paperSource) (*paperDaemon, error) {
	if !paperTokenValid(token) || static == nil || source == nil {
		return nil, errors.New("invalid paper server authentication, assets or source configuration")
	}
	if err := validatePaperOutputPaths(opt); err != nil {
		return nil, err
	}
	metrics, metricsOutput, err := openPaperMetrics(opt.metricsOutput)
	if err != nil {
		return nil, err
	}
	metricsOwned := false
	defer func() {
		if metricsOutput != nil && !metricsOwned {
			_ = metricsOutput.Close()
		}
	}()
	op, cancel := paperServerOperation()
	store, err := pg.OpenWithOptions(op, dsn, opt.schema, pg.Options{Telemetry: metrics})
	cancel()
	if err != nil {
		return nil, paperServerError("open isolated paper database", err)
	}
	fail := func(stage string, err error) (*paperDaemon, error) {
		store.Close()
		return nil, paperServerError(stage, err)
	}
	op, cancel = paperServerOperation()
	err = store.Migrate(op)
	cancel()
	if err != nil {
		return fail("migrate isolated paper database", err)
	}
	var policyClient *inference.Client
	if opt.policyTarget != "" {
		op, cancel = paperServerOperation()
		policyClient, err = inference.New(op, inference.Config{Telemetry: metrics, Target: opt.policyTarget, PolicySHA256: opt.policyHash, Profile: opt.policyProfile, AllowUnvalidated: opt.policyAllowUnvalidated, Timeout: opt.policyTimeout})
		cancel()
		if err != nil {
			return fail("connect and verify pinned paper policy", err)
		}
	}
	policyOwned := false
	defer func() {
		if policyClient != nil && !policyOwned {
			_ = policyClient.Close()
		}
	}()
	key := pg.Key{TenantID: paperDaemonTenant, AccountID: opt.account}
	op, cancel = paperServerOperation()
	if opt.resume {
		_, err = store.GetAccount(op, key)
	} else {
		initial := []pg.Balance{{Currency: "BTC", Total: opt.initialBase, Free: opt.initialBase}, {Currency: "USD", Total: opt.initialQuote, Free: opt.initialQuote}}
		_, err = store.CreateAccount(op, key, initial, json.RawMessage(`{}`))
	}
	cancel()
	if err != nil {
		if errors.Is(err, pg.ErrConflict) {
			store.Close()
			return nil, errors.New("paper account already exists; refusing reset in create mode; use paper resume for owned recovery")
		}
		if opt.resume {
			return fail("open existing paper account; no account created or balances replaced", err)
		}
		return fail("create NEW synthetic paper account", err)
	}
	config := rt.DefaultConfig(store, key)
	config.Telemetry = metrics
	config.AutoExecute = true
	if policyClient != nil {
		config.Policy = &rt.PolicyConfig{Worker: policyClient, PolicySHA256: opt.policyHash, ExpectedMode: policyClient.PromotionStatus(), Profile: opt.policyProfile, AllowUnvalidated: opt.policyAllowUnvalidated, Fallback: opt.policyFallback, Timeout: opt.policyTimeout}
	}
	config.DataSource = "synthetic"
	if opt.source == "kraken" {
		config.DataSource = "kraken-public"
	}
	op, cancel = paperServerOperation()
	actor, err := rt.New(op, config)
	cancel()
	if err != nil {
		return fail("start compatible durable paper actor; existing account retained", err)
	}
	if opt.resume {
		fmt.Fprintln(output, "PAPER startup state=recovering; retained balances and original deadline preserved")
		op, cancel = paperServerOperation()
		recoveryErr := actor.RecoverPaper(op)
		cancel()
		if recoveryErr != nil && !errors.Is(recoveryErr, rt.ErrRecoveryRequired) {
			fmt.Fprintln(output, "PAPER startup state=recovery-required; retained outcomes require inspection")
		}
	}
	replayService, replayStore, err := openPaperReplayJobs(ctx, opt, dsn)
	if err != nil {
		op, cancel = paperServerOperation()
		_ = actor.Close(op)
		cancel()
		return fail("configure bounded analytical replay", err)
	}
	replayOwned := false
	defer func() {
		if replayStore != nil && !replayOwned {
			replayStore.Close()
		}
	}()
	var replayAPI api.ReplayService
	if replayService != nil {
		replayAPI = replayService
	}
	handler, err := api.New(api.Config{Accounts: map[pg.Key]api.Authority{key: actor}, Credentials: []api.Credential{{Token: token, Scopes: []pg.Key{key}}}, Journal: store, Replays: replayAPI})
	if err != nil {
		op, cancel = paperServerOperation()
		_ = actor.Close(op)
		cancel()
		return fail("configure authenticated trader API", err)
	}
	sourceCtx, stopSource := context.WithCancel(ctx)
	if metrics != nil {
		sourceCtx = context.WithValue(sourceCtx, paperMetricsContextKey{}, metrics)
	}
	daemon := &paperDaemon{policyClient: policyClient, store: store, actor: actor, key: key, log: output, stopSource: stopSource, sourceDone: make(chan struct{})}
	if replayService != nil {
		replayCtx, stopReplay := context.WithCancel(ctx)
		daemon.replayStore, daemon.stopReplay, daemon.replayDone = replayStore, stopReplay, make(chan struct{})
		go func() {
			defer close(daemon.replayDone)
			_ = replayService.Run(replayCtx)
		}()
	}
	replayOwned = true
	daemon.metrics, daemon.metricsOutput = metrics, metricsOutput
	metricsOwned = true
	daemon.handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		if r.URL.Path == "/healthz" || r.URL.Path == "/v1" || strings.HasPrefix(r.URL.Path, "/v1/") {
			if daemon.closing.Load() && r.Method != http.MethodGet && r.Method != http.MethodHead {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = io.WriteString(w, `{"error":"server_stopping"}`)
				return
			}
			handler.ServeHTTP(w, r)
			return
		}
		static.ServeHTTP(w, r)
	})
	policyOwned = true
	go func() {
		defer close(daemon.sourceDone)
		err := source(sourceCtx, daemon.Ingest)
		if sourceCtx.Err() == nil {
			_ = daemon.invalidate()
			if err != nil {
				fmt.Fprintln(output, "PAPER public-data source failed; readiness invalidated; account remains available for inspection/cancellation")
			} else {
				fmt.Fprintln(output, "PAPER public-data source ended; readiness invalidated; account remains available for inspection/cancellation")
			}
		}
	}()
	return daemon, nil
}

func productionPaperSource(opt paperServerOptions) paperSource {
	if opt.source == "synthetic" {
		return syntheticPaperSource
	}
	return func(ctx context.Context, deliver func(context.Context, marketdata.Event) error) error {
		metrics, _ := ctx.Value(paperMetricsContextKey{}).(*telemetry.Recorder)
		_, err := marketdata.Capture(ctx, marketdata.CaptureConfig{
			Telemetry: metrics,
			Symbol:    "BTC/USD", Output: opt.captureOutput, Duration: opt.duration, Depth: 10,
			MaxFrames: opt.maxFrames, MaxBytes: opt.maxCaptureBytes, OnEvent: deliver,
		})
		return err
	}
}

func syntheticPaperSource(ctx context.Context, deliver func(context.Context, marketdata.Event) error) error {
	started := time.Now()
	session := fmt.Sprintf("synthetic-demo-%d", started.UnixNano())
	event := marketdata.Event{Kind: "snapshot", Type: "snapshot", Symbol: "BTC/USD", Valid: true, ReceiptSessionID: session,
		Bids: [][]string{{"99.99", "2"}}, Asks: [][]string{{"100.01", "2"}}}
	if err := deliver(ctx, event); err != nil {
		return err
	}
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	var index uint64
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			index++
			event = marketdata.Event{Kind: "trade", Type: "trade", Time: time.Since(started).Nanoseconds(), Symbol: "BTC/USD", Valid: true,
				ReceiptIndex: index, ReceiptSessionID: session, Price: "99.99", Quantity: "0.4", Side: "sell"}
			if err := deliver(ctx, event); err != nil {
				return err
			}
		}
	}
}

func serverMarketEvent(wire marketdata.Event) (d.MarketEvent, error) {
	if wire.Symbol != "BTC/USD" || wire.Time < 0 || len(wire.ReceiptSessionID) == 0 || len(wire.ReceiptSessionID) > 128 || len(wire.Bids)+len(wire.Asks) > 512 || wire.Type != "" && wire.Type != wire.Kind {
		return d.MarketEvent{}, rt.ErrInvalid
	}
	event := d.MarketEvent{Kind: wire.Kind, Time: wire.Time, Symbol: wire.Symbol, ReceiptIndex: wire.ReceiptIndex, Invalid: !wire.Valid, Side: wire.Side, TradeID: wire.TradeID}
	levels := func(input [][]string) ([]d.Level, error) {
		out := make([]d.Level, 0, len(input))
		for _, pair := range input {
			if len(pair) != 2 {
				return nil, rt.ErrInvalid
			}
			price, err := d.ParseDecimal(pair[0])
			if err != nil || price.Sign() <= 0 {
				return nil, rt.ErrInvalid
			}
			quantity, err := d.ParseDecimal(pair[1])
			if err != nil || quantity.Sign() < 0 {
				return nil, rt.ErrInvalid
			}
			out = append(out, d.Level{Price: price, Quantity: quantity})
		}
		return out, nil
	}
	var err error
	switch wire.Kind {
	case "snapshot", "book":
		event.Bids, err = levels(wire.Bids)
		if err == nil {
			event.Asks, err = levels(wire.Asks)
		}
	case "trade":
		event.Price, err = d.ParseDecimal(wire.Price)
		if err == nil {
			event.Quantity, err = d.ParseDecimal(wire.Quantity)
		}
		if err == nil && (event.Price.Sign() <= 0 || event.Quantity.Sign() <= 0 || wire.Side != "buy" && wire.Side != "sell" || len(wire.Bids)+len(wire.Asks) != 0) {
			err = rt.ErrInvalid
		}
	case "gap":
		event.Invalid = true
	default:
		err = rt.ErrInvalid
	}
	return event, err
}

func (p *paperDaemon) Ingest(ctx context.Context, wire marketdata.Event) (resultErr error) {
	var started time.Time
	if p.metrics != nil {
		started = time.Now()
	}
	p.metrics.Add(telemetry.PublicIngressRequests, 1)
	defer func() {
		if !started.IsZero() {
			p.metrics.Observe(telemetry.PublicIngressNS, uint64(max(0, time.Since(started).Nanoseconds())))
		}
		if resultErr != nil {
			p.metrics.Add(telemetry.PublicIngressErrors, 1)
		}
	}()
	received, hasOrigin := marketdata.SourceReceiptStart(ctx)
	if !hasOrigin {
		p.metrics.Add(telemetry.PublicReceiptOriginMissing, 1)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	p.ingress.Lock()
	defer p.ingress.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	ctx, stop := context.WithTimeout(ctx, 7*time.Second)
	defer stop()
	if p.closing.Load() {
		return rt.ErrClosed
	}
	event, err := serverMarketEvent(wire)
	if err != nil {
		_ = p.invalidateLocked()
		return rt.ErrInvalid
	}
	sum := sha256.Sum256([]byte(wire.ReceiptSessionID))
	id := fmt.Sprintf("web-feed-%x-%d", sum[:12], wire.ReceiptIndex)
	op, cancel := context.WithCancel(ctx)
	err = p.actor.MarketFromSession(op, event, id, wire.ReceiptSessionID)
	cancel()
	if err != nil {
		_ = p.invalidateLocked()
		return paperServerError("paper public market ingress", err)
	}
	if hasOrigin && !event.Invalid && (event.Kind == "snapshot" || event.Kind == "book") {
		p.metrics.Observe(telemetry.PublicReceiptToBookNS, uint64(max(0, time.Since(received).Nanoseconds())))
	}
	if event.Invalid || event.Kind == "gap" {
		p.gapSeen = true
		return nil
	}
	if !p.reconciled && !p.gapSeen && event.Kind == "snapshot" {
		for attempt := 0; attempt < 32; attempt++ {
			op, cancel = context.WithCancel(ctx)
			view, viewErr := p.actor.View(op)
			cancel()
			if viewErr != nil || !view.State.Book.Valid {
				_ = p.invalidateLocked()
				return errors.New("paper initial snapshot not valid; recovery required")
			}
			op, cancel = context.WithCancel(ctx)
			err = p.actor.RecoverPaper(op)
			cancel()
			if errors.Is(err, pg.ErrStaleRevision) {
				continue
			}
			if err != nil {
				_ = p.invalidateLocked()
				return paperServerError("owned simulated recovery", err)
			}
			p.reconciled = true
			fmt.Fprintln(p.log, "PAPER startup state=ready; recovered plans remain paused until explicit trader resume")
			return nil
		}
		_ = p.invalidateLocked()
		return errors.New("initial simulated reconciliation could not obtain a current account revision")
	}
	return nil
}

func (p *paperDaemon) invalidate() error {
	p.ingress.Lock()
	defer p.ingress.Unlock()
	return p.invalidateLocked()
}
func (p *paperDaemon) invalidateLocked() error {
	p.gapSeen = true
	op, cancel := paperServerOperation()
	defer cancel()
	return p.actor.MarketFromSession(op, d.MarketEvent{Kind: "gap", Symbol: "BTC/USD", Invalid: true}, "web-source-ended", "web-source-ended")
}

func (p *paperDaemon) Close() error {
	p.closeOnce.Do(func() {
		p.closing.Store(true)
		if p.stopReplay != nil {
			p.stopReplay()
		}
		p.stopSource()
		select {
		case <-p.sourceDone:
		case <-time.After(8 * time.Second):
			p.closeResult = errors.New("paper source shutdown exceeded deadline; source outcome unknown")
		}
		for attempt := 0; attempt < 32; attempt++ {
			op, cancel := paperServerOperation()
			view, err := p.actor.View(op)
			cancel()
			if err != nil {
				p.closeResult = paperServerError("shutdown account view", err)
				break
			}
			parent := view.State.Parent
			if parent == nil || parent.Status == "completed" || parent.Status == "cancelled" || parent.Status == "expired" {
				break
			}
			op, cancel = paperServerOperation()
			_, err = p.actor.Do(op, rt.Command{ID: fmt.Sprintf("web-shutdown-cancel-%d", view.Account.Revision), ExpectedRevision: view.Account.Revision, Kind: "cancel"})
			cancel()
			if errors.Is(err, pg.ErrStaleRevision) {
				continue
			}
			if err != nil {
				p.closeResult = paperServerError("cancel paper plan during shutdown", err)
			}
			break
		}
		op, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := p.actor.Close(op)
		cancel()
		if err != nil {
			p.closeResult = errors.New("paper actor shutdown deadline exceeded; accepted outcomes unknown")
		}
		op, cancel = paperServerOperation()
		account, err := p.store.GetAccount(op, p.key)
		cancel()
		knownFinal := false
		if err == nil {
			state, decodeErr := rt.DecodeSnapshot(account.Snapshot)
			terminal := state.Parent == nil || state.Parent.Status == "completed" || state.Parent.Status == "cancelled" || state.Parent.Status == "expired"
			knownFinal = decodeErr == nil && terminal && len(account.Intents) == 0 && len(account.Reservations) == 0 && len(state.PendingFacts) == 0 && len(state.UnresolvedFacts) == 0 && account.ObservedSequence == account.ProcessedSequence && p.closeResult == nil
		}
		if !knownFinal && p.closeResult == nil {
			p.closeResult = errors.New("paper shutdown outcome not known final; retain durable account for operator recovery")
		}
		fmt.Fprintf(p.log, "PAPER stopped account=%s/%s known_final=%t; durable state retained\n", p.key.TenantID, p.key.AccountID, knownFinal)
		if p.policyClient != nil {
			_ = p.policyClient.Close()
		}
		if err := p.finishMetrics(knownFinal); err != nil {
			p.closeResult = errors.Join(p.closeResult, err)
		}
		if err := p.closeReplayJobs(); err != nil {
			p.closeResult = errors.Join(p.closeResult, err)
		}
		p.store.Close()
	})
	return p.closeResult
}

// Preload a small, immutable allowlist. No request can traverse a filesystem or
// expose raw recordings, source maps, dotfiles, directories or arbitrary files.
func paperStatic(directory string) (http.Handler, error) {
	absolute, err := filepath.Abs(directory)
	if err != nil {
		return nil, errors.New("invalid built web directory")
	}
	for current := absolute; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("built web directory must exist without symlink components")
		}
		if current == filepath.Dir(current) {
			break
		}
	}
	root, err := os.OpenRoot(absolute)
	if err != nil {
		return nil, errors.New("built web directory could not be opened")
	}
	defer root.Close()
	type asset struct {
		body []byte
		mime string
		etag string
	}
	assets := map[string]asset{}
	var total int64
	extensions := map[string]bool{".js": true, ".css": true, ".svg": true, ".png": true, ".jpg": true, ".jpeg": true, ".webp": true, ".avif": true, ".gif": true, ".ico": true, ".woff": true, ".woff2": true, ".ttf": true}
	err = fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("symlinks are not permitted in built web assets")
		}
		if entry.IsDir() {
			if name != "." && name != "assets" && !strings.HasPrefix(name, "assets/") {
				return fs.SkipDir
			}
			return nil
		}
		allowed := name == "index.html" || name == "favicon.svg" || name == "favicon.ico" || strings.HasPrefix(name, "assets/") && extensions[strings.ToLower(path.Ext(name))]
		for _, part := range strings.Split(name, "/") {
			if strings.HasPrefix(part, ".") {
				allowed = false
			}
		}
		if !allowed {
			return nil
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() || info.Size() > 8<<20 || total+info.Size() > 32<<20 || len(assets) >= 512 {
			return errors.New("built web assets exceed regular-file or size/count limits")
		}
		file, err := root.Open(name)
		if err != nil {
			return err
		}
		body, readErr := io.ReadAll(io.LimitReader(file, (8<<20)+1))
		_ = file.Close()
		if readErr != nil || len(body) > 8<<20 || total+int64(len(body)) > 32<<20 {
			return errors.New("built web asset could not be read within its size bound")
		}
		total += int64(len(body))
		sum := sha256.Sum256(body)
		assets["/"+name] = asset{body: body, mime: mime.TypeByExtension(path.Ext(name)), etag: `"` + hex.EncodeToString(sum[:]) + `"`}
		return nil
	})
	if err != nil {
		return nil, errors.New("built web directory contains unsafe or oversized assets")
	}
	if _, ok := assets["/index.html"]; !ok {
		return nil, errors.New("built web directory requires index.html; build apps/web first")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; connect-src 'self'; img-src 'self' data:; object-src 'none'; base-uri 'none'; frame-ancestors 'none'")
		w.Header().Set("Referrer-Policy", "no-referrer")
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		name := r.URL.Path
		if strings.Contains(name, "\\") || strings.Contains(name, "\x00") || path.Clean(name) != name && name != "/" || r.URL.RawPath != "" {
			http.NotFound(w, r)
			return
		}
		if name == "/" {
			name = "/index.html"
		}
		asset, ok := assets[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Content-Type", asset.mime)
		w.Header().Set("ETag", asset.etag)
		http.ServeContent(w, r, path.Base(name), time.Time{}, bytes.NewReader(asset.body))
	}), nil
}
