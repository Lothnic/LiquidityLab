package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/liquiditylab/liquiditylab/internal/marketdata"
	rt "github.com/liquiditylab/liquiditylab/internal/runtime"
	pg "github.com/liquiditylab/liquiditylab/internal/store/postgres"
)

func TestPaperServerErrorRetainsSafeClassification(t *testing.T) {
	const secret = "postgres://secret-user:secret-password@private-host/database"
	err := paperServerError("paper public market ingress", fmt.Errorf("%w: %s", pg.ErrOwnership, secret))
	if !errors.Is(err, pg.ErrOwnership) {
		t.Fatal("sanitized ingress error lost ownership classification")
	}
	if strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "ownership or lease lost") {
		t.Fatal("sanitized ingress error leaked private details or lost safe reason")
	}
}

func TestPaperServerErrorWithholdsUnknownSourceCredentials(t *testing.T) {
	err := paperServerError("source", errors.New("postgres://private:password@host/db token=private-source-token"))
	if err.Error() != "source: operation failed; connection details withheld" {
		t.Fatal("unknown source error exposed non-allowlisted details")
	}
}

func TestPaperServerErrorClassifiesRecordingBudgetWithoutPrivateDetails(t *testing.T) {
	err := paperServerError("source", fmt.Errorf("private-path token=secret: %w", marketdata.ErrCaptureStorageLimit))
	if !errors.Is(err, marketdata.ErrCaptureStorageLimit) || err.Error() != "source: configured recording byte budget exhausted" {
		t.Fatalf("storage classification not safely retained: %v", err)
	}
}

func TestPaperServerErrorPrioritizesAuthorityOverRecordingBudget(t *testing.T) {
	err := paperServerError("source", errors.Join(pg.ErrOwnership, marketdata.ErrCaptureStorageLimit))
	if !errors.Is(err, pg.ErrOwnership) || !errors.Is(err, marketdata.ErrCaptureStorageLimit) ||
		err.Error() != "source: ownership or lease lost; operator recovery required" {
		t.Fatalf("joined source failure lost authority priority: %v", err)
	}
}

func TestPaperDaemonOwnershipLossPreservesIngressClassification(t *testing.T) {
	opt := daemonOptions(t)
	dsn := daemonPostgres(t, &opt)
	var log bytes.Buffer
	static, err := paperStatic(opt.webDir)
	if err != nil {
		t.Fatal(err)
	}
	callbackError := make(chan error, 1)
	source := func(ctx context.Context, deliver func(context.Context, marketdata.Event) error) error {
		if err := deliver(ctx, daemonSnapshot()); err != nil {
			return err
		}
		store, err := pg.Open(ctx, dsn, opt.schema)
		if err != nil {
			return err
		}
		defer store.Close()
		// Fence only this test's randomized account through the real paper
		// ownership seam, then pass its next receipt through actual ingress.
		_, err = store.ClaimPaperOwnership(ctx, pg.Key{TenantID: paperDaemonTenant, AccountID: opt.account}, "controlled-replacement-owner", time.Minute)
		if err != nil {
			return err
		}
		event := marketdata.Event{Kind: "trade", Type: "trade", Symbol: "BTC/USD", Valid: true,
			ReceiptSessionID: "ownership-loss-session", ReceiptIndex: 1, Price: "100", Quantity: "0.1", Side: "sell"}
		err = deliver(ctx, event)
		callbackError <- err
		return err
	}
	daemon, err := newPaperDaemon(context.Background(), opt, dsn, daemonTestToken, static, &log, source)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := daemon.Close(); !errors.Is(err, pg.ErrOwnership) {
			t.Error("fenced daemon shutdown did not report ownership loss")
		}
	})
	select {
	case <-daemon.sourceDone:
	case <-time.After(5 * time.Second):
		t.Fatal("controlled ownership failure did not finish")
	}
	select {
	case err := <-callbackError:
		if !errors.Is(err, pg.ErrOwnership) {
			t.Fatalf("actual ingress lost ownership error identity: %v", err)
		}
	default:
		t.Fatal("test did not reach actual ingress after ownership fencing")
	}
	if !strings.Contains(log.String(), "source: ownership or lease lost") ||
		!strings.Contains(log.String(), "invalidation=unconfirmed") ||
		!strings.Contains(log.String(), "source invalidation: ownership or lease lost") {
		t.Fatalf("ownership diagnostics missing safe source/invalidation outcomes: %s", log.String())
	}
}

