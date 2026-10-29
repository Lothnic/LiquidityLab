package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"time"

	d "github.com/liquiditylab/liquiditylab/internal/domain"
	x "github.com/liquiditylab/liquiditylab/internal/execution"
	"github.com/liquiditylab/liquiditylab/internal/marketarchive"
	"github.com/liquiditylab/liquiditylab/internal/marketdata"
	rt "github.com/liquiditylab/liquiditylab/internal/runtime"
	pg "github.com/liquiditylab/liquiditylab/internal/store/postgres"
)

const paperMaxDuration = 24 * time.Hour

var paperSchema = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)

type paperOptions struct {
	archiveRoot, datasetID                        string
	eventsPath, planPath, schema, account, policy string
	initialBase, initialQuote                     d.Decimal
	duration                                      time.Duration
	maxBytes                                      int64
	maxEvents, maxLineBytes                       int
}

type paperRecording struct {
	sealedSource  *rt.SealedSource
	sourceCursors []rt.SourceCursor
	events        []d.MarketEvent
	digest        string
	bytes         int64
}

type paperCommandReceipt struct {
	Command rt.Command `json:"command"`
	Result  pg.Result  `json:"result"`
	Outcome string     `json:"outcome"`
}

type paperJournalCount struct {
	Kind          string `json:"kind"`
	Entries       int64  `json:"entries"`
	FirstRevision int64  `json:"first_revision"`
	LastRevision  int64  `json:"last_revision"`
}

type paperSummary struct {
	SealedSource           *rt.SealedSource      `json:"sealed_source"`
	Profile                string                `json:"profile"`
	Source                 string                `json:"source"`
	Simulation             string                `json:"simulation"`
	SessionID              string                `json:"session_id"`
	RecordingSHA256        string                `json:"recording_sha256"`
	RecordingBytes         int64                 `json:"recording_bytes"`
	Schema                 string                `json:"schema"`
	Policy                 string                `json:"policy"`
	InitialBalances        []pg.Balance          `json:"initial_synthetic_balances"`
	ReconciliationEvidence string                `json:"simulated_reconciliation_evidence,omitempty"`
	StartedAt              time.Time             `json:"started_at"`
	EndedAt                time.Time             `json:"ended_at"`
	EventsAvailable        int                   `json:"events_available"`
	EventsDelivered        int                   `json:"events_delivered"`
	StopReason             string                `json:"stop_reason"`
	ActorClosed            bool                  `json:"actor_closed"`
	KnownFinal             bool                  `json:"known_final"`
	Error                  string                `json:"error,omitempty"`
	Account                pg.Account            `json:"account"`
	State                  rt.State              `json:"durable_state"`
	Fills                  []d.Fill              `json:"simulated_fills"`
	CommandReceipts        []paperCommandReceipt `json:"command_receipts"`
	Journal                []paperJournalCount   `json:"journal_summary"`
	JournalEntries         int64                 `json:"journal_entries"`
}

