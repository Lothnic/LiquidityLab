package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/liquiditylab/liquiditylab/internal/marketdata"
	rt "github.com/liquiditylab/liquiditylab/internal/runtime"
	pg "github.com/liquiditylab/liquiditylab/internal/store/postgres"
)

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