func TestPaperDaemonSourceFailureReportsSafeReasonAndInvalidation(t *testing.T) {
	opt := daemonOptions(t)
	dsn := daemonPostgres(t, &opt)
	var log bytes.Buffer
	static, err := paperStatic(opt.webDir)
	if err != nil {
		t.Fatal(err)
	}
	source := func(ctx context.Context, deliver func(context.Context, marketdata.Event) error) error {
		if err := deliver(ctx, daemonSnapshot()); err != nil {
			return err
		}
		return paperServerError("paper public market ingress", fmt.Errorf("%w: private-source-token", pg.ErrOwnership))
	}
	daemon, err := newPaperDaemon(context.Background(), opt, dsn, daemonTestToken, static, &log, source)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := daemon.Close(); err != nil {
			t.Error(err)
		}
	})
	select {
	case <-daemon.sourceDone:
	case <-time.After(5 * time.Second):
		t.Fatal("controlled source failure did not finish")
	}
	if !strings.Contains(log.String(), "ownership or lease lost") || !strings.Contains(log.String(), "invalidation=confirmed") {
		t.Fatalf("source failure did not expose safe causal class and invalidation outcome: %s", log.String())
	}
	if strings.Contains(log.String(), "private-source-token") {
		t.Fatal("source diagnostics leaked raw source error")
	}
	_, timestamp, found := strings.Cut(log.String(), "observed_at=")
	if !found {
		t.Fatal("source failure omitted observation time")
	}
	timestamp, _, _ = strings.Cut(timestamp, ";")
	if _, err := time.Parse(time.RFC3339Nano, timestamp); err != nil || !strings.HasSuffix(timestamp, "Z") {
		t.Fatal("source failure observation time must be RFC3339Nano UTC")
	}
	view := daemonView(t, daemon)
	if view.Account.Ready || view.State.Book.Valid || !view.State.RecoveryRequired {
		t.Fatal("controlled source failure did not quarantine paper readiness")
	}
	route := "/v1/accounts/" + daemon.key.TenantID + "/" + daemon.key.AccountID
	if response := daemonRequest(t, daemon.handler, http.MethodGet, route, daemonTestToken, nil); response.Code != http.StatusOK {
		t.Fatal("source failure made authenticated account inspection unavailable")
	}
	response := daemonRequest(t, daemon.handler, http.MethodPost, route+"/commands", daemonTestToken,
		map[string]any{"id": "source-failure-cancel", "kind": "cancel", "expected_revision": view.Account.Revision})
	// This fixture has no parent to cancel. Its control endpoint still reaches
	// the account validator rather than disappearing when the source ends.
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "invalid_command") {
		t.Fatalf("source failure control endpoint unavailable: %d %s", response.Code, response.Body.String())
	}
}

func TestPaperDaemonCanceledIngressLeavesDurableStateAndCursorsUntouched(t *testing.T) {
	opt := daemonOptions(t)
	dsn := daemonPostgres(t, &opt)
	source := func(ctx context.Context, deliver func(context.Context, marketdata.Event) error) error {
		if err := deliver(ctx, daemonSnapshot()); err != nil {
			return err
		}
		<-ctx.Done()
		return ctx.Err()
	}
	daemon := daemonNewTest(t, opt, dsn, source)
	before := daemonWait(t, daemon, func(view rt.View) bool { return view.Account.Ready })
	if !before.Account.Market.Valid || before.Account.ObservedSequence != before.Account.ProcessedSequence {
		t.Fatal("test fixture did not establish a ready, caught-up paper account")
	}

	ingressCtx, cancel := context.WithCancel(context.Background())
	cancel()
	event := marketdata.Event{Kind: "trade", Type: "trade", Symbol: "BTC/USD", Valid: true,
		ReceiptSessionID: "cancelled-ingress-session", ReceiptIndex: 1, Price: "100", Quantity: "0.1", Side: "sell"}
	if err := daemon.Ingest(ingressCtx, event); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled ingress error = %v, want context.Canceled", err)
	}

	after := daemonView(t, daemon)
	if after.Account.Revision != before.Account.Revision ||
		after.Account.ObservedSequence != before.Account.ObservedSequence ||
		after.Account.ProcessedSequence != before.Account.ProcessedSequence ||
		after.Account.Ready != before.Account.Ready || after.Account.Market != before.Account.Market ||
		!bytes.Equal(after.Account.Snapshot, before.Account.Snapshot) {
		t.Fatalf("cancelled ingress mutated durable account or source cursors: before revision/cursors/ready=%d/%d/%d/%t after=%d/%d/%d/%t",
			before.Account.Revision, before.Account.ObservedSequence, before.Account.ProcessedSequence, before.Account.Ready,
			after.Account.Revision, after.Account.ObservedSequence, after.Account.ProcessedSequence, after.Account.Ready)
	}
}