func paper(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("paper", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var opt paperOptions
	fs.StringVar(&opt.eventsPath, "events", "", "bounded normalized Go JSONL recording (exclusive with --archive and --dataset-id)")
	fs.StringVar(&opt.archiveRoot, "archive", "", "verified sealed archive root (requires --dataset-id)")
	fs.StringVar(&opt.datasetID, "dataset-id", "", "verified dataset content ID (requires --archive)")
	fs.StringVar(&opt.planPath, "plan", "", "Go domain.Plan JSON with deadline_ns (required)")
	fs.StringVar(&opt.schema, "schema", "", "isolated PostgreSQL schema (required; never dropped)")
	fs.StringVar(&opt.account, "account", "", "new synthetic account ID (default: recording digest)")
	fs.StringVar(&opt.policy, "policy", x.BaselineScheduled, "scheduled or liquidity-aware baseline")
	base := fs.String("initial-base", "", "explicit initial synthetic BTC balance (required)")
	quote := fs.String("initial-quote", "", "explicit initial synthetic USD balance (required)")
	fs.DurationVar(&opt.duration, "duration", 30*time.Second, "maximum recorded-data running time (0 < duration <= 24h)")
	fs.Int64Var(&opt.maxBytes, "max-input-bytes", 16<<20, "maximum recording bytes (1..64MiB)")
	fs.IntVar(&opt.maxEvents, "max-events", 10000, "maximum events (1..10000)")
	fs.IntVar(&opt.maxLineBytes, "max-line-bytes", 256<<10, "maximum JSONL line bytes (128..1MiB)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("paper accepts flags only")
	}
	if *base == "" || *quote == "" {
		return errors.New("paper requires explicit --initial-base and --initial-quote synthetic balances")
	}
	var err error
	if opt.initialBase, err = d.ParseDecimal(*base); err != nil || opt.initialBase.Sign() < 0 {
		return errors.New("initial-base must be a nonnegative exact decimal")
	}
	if opt.initialQuote, err = d.ParseDecimal(*quote); err != nil || opt.initialQuote.Sign() < 0 {
		return errors.New("initial-quote must be a nonnegative exact decimal")
	}
	if err := opt.validate(); err != nil {
		return err
	}
	dsn := os.Getenv("LIQUIDITYLAB_DATABASE_URL")
	if dsn == "" {
		return errors.New("paper requires LIQUIDITYLAB_DATABASE_URL")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runPaper(ctx, opt, dsn, stdout, stderr)
}

func (o paperOptions) validate() error {
	if o.planPath == "" || o.schema == "" || (o.eventsPath == "" && (o.archiveRoot == "" || o.datasetID == "")) || (o.eventsPath != "" && (o.archiveRoot != "" || o.datasetID != "")) {
		return errors.New("paper requires --plan, --schema and exactly one source: --events or --archive with --dataset-id")
	}
	if o.archiveRoot != "" && !marketarchive.ValidID(o.datasetID) {
		return errors.New("dataset-id must be lowercase 64-hex")
	}
	if !paperSchema.MatchString(o.schema) || o.schema == "public" || o.schema == "information_schema" || strings.HasPrefix(o.schema, "pg_") {
		return errors.New("schema must name an isolated lowercase PostgreSQL schema, excluding public and system schemas")
	}
	if len(o.account) > 128 || strings.ContainsAny(o.account, "\x00\r\n") {
		return errors.New("account must be at most 128 bytes without control line breaks")
	}
	if err := x.ValidateBaselinePolicy(o.policy); err != nil {
		return errors.New("policy must be scheduled or liquidity-aware")
	}
	if o.duration <= 0 || o.duration > paperMaxDuration || o.maxBytes <= 0 || o.maxBytes > 64<<20 || o.maxEvents <= 0 || o.maxEvents > 10000 || o.maxLineBytes < 128 || o.maxLineBytes > 1<<20 {
		return errors.New("paper duration or input bounds are outside supported limits")
	}
	return nil
}

func paperReadFile(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return nil, errors.New("paper input must be a regular file within its byte limit")
	}
	body, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, errors.New("paper input exceeds its byte limit")
	}
	return body, nil
}

func paperDecode(body []byte, dst any) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("expected exactly one JSON value")
	}
	return nil
}

func paperPlan(path string, initialBase, initialQuote d.Decimal) (d.Plan, error) {
	body, err := paperReadFile(path, 64<<10)
	if err != nil {
		return d.Plan{}, err
	}
	var plan d.Plan
	if err := paperDecode(body, &plan); err != nil {
		return plan, fmt.Errorf("invalid Go plan JSON (use deadline_ns): %w", err)
	}
	if err := plan.Validate(); err != nil || plan.Deadline > int64(paperMaxDuration) || plan.Symbol != "BTC/USD" {
		return plan, errors.New("paper plan must be valid BTC/USD with 0 < deadline_ns <= 24h")
	}
	if plan.BaseBalance.Cmp(initialBase) > 0 || plan.QuoteBudget.Cmp(initialQuote) > 0 {
		return plan, errors.New("plan base_balance or quote_budget exceeds explicit synthetic initial balances")
	}
	return plan, nil
}

// paperTape accepts the actual `normalize` wire format: exact string level pairs,
// validity flags and integer receipt-relative nanoseconds. No time acceleration.
func paperTape(path string, maxBytes int64, maxEvents, maxLine int) (paperRecording, error) {
	body, err := paperReadFile(path, maxBytes)
	if err != nil {
		return paperRecording{}, err
	}
	return paperTapeBytes(body, maxBytes, maxEvents, maxLine)
}

