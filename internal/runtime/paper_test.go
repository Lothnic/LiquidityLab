package runtime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	d "github.com/liquiditylab/liquiditylab/internal/domain"
	x "github.com/liquiditylab/liquiditylab/internal/execution"
	pg "github.com/liquiditylab/liquiditylab/internal/store/postgres"
)

func paperTestStore(t *testing.T) (*pg.Store, pg.Key, pg.Ownership) {
	t.Helper()
	dsn := os.Getenv("LIQUIDITYLAB_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set LIQUIDITYLAB_TEST_POSTGRES_DSN for paper transport persistence tests")
	}
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatal(err)
	}
	schema := "ll_paper_test_" + hex.EncodeToString(suffix[:])
	ctx := context.Background()
	store, err := pg.Open(ctx, dsn, schema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		connection, err := pgx.Connect(context.Background(), dsn)
		if err != nil {
			t.Error(err)
		} else {
			_, err = connection.Exec(context.Background(), fmt.Sprintf("DROP SCHEMA %s CASCADE", pgx.Identifier{schema}.Sanitize()))
			if err != nil {
				t.Error(err)
			}
			connection.Close(context.Background())
		}
		store.Close()
	})
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	key := pg.Key{TenantID: "paper-tenant", AccountID: "paper-account"}
	if _, err := store.CreateAccount(ctx, key,
		[]pg.Balance{{Currency: "USD", Total: d.MustDecimal("1000"), Free: d.MustDecimal("1000")}},
		json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	account, err := store.ClaimPaperOwnership(ctx, key, "paper-transport-test-owner", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return store, key, account.Ownership
}

func paperTestBook(t *testing.T) *x.Book {
	t.Helper()
	book := x.NewBook()
	if err := book.Apply(d.MarketEvent{
		Kind: "snapshot",
		Bids: []d.Level{{Price: d.MustDecimal("99"), Quantity: d.MustDecimal("0.7")},
			{Price: d.MustDecimal("98"), Quantity: d.MustDecimal("2")}},
		Asks: []d.Level{{Price: d.MustDecimal("101"), Quantity: d.MustDecimal("1")},
			{Price: d.MustDecimal("102"), Quantity: d.MustDecimal("0.7")}},
	}); err != nil {
		t.Fatal(err)
	}
	if !book.Valid {
		t.Fatal("test book is invalid")
	}
	return book
}

func paperTestFees() d.FeeConfig {
	return d.FeeConfig{Maker: d.MustDecimal("0.001"), Taker: d.MustDecimal("0.002"), Currency: "quote", Source: "paper-test"}
}

func TestPaperTransportHandoffPersistsAtomicExactIOCOutcome(t *testing.T) {
	store, key, owner := paperTestStore(t)
	transport := newPaperTransport(store, key, owner)
	if transport.Profile() != ProfilePaper {
		t.Fatalf("paper transport profile=%q", transport.Profile())
	}
	book := paperTestBook(t)
	before := book.Available("asks", d.MustDecimal("101"))
	submission := Submission{
		ID: "child-ioc", OperationID: "dispatch-command-1", InputDigest: "sha256:input-1",
		Intent: d.OrderIntent{Side: "buy", Quantity: d.MustDecimal("1.5"), Price: d.MustDecimal("102")},
		Book:   book, Fees: paperTestFees(), Now: 123456789,
	}
	result, err := transport.Handoff(context.Background(), submission)
	if err != nil {
		t.Fatal(err)
	}
	if result.Handoff != PotentiallyInFlight || !result.Accepted || !result.KnownFinal || len(result.Fills) != 2 {
		t.Fatalf("IOC outcome: %+v", result)
	}
	want := []d.Fill{
		{ExecutionID: "paper-child-ioc-1", OrderID: "child-ioc", Side: "buy", Quantity: d.MustDecimal("1"), Price: d.MustDecimal("101"), Fee: d.MustDecimal("0.202"), FeeCurrency: "quote", Time: submission.Now, Maker: false},
		{ExecutionID: "paper-child-ioc-2", OrderID: "child-ioc", Side: "buy", Quantity: d.MustDecimal("0.5"), Price: d.MustDecimal("102"), Fee: d.MustDecimal("0.102"), FeeCurrency: "quote", Time: submission.Now, Maker: false},
	}
	for index := range want {
		if !result.Fills[index].Equal(want[index]) {
			t.Fatalf("fill %d got %+v want %+v", index, result.Fills[index], want[index])
		}
	}
	if !book.Available("asks", d.MustDecimal("101")).Equal(before) {
		t.Fatal("paper execution mutated the caller's book")
	}

	retry, err := transport.Handoff(context.Background(), submission)
	if err != nil || !reflect.DeepEqual(result, retry) {
		t.Fatalf("stable retry result=%+v err=%v", retry, err)
	}
	conflict := submission
	conflict.OperationID = "different-operation"
	if _, err = transport.Handoff(context.Background(), conflict); !errors.Is(err, pg.ErrConflict) {
		t.Fatalf("operation identity reuse got %v, want conflict", err)
	}
	conflict = submission
	conflict.InputDigest = "sha256:different-input"
	if _, err = transport.Handoff(context.Background(), conflict); !errors.Is(err, pg.ErrConflict) {
		t.Fatalf("input digest reuse got %v, want conflict", err)
	}

	fact, err := store.GetObservation(context.Background(), key, "paper-submit-child-ioc")
	if err != nil || fact.Kind != paperTransportKind {
		t.Fatalf("durable transport fact=%+v err=%v", fact, err)
	}
	var recorded transportFact
	if err = json.Unmarshal(fact.Payload, &recorded); err != nil {
		t.Fatal(err)
	}
	if recorded.OperationID != submission.OperationID || recorded.InputDigest != submission.InputDigest || !reflect.DeepEqual(recorded.Outcome, result) {
		t.Fatalf("stored result does not contain the whole handoff: %+v", recorded)
	}
	pending, err := store.PendingObservations(context.Background(), key, 10)
	if err != nil || len(pending) != 1 || pending[0].ID != "paper-submit-child-ioc" {
		t.Fatalf("paper handoff did not commit exactly one observation: %+v err=%v", pending, err)
	}
}

func TestPaperTransportPassivePostOnlyAndCancellationFacts(t *testing.T) {
	store, key, owner := paperTestStore(t)
	transport := newPaperTransport(store, key, owner)
	book := paperTestBook(t)
	passive := Submission{
		ID: "child-passive", OperationID: "dispatch-passive", InputDigest: "sha256:passive",
		Intent: d.OrderIntent{Side: "buy", Quantity: d.MustDecimal("0.5"), Price: d.MustDecimal("99"), Passive: true},
		Book:   book, Fees: paperTestFees(), Now: 10,
	}
	accepted, err := transport.Handoff(context.Background(), passive)
	if err != nil || !accepted.Accepted || accepted.KnownFinal || !accepted.QueueAhead.Equal(d.MustDecimal("0.7")) || len(accepted.Fills) != 0 {
		t.Fatalf("passive accepted result=%+v err=%v", accepted, err)
	}
	cancelled, err := transport.Cancel(context.Background(), passive.ID)
	if err != nil || !cancelled.Accepted || !cancelled.KnownFinal || len(cancelled.Fills) != 0 {
		t.Fatalf("paper cancel result=%+v err=%v", cancelled, err)
	}
	cancelRetry, err := transport.Cancel(context.Background(), passive.ID)
	if err != nil || !reflect.DeepEqual(cancelled, cancelRetry) {
		t.Fatalf("cancel retry result=%+v err=%v", cancelRetry, err)
	}
	// A prior submit retry returns its committed outcome even after a later cancel.
	passiveRetry, err := transport.Handoff(context.Background(), passive)
	if err != nil || !reflect.DeepEqual(accepted, passiveRetry) {
		t.Fatalf("submit retry after cancel result=%+v err=%v", passiveRetry, err)
	}
	cancelFact, err := store.GetObservation(context.Background(), key, "paper-cancel-child-passive")
	if err != nil {
		t.Fatal(err)
	}
	var recorded transportFact
	if err = json.Unmarshal(cancelFact.Payload, &recorded); err != nil {
		t.Fatal(err)
	}
	if recorded.OperationID != cancelFact.ID || recorded.InputDigest != "paper-cancel" || !recorded.Outcome.KnownFinal || len(recorded.Outcome.Fills) != 0 {
		t.Fatalf("cancel fact identity/outcome: %+v", recorded)
	}
	if _, err = store.GetObservation(context.Background(), key, "paper-submit-child-passive"); err != nil {
		t.Fatalf("passive acceptance fact missing: %v", err)
	}

	crossing := passive
	crossing.ID, crossing.OperationID, crossing.InputDigest = "child-cross", "dispatch-cross", "sha256:cross"
	crossing.Intent.Price = d.MustDecimal("101")
	rejected, err := transport.Handoff(context.Background(), crossing)
	if err != nil || rejected.Handoff != RejectedBeforeHandoff || rejected.Accepted || !rejected.KnownFinal || rejected.Reason != "post_only_would_cross" {
		t.Fatalf("crossing post-only result=%+v err=%v", rejected, err)
	}
	rejectedRetry, err := transport.Handoff(context.Background(), crossing)
	if err != nil || !reflect.DeepEqual(rejected, rejectedRetry) {
		t.Fatalf("crossing post-only retry=%+v err=%v", rejectedRetry, err)
	}
	if _, err = store.GetObservation(context.Background(), key, "paper-submit-child-cross"); err != nil {
		t.Fatalf("post-only rejection fact missing: %v", err)
	}

	preCancelled, err := transport.Cancel(context.Background(), "child-cancel-first")
	if err != nil || !preCancelled.KnownFinal {
		t.Fatalf("cancel-before-submit result=%+v err=%v", preCancelled, err)
	}
	blocked := passive
	blocked.ID, blocked.OperationID, blocked.InputDigest = "child-cancel-first", "dispatch-late", "sha256:late"
	blockedResult, err := transport.Handoff(context.Background(), blocked)
	if err != nil || blockedResult.Handoff != RejectedBeforeHandoff || blockedResult.Accepted || !blockedResult.KnownFinal || blockedResult.Reason != "paper_child_already_cancelled" {
		t.Fatalf("handoff after durable cancel result=%+v err=%v", blockedResult, err)
	}
	if _, err = store.GetObservation(context.Background(), key, "paper-submit-child-cancel-first"); !errors.Is(err, pg.ErrNotFound) {
		t.Fatalf("cancelled child unexpectedly recorded a submission: %v", err)
	}
}

func TestPaperTradeStableIdentity(t *testing.T) {
	trade := d.MarketEvent{Kind: "trade", Symbol: "BTC/USD", Time: 123, TradeID: "venue-1", ReceiptIndex: 5, Side: "sell", Price: d.MustDecimal("99"), Quantity: d.MustDecimal("0.9")}
	id, digest := paperTradeIdentity("session-1", trade)
	repeat := trade
	repeat.ReceiptIndex = 27
	repeat.Time = 456
	repeatedID, repeatedDigest := paperTradeIdentity("session-2", repeat)
	if id != repeatedID || digest != repeatedDigest || len(id)+len("-fill") > 128 {
		t.Fatal("venue trade identity changed with receipt/session or exceeded durable ID bound")
	}
	changed := trade
	changed.Quantity = d.One
	changedID, changedDigest := paperTradeIdentity("session-1", changed)
	if changedID != id || changedDigest == digest {
		t.Fatal("changed venue trade content must reuse the identity and conflict on digest")
	}
	trade.TradeID = ""
	id, _ = paperTradeIdentity("session-1", trade)
	otherSession, _ := paperTradeIdentity("session-2", trade)
	trade.ReceiptIndex++
	otherReceipt, _ := paperTradeIdentity("session-1", trade)
	if id == otherSession || id == otherReceipt {
		t.Fatal("receipt fallback failed to scope the source session and receipt index")
	}
}

func TestPaperTradePersistsQueueAndExactMakerFillBeforeReduction(t *testing.T) {
	store, key, owner := paperTestStore(t)
	transport := newPaperTransport(store, key, owner)
	child := &Child{ID: "child-maker", Order: d.ChildOrder{
		OrderID: "child-maker", State: "cancel_requested", Remaining: d.MustDecimal("0.5"), QueueAhead: d.MustDecimal("0.7"),
		Intent: d.OrderIntent{Side: "buy", Price: d.MustDecimal("99"), Quantity: d.MustDecimal("0.5"), Passive: true},
	}}
	trade := d.MarketEvent{Kind: "trade", Symbol: "BTC/USD", Time: 123, TradeID: "maker-trade-1", ReceiptIndex: 5, Side: "sell", Price: d.MustDecimal("99"), Quantity: d.MustDecimal("0.9")}
	fact, err := transport.ObserveTrade(context.Background(), "session-1", trade, child, paperTestFees())
	if err != nil {
		t.Fatal(err)
	}
	if fact.IntentID != child.ID || !fact.QueueAhead.IsZero() || len(fact.Fills) != 1 {
		t.Fatalf("unexpected maker trade fact: %+v", fact)
	}
	want := d.Fill{ExecutionID: fact.ID + "-fill", OrderID: child.ID, Side: "buy", Quantity: d.MustDecimal("0.2"), Price: d.MustDecimal("99"), Fee: d.MustDecimal("0.0198"), FeeCurrency: "quote", Time: 123, Maker: true}
	if !fact.Fills[0].Equal(want) || !child.Order.QueueAhead.Equal(d.MustDecimal("0.7")) || !child.Order.Remaining.Equal(d.MustDecimal("0.5")) {
		t.Fatalf("maker fill must be exact and leave state untouched: fact=%+v child=%+v", fact, child)
	}
	stored, err := store.GetObservation(context.Background(), key, fact.ID)
	if err != nil || stored.Kind != paperTradeKind {
		t.Fatalf("maker source result not durable: %+v %v", stored, err)
	}
	// Account projections and execution identity are reduced by the actor only.
	if _, err := store.GetExecution(context.Background(), key, "paper", want.ExecutionID); !errors.Is(err, pg.ErrNotFound) {
		t.Fatalf("source helper booked authoritative execution: %v", err)
	}
	child.ID, child.Order.OrderID = "later-child", "later-child"
	child.Order.QueueAhead = d.Zero
	trade.ReceiptIndex++
	trade.Time++
	retry, err := newPaperTransport(store, key, owner).ObserveTrade(context.Background(), "reconnected-session", trade, child, paperTestFees())
	if err != nil || !reflect.DeepEqual(retry, fact) {
		t.Fatalf("restart duplicate rematched against later child: %+v err=%v", retry, err)
	}
	trade.Quantity = d.One
	if _, err := transport.ObserveTrade(context.Background(), "session-1", trade, child, paperTestFees()); !errors.Is(err, pg.ErrConflict) {
		t.Fatalf("changed source payload should conflict: %v", err)
	}
}

func TestPaperTradeIgnoredAndQueueOnlyObservationsDeduplicate(t *testing.T) {
	store, key, owner := paperTestStore(t)
	transport := newPaperTransport(store, key, owner)
	trade := d.MarketEvent{Kind: "trade", Symbol: "BTC/USD", Time: 123, ReceiptIndex: 5, Side: "sell", Price: d.MustDecimal("99"), Quantity: d.MustDecimal("0.4")}
	ignored, err := transport.ObserveTrade(context.Background(), "session-1", trade, nil, paperTestFees())
	if err != nil || ignored.IntentID != "" || len(ignored.Fills) != 0 {
		t.Fatalf("ignored source trade result=%+v err=%v", ignored, err)
	}
	child := &Child{ID: "child-queue", Order: d.ChildOrder{
		OrderID: "child-queue", State: "open", Remaining: d.MustDecimal("0.5"), QueueAhead: d.MustDecimal("0.7"),
		Intent: d.OrderIntent{Side: "buy", Price: d.MustDecimal("99"), Quantity: d.MustDecimal("0.5"), Passive: true},
	}}
	trade.Time++
	retry, err := transport.ObserveTrade(context.Background(), "session-1", trade, child, paperTestFees())
	if err != nil || !reflect.DeepEqual(retry, ignored) {
		t.Fatalf("previously ignored trade rematched: %+v %v", retry, err)
	}
	// A new identity cannot freeze an outcome against unreduced predecessor
	// state. Transport-only fixtures consume that ignored predecessor explicitly.
	account, err := store.GetAccount(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Apply(context.Background(), pg.Change{Key: key, Ownership: owner, Command: pg.Command{ID: "consume-ignored-trade", Kind: "reduce", ExpectedRevision: account.Revision}, Snapshot: account.Snapshot, ConsumeObservationThrough: account.ObservedSequence})
	if err != nil {
		t.Fatal(err)
	}
	trade.ReceiptIndex++
	queued, err := transport.ObserveTrade(context.Background(), "session-1", trade, child, paperTestFees())
	if err != nil || queued.IntentID != child.ID || !queued.QueueAhead.Equal(d.MustDecimal("0.3")) || len(queued.Fills) != 0 {
		t.Fatalf("queue-only source trade result=%+v err=%v", queued, err)
	}
	pending, err := store.PendingObservations(context.Background(), key, 10)
	if err != nil || len(pending) != 1 {
		t.Fatalf("duplicate trade added observations: %+v %v", pending, err)
	}
}