func TestPaperDaemonGracefulSourceCancellationClosesCallbackAndLeavesKnownFinalAccount(t *testing.T) {
	opt := daemonOptions(t)
	dsn := daemonPostgres(t, &opt)
	callbackResult := make(chan error, 1)
	sourceReady := make(chan struct{})
	source := func(ctx context.Context, deliver func(context.Context, marketdata.Event) error) error {
		if err := deliver(ctx, daemonSnapshot()); err != nil {
			return err
		}
		close(sourceReady)
		<-ctx.Done()
		// A well-behaved source should pass its cancelled context through. The
		// callback must reject this event without touching the actor.
		callbackResult <- deliver(ctx, marketdata.Event{Kind: "trade", Type: "trade", Symbol: "BTC/USD", Valid: true,
			ReceiptSessionID: "stopped-source-session", ReceiptIndex: 1, Price: "100", Quantity: "0.1", Side: "sell"})
		return ctx.Err()
	}
	var log bytes.Buffer
	static, err := paperStatic(opt.webDir)
	if err != nil {
		t.Fatal(err)
	}
	daemon, err := newPaperDaemon(context.Background(), opt, dsn, daemonTestToken, static, &log, source)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := daemon.Close(); err != nil {
			t.Error(err)
		}
	})
	select {
	case <-sourceReady:
	case <-time.After(5 * time.Second):
		t.Fatal("paper source did not establish its initial snapshot")
	}
	daemonWait(t, daemon, func(view rt.View) bool { return view.Account.Ready })
	if err := daemon.Close(); err != nil {
		t.Fatalf("graceful paper close: %v", err)
	}
	select {
	case err := <-callbackResult:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("post-cancellation source callback error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("source callback did not finish before graceful close returned")
	}
	select {
	case <-daemon.sourceDone:
	default:
		t.Fatal("source goroutine leaked past graceful close")
	}
	if !strings.Contains(log.String(), "known_final=true") {
		t.Fatalf("graceful close did not report a known-final durable account: %s", log.String())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	store, err := pg.Open(ctx, dsn, opt.schema)
	if err != nil {
		t.Fatal("reopen durable account after close")
	}
	defer store.Close()
	account, err := store.GetAccount(ctx, daemon.key)
	if err != nil {
		t.Fatal("read durable account after close")
	}
	state, err := rt.DecodeSnapshot(account.Snapshot)
	if err != nil || state.Parent != nil || len(account.Intents) != 0 || len(account.Reservations) != 0 ||
		len(state.PendingFacts) != 0 || len(state.UnresolvedFacts) != 0 || account.ObservedSequence != account.ProcessedSequence {
		t.Fatalf("graceful cancellation left account not known final: parent=%+v intents=%d reservations=%d pending=%d unresolved=%d cursors=%d/%d err=%v",
			state.Parent, len(account.Intents), len(account.Reservations), len(state.PendingFacts), len(state.UnresolvedFacts),
			account.ObservedSequence, account.ProcessedSequence, err)
	}
}

func TestPaperDaemonActualHandlerRejectsWritesOnceShutdownBegins(t *testing.T) {
	opt := daemonOptions(t)
	dsn := daemonPostgres(t, &opt)
	source := func(ctx context.Context, deliver func(context.Context, marketdata.Event) error) error {
		if err := deliver(ctx, daemonSnapshot()); err != nil {
			return err
		}
		<-ctx.Done()
		return ctx.Err()
	}
	daemon := daemonNewTest(t, opt, dsn, source)
	before := daemonWait(t, daemon, func(view rt.View) bool { return view.Account.Ready })
	daemon.closing.Store(true)
	route := "/v1/accounts/" + daemon.key.TenantID + "/" + daemon.key.AccountID + "/commands"
	response := daemonRequest(t, daemon.handler, http.MethodPost, route, daemonTestToken,
		map[string]any{"id": "shutdown-admission-check", "kind": "resume", "expected_revision": before.Account.Revision})
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "server_stopping") {
		t.Fatalf("actual handler admitted or misreported a write during shutdown: %d %s", response.Code, response.Body.String())
	}
	after := daemonView(t, daemon)
	if after.Account.Revision != before.Account.Revision || after.Account.ObservedSequence != before.Account.ObservedSequence ||
		after.Account.ProcessedSequence != before.Account.ProcessedSequence {
		t.Fatal("shutdown-rejected HTTP write mutated the paper account")
	}
}