func paperTapeBytes(body []byte, maxBytes int64, maxEvents, maxLine int) (paperRecording, error) {
	if int64(len(body)) > maxBytes {
		return paperRecording{}, errors.New("paper recording exceeds its byte limit")
	}
	var err error
	sum := sha256.Sum256(body)
	tape := paperRecording{digest: hex.EncodeToString(sum[:]), bytes: int64(len(body))}
	scan := bufio.NewScanner(bytes.NewReader(body))
	scan.Buffer(make([]byte, min(maxLine+1, 64<<10)), maxLine+1)
	var previousTime int64
	book := x.NewBook()
	line := 0
	for scan.Scan() {
		line++
		if len(scan.Bytes()) > maxLine {
			return tape, fmt.Errorf("event line %d exceeds --max-line-bytes", line)
		}
		if len(bytes.TrimSpace(scan.Bytes())) == 0 {
			return tape, fmt.Errorf("event line %d is empty", line)
		}
		if len(tape.events) >= maxEvents {
			return tape, errors.New("recording exceeds --max-events")
		}
		var wire marketdata.Event
		if err := paperDecode(scan.Bytes(), &wire); err != nil {
			return tape, fmt.Errorf("invalid normalized event at line %d: %w", line, err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(scan.Bytes(), &fields); err != nil {
			return tape, fmt.Errorf("invalid event object at line %d", line)
		}
		for _, required := range []string{"time_ns", "receipt_index", "valid"} {
			if value, ok := fields[required]; !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
				return tape, fmt.Errorf("event line %d requires %s", line, required)
			}
		}
		if wire.Time < 0 || wire.Time > int64(paperMaxDuration) || len(tape.events) > 0 && wire.Time < previousTime || wire.ReceiptIndex != uint64(len(tape.events)) {
			return tape, fmt.Errorf("event line %d has negative, out-of-order, excessive time or noncontiguous receipt identity", line)
		}
		if wire.Symbol != "BTC/USD" || wire.Type != "" && wire.Type != wire.Kind || len(wire.TradeID) > 128 {
			return tape, fmt.Errorf("event line %d has unsupported symbol or inconsistent identity", line)
		}
		event := d.MarketEvent{Time: wire.Time, Kind: wire.Kind, Symbol: wire.Symbol, ReceiptIndex: wire.ReceiptIndex, Invalid: !wire.Valid, Side: wire.Side, TradeID: wire.TradeID}
		if len(wire.Bids)+len(wire.Asks) > 512 {
			return tape, fmt.Errorf("event line %d exceeds 512 levels", line)
		}
		switch wire.Kind {
		case "snapshot", "book":
			if wire.Side != "" || wire.Price != "" || wire.Quantity != "" || wire.TradeID != "" {
				return tape, fmt.Errorf("book event line %d contains trade fields", line)
			}
			if event.Bids, err = paperLevels(wire.Bids); err == nil {
				event.Asks, err = paperLevels(wire.Asks)
			}
		case "trade":
			if len(wire.Bids)+len(wire.Asks) != 0 || wire.Side != "buy" && wire.Side != "sell" {
				return tape, fmt.Errorf("invalid trade at line %d", line)
			}
			event.Price, err = d.ParseDecimal(wire.Price)
			if err == nil {
				event.Quantity, err = d.ParseDecimal(wire.Quantity)
			}
			if err == nil && (event.Price.Sign() <= 0 || event.Quantity.Sign() <= 0) {
				err = errors.New("trade price and quantity must be positive")
			}
		case "gap":
			event.Invalid = true
			if len(wire.Bids)+len(wire.Asks) != 0 || wire.Side != "" || wire.Price != "" || wire.Quantity != "" || wire.TradeID != "" {
				err = errors.New("gap contains market payload")
			}
		default:
			err = errors.New("unsupported event kind")
		}
		if err != nil {
			return tape, fmt.Errorf("invalid event line %d: %w", line, err)
		}
		if err := book.Apply(event); err != nil {
			return tape, fmt.Errorf("invalid book at line %d: %w", line, err)
		}
		if len(book.Bids)+len(book.Asks) > 2048 {
			return tape, fmt.Errorf("accumulated book exceeds 2048 levels at line %d", line)
		}
		// An explicitly invalid/gap event is delivered to persist the recovery
		// gate, then terminates the session. A falsely valid crossed/empty book
		// is rejected before creating any synthetic account.
		if wire.Valid && wire.Kind != "gap" && !book.Valid {
			return tape, fmt.Errorf("valid event at line %d lacks a valid preceding snapshot/book", line)
		}
		tape.events = append(tape.events, event)
		previousTime = wire.Time
	}
	if err := scan.Err(); err != nil {
		return tape, errors.New("recording contains a line exceeding --max-line-bytes")
	}
	if len(tape.events) == 0 {
		return tape, errors.New("paper recording contains no events")
	}
	return tape, nil
}

func paperArchiveTape(ctx context.Context, root, id string, maxBytes int64, maxEvents, maxLine int) (paperRecording, error) {
	recording, err := marketarchive.Load(ctx, root, id)
	if err != nil {
		return paperRecording{}, err
	}
	if recording.Manifest.Normalized.Bytes > maxBytes || len(recording.Events) > maxEvents {
		return paperRecording{}, errors.New("verified recording exceeds paper input bounds")
	}
	sealed, err := rt.SealedSourceFromManifest(recording.Manifest)
	if err != nil {
		return paperRecording{}, err
	}
	var body bytes.Buffer
	cursors := make([]rt.SourceCursor, 0, len(recording.Events))
	for _, wire := range recording.Events {
		if err := ctx.Err(); err != nil {
			return paperRecording{}, err
		}
		line, err := json.Marshal(wire)
		if err != nil {
			return paperRecording{}, err
		}
		if len(line) > maxLine || int64(body.Len()+len(line)+1) > maxBytes {
			return paperRecording{}, errors.New("verified recording exceeds paper line or byte bounds")
		}
		body.Write(line)
		body.WriteByte('\n')
		cursors = append(cursors, rt.SourceCursor{ArchiveID: sealed.ArchiveID, NativeSessionID: sealed.NativeSessionID, NormalizedReceiptIndex: wire.ReceiptIndex, RawReceiptIndex: wire.SourceReceiptIndex, NormalizedTimeNS: wire.Time, NativeElapsedNS: sealed.FirstSnapshotElapsedNS + wire.Time})
	}
	tape, err := paperTapeBytes(body.Bytes(), maxBytes, maxEvents, maxLine)
	if err != nil {
		return paperRecording{}, err
	}
	if tape.digest != recording.Manifest.Normalized.SHA256 || tape.bytes != recording.Manifest.Normalized.Bytes {
		return paperRecording{}, errors.New("verified canonical recording changed during paper conversion")
	}
	tape.sealedSource, tape.sourceCursors = sealed, cursors
	return tape, nil
}

func paperLevels(wire [][]string) ([]d.Level, error) {
	levels := make([]d.Level, 0, len(wire))
	seen := make(map[string]bool, len(wire))
	for _, pair := range wire {
		if len(pair) != 2 {
			return nil, errors.New("each level must contain exactly two decimal strings")
		}
		price, err := d.ParseDecimal(pair[0])
		if err != nil || price.Sign() <= 0 {
			return nil, errors.New("level price must be a positive exact decimal")
		}
		quantity, err := d.ParseDecimal(pair[1])
		if err != nil || quantity.Sign() < 0 || seen[price.String()] {
			return nil, errors.New("level quantity must be nonnegative and level prices unique per side")
		}
		seen[price.String()] = true
		levels = append(levels, d.Level{Price: price, Quantity: quantity})
	}
	return levels, nil
}

// Never return a database driver's connection text: it may contain a DSN.
func paperSafeError(stage string, err error) error {
	kind := "operation failed; connection details withheld"
	for _, item := range []struct {
		err  error
		name string
	}{
		{rt.ErrOutcomeUnknown, "outcome unknown; retain account and command identities for operator recovery"},
		{pg.ErrOwnership, "ownership or lease lost; operator recovery required"},
		{pg.ErrIsolationRequired, "prior sender isolation and reconciliation required"},
		{pg.ErrConflict, "durable identity conflict"},
		{pg.ErrStaleRevision, "stale revision"},
		{pg.ErrInsufficientFunds, "insufficient synthetic funds"},
		{rt.ErrRecoveryRequired, "recovery required"},
		{rt.ErrHistoryLimit, "bounded runtime history exhausted"},
		{pg.ErrInboxFull, "bounded durable observation inbox exhausted"},
		{context.DeadlineExceeded, "operation deadline exceeded; accepted outcome may be unknown"},
	} {
		if errors.Is(err, item.err) {
			kind = item.name
			break
		}
	}
	return fmt.Errorf("%s: %s", stage, kind)
}

func paperOperation() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 7*time.Second)
}

func runPaper(ctx context.Context, opt paperOptions, dsn string, stdout, stderr io.Writer) error {
	if err := opt.validate(); err != nil {
		return err
	}
	plan, err := paperPlan(opt.planPath, opt.initialBase, opt.initialQuote)
	if err != nil {
		return err
	}
	var tape paperRecording
	if opt.archiveRoot != "" {
		tape, err = paperArchiveTape(ctx, opt.archiveRoot, opt.datasetID, opt.maxBytes, opt.maxEvents, opt.maxLineBytes)
	} else {
		tape, err = paperTape(opt.eventsPath, opt.maxBytes, opt.maxEvents, opt.maxLineBytes)
	}
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return errors.New("paper session interrupted before account creation")
	}
	if opt.account == "" {
		opt.account = "recorded-" + tape.digest[:24]
	}
	key := pg.Key{TenantID: "synthetic-paper-cli", AccountID: opt.account}
	initial := []pg.Balance{
		{Currency: "BTC", Total: opt.initialBase, Free: opt.initialBase},
		{Currency: "USD", Total: opt.initialQuote, Free: opt.initialQuote},
	}
	op, cancel := paperOperation()
	store, err := pg.Open(op, dsn, opt.schema)
	cancel()
	if err != nil {
		return paperSafeError("open isolated paper database", err)
	}
	defer store.Close()
	op, cancel = paperOperation()
	err = store.Migrate(op)
	cancel()
	if err != nil {
		return paperSafeError("migrate isolated paper database", err)
	}
	op, cancel = paperOperation()
	_, err = store.CreateAccount(op, key, initial, json.RawMessage(`{}`))
	cancel()
	if err != nil {
		if errors.Is(err, pg.ErrConflict) {
			return errors.New("synthetic paper account already exists; refusing to reset or claim it: choose a new --account or isolated --schema; existing accounts require explicit operator recovery")
		}
		return paperSafeError("create new synthetic paper account", err)
	}
	fmt.Fprintln(stderr, "PAPER / RECORDED DATA: synthetic BTC/USD account, synthetic rules and fees; public data is not independently authenticated.")
	summary := paperSummary{
		SealedSource: tape.sealedSource,
		Profile:      "paper", Source: "recorded-public-data", Simulation: "synthetic balances, rules, fees and fills; no exchange orders",
		SessionID: "recording-" + tape.digest, RecordingSHA256: tape.digest, RecordingBytes: tape.bytes,
		Schema: opt.schema, Policy: opt.policy, InitialBalances: initial,
		EventsAvailable: len(tape.events), StopReason: "recording_end", Fills: []d.Fill{},
		CommandReceipts: []paperCommandReceipt{}, Journal: []paperJournalCount{},
	}
	config := rt.DefaultConfig(store, key)
	config.DataSource = "recorded"
	config.SealedSource = tape.sealedSource
	config.AutoExecute, config.Baseline = true, opt.policy
	op, cancel = paperOperation()
	actor, err := rt.New(op, config)
	cancel()
	if err != nil {
		summary.StopReason, summary.Error = "startup_failed", paperSafeError("start durable paper actor", err).Error()
		return finishPaper(store, nil, key, summary, stdout)
	}
	summary.StartedAt = time.Now().UTC()
	runCtx, stop := context.WithTimeout(ctx, opt.duration)
	err = feedPaper(runCtx, actor, tape, plan, &summary)
	stop()
	if err != nil {
		summary.Error = err.Error()
	}
	return finishPaper(store, actor, key, summary, stdout)
}

func feedPaper(ctx context.Context, actor *rt.Actor, tape paperRecording, plan d.Plan, summary *paperSummary) error {
	anchor := time.Now()
	firstTime := tape.events[0].Time
	started := false
	for index, event := range tape.events {
		wait := time.Until(anchor.Add(time.Duration(event.Time - firstTime)))
		if wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				return paperStop(ctx, summary)
			}
		}
		if ctx.Err() != nil {
			return paperStop(ctx, summary)
		}
		id := fmt.Sprintf("%s-event-%d", summary.SessionID, event.ReceiptIndex)
		op, cancel := paperOperation()
		var err error
		if tape.sealedSource != nil {
			err = actor.MarketFromSource(op, event, id, tape.sourceCursors[index])
		} else {
			err = actor.MarketFromSession(op, event, id, summary.SessionID)
		}
		cancel()
		if err != nil {
			summary.StopReason = "market_ingress_failed"
			return paperSafeError("recorded market ingress", err)
		}
		summary.EventsDelivered++
		if event.Kind == "gap" || event.Invalid {
			summary.StopReason = "feed_gap"
			return errors.New("recorded feed gap or invalid event; paper session stopped with recovery required")
		}
		if !started && event.Kind == "snapshot" {
			op, cancel = paperOperation()
			view, err := actor.View(op)
			cancel()
			if err != nil {
				return paperSafeError("view initial paper snapshot", err)
			}
			if !view.State.Book.Valid {
				summary.StopReason = "invalid_snapshot"
				return errors.New("initial recorded snapshot is invalid")
			}
			summary.ReconciliationEvidence = "SIMULATED:new account; no exchange verification; sha256:" + tape.digest
			op, cancel = paperOperation()
			_, err = actor.Reconcile(op, rt.Reconciliation{
				ID: "cli-reconcile-" + tape.digest[:24], ExpectedRevision: view.Account.Revision,
				Evidence: summary.ReconciliationEvidence, NoExternalOrders: true, Balances: summary.InitialBalances,
			})
			cancel()
			if err != nil {
				summary.StopReason = "reconciliation_failed"
				return paperSafeError("explicit simulated reconciliation", err)
			}
			for _, command := range []rt.Command{
				{ID: "cli-create-" + tape.digest[:24], Kind: "create", PlanID: "cli-plan-" + tape.digest[:24], Plan: &plan, Policy: summary.Policy},
				{ID: "cli-start-" + tape.digest[:24], Kind: "start"},
			} {
				if err := paperDo(actor, command, summary); err != nil {
					summary.StopReason = "plan_command_failed"
					return err
				}
			}
			started = true
		}
		// Check the authority after every delivered event. Runtime uncertainty,
		// lease loss and rejected facts are terminal for this bounded CLI.
		op, cancel = paperOperation()
		view, err := actor.View(op)
		cancel()
		if err != nil {
			summary.StopReason = "authority_failed"
			return paperSafeError("inspect paper account authority", err)
		}
		if view.State.RecoveryRequired || !view.Account.Ready && started {
			summary.StopReason = "recovery_required"
			return errors.New("paper authority requires recovery; session stopped")
		}
	}
	if !started {
		summary.StopReason = "no_valid_snapshot"
		return errors.New("recording ended without a valid initial snapshot; no plan started")
	}
	return nil
}

func paperStop(ctx context.Context, summary *paperSummary) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		summary.StopReason = "duration_limit"
		return nil
	}
	summary.StopReason = "signal_or_context_cancelled"
	return nil
}

func paperDo(actor *rt.Actor, command rt.Command, summary *paperSummary) error {
	op, cancel := paperOperation()
	view, err := actor.View(op)
	cancel()
	if err != nil {
		return paperSafeError("view for "+command.Kind, err)
	}
	command.ExpectedRevision = view.Account.Revision
	op, cancel = paperOperation()
	result, err := actor.Do(op, command)
	cancel()
	receipt := paperCommandReceipt{Command: command, Result: result, Outcome: "committed"}
	if err != nil {
		receipt.Outcome = paperSafeError(command.Kind, err).Error()
	}
	summary.CommandReceipts = append(summary.CommandReceipts, receipt)
	if err != nil {
		return paperSafeError("paper "+command.Kind, err)
	}
	return nil
}

func finishPaper(store *pg.Store, actor *rt.Actor, key pg.Key, summary paperSummary, stdout io.Writer) error {
	if actor != nil {
		// Refresh/retry only proven stale revisions. An uncertain accepted command
		// keeps its original identity in the summary and requires operator recovery.
		for attempt := 0; attempt < 32; attempt++ {
			op, cancel := paperOperation()
			view, err := actor.View(op)
			cancel()
			if err != nil {
				summary.Error = paperJoinError(summary.Error, paperSafeError("shutdown view", err))
				break
			}
			parent := view.State.Parent
			if parent == nil || parent.Status == "completed" || parent.Status == "cancelled" || parent.Status == "expired" {
				break
			}
			command := rt.Command{ID: fmt.Sprintf("cli-cancel-%s-%d", summary.RecordingSHA256[:24], view.Account.Revision), Kind: "cancel", ExpectedRevision: view.Account.Revision}
			op, cancel = paperOperation()
			result, err := actor.Do(op, command)
			cancel()
			if errors.Is(err, pg.ErrStaleRevision) {
				continue
			}
			receipt := paperCommandReceipt{Command: command, Result: result, Outcome: "committed"}
			if err != nil {
				receipt.Outcome = paperSafeError("cancel", err).Error()
				summary.Error = paperJoinError(summary.Error, paperSafeError("cancel and drain paper plan", err))
			}
			summary.CommandReceipts = append(summary.CommandReceipts, receipt)
			break
		}
		op, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := actor.Close(op)
		cancel()
		summary.ActorClosed = err == nil
		if err != nil {
			summary.Error = paperJoinError(summary.Error, errors.New("actor close deadline exceeded; accepted outcomes may remain unknown"))
		}
	}
	op, cancel := paperOperation()
	account, err := store.GetAccount(op, key)
	cancel()
	if err != nil {
		summary.Error = paperJoinError(summary.Error, paperSafeError("final durable account read", err))
	} else {
		summary.Account = account
		if state, err := rt.DecodeSnapshot(account.Snapshot); err != nil {
			summary.Error = paperJoinError(summary.Error, errors.New("final durable account state could not be decoded"))
		} else {
			summary.State = state
		}
		summary.Fills = summary.State.Fills
		if summary.Fills == nil {
			summary.Fills = []d.Fill{}
		}
		terminal := summary.State.Parent == nil || summary.State.Parent.Status == "completed" || summary.State.Parent.Status == "cancelled" || summary.State.Parent.Status == "expired"
		summary.KnownFinal = summary.ActorClosed && terminal && len(account.Intents) == 0 && len(account.Reservations) == 0 && len(summary.State.PendingFacts) == 0 && len(summary.State.UnresolvedFacts) == 0 && account.ObservedSequence == account.ProcessedSequence && summary.Error == ""
	}
	if err := paperJournal(store, key, &summary); err != nil {
		summary.Error = paperJoinError(summary.Error, err)
		summary.KnownFinal = false
	}
	summary.EndedAt = time.Now().UTC()
	if err := json.NewEncoder(stdout).Encode(summary); err != nil {
		return err
	}
	if summary.Error != "" {
		return errors.New(summary.Error)
	}
	if !summary.KnownFinal {
		return errors.New("paper shutdown is not known final; retain this account for explicit operator recovery")
	}
	return nil
}

func paperJoinError(previous string, err error) string {
	if previous == "" {
		return err.Error()
	}
	return previous + "; " + err.Error()
}

func paperJournal(store *pg.Store, key pg.Key, summary *paperSummary) error {
	var after int64
	indices := map[string]int{}
	// Rows are metadata only. A bounded CLI session cannot exceed this cap at
	// the default one-second decision cadence and the 10,000-event input cap.
	for page := 0; page < 1000; page++ {
		op, cancel := paperOperation()
		entries, err := store.Journal(op, key, after, 1000)
		cancel()
		if err != nil {
			return paperSafeError("read account journal summary", err)
		}
		for _, entry := range entries {
			if entry.Revision <= after {
				return errors.New("journal metadata did not advance")
			}
			index, ok := indices[entry.Kind]
			if !ok {
				index = len(summary.Journal)
				indices[entry.Kind] = index
				summary.Journal = append(summary.Journal, paperJournalCount{Kind: entry.Kind, FirstRevision: entry.Revision})
			}
			row := &summary.Journal[index]
			row.Entries++
			row.LastRevision = entry.Revision
			summary.JournalEntries++
			after = entry.Revision
		}
		if len(entries) < 1000 {
			return nil
		}
	}
	return errors.New("journal summary exceeded bounded metadata scan; summary is incomplete")
}
