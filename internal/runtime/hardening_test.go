package runtime

import (
	"errors"
	"reflect"
	"testing"
	"time"

	d "github.com/liquiditylab/liquiditylab/internal/domain"
)

func TestActorReservedPublicIDsAreRejectedBeforeStateChange(t *testing.T) {
	_, _, _, actor := actorTestReady(t, 0)
	before := actorTestView(t, actor)
	prefixes := []string{
		"boot-", "deadline-", "dispatch-market-", "market-invalid-", "clock-invalid-",
		"observation-", "paper-", "transport-", "runtime-",
	}
	for _, prefix := range prefixes {
		t.Run(prefix, func(t *testing.T) {
			id := prefix + "public-test"
			ctx, cancel := actorTestContext(t)
			_, err := actor.Do(ctx, Command{ID: id, Kind: "pause"})
			cancel()
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("Do with reserved ID %q error=%v, want ErrInvalid", id, err)
			}

			ctx, cancel = actorTestContext(t)
			_, err = actor.Reconcile(ctx, Reconciliation{ID: id})
			cancel()
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("Reconcile with reserved ID %q error=%v, want ErrInvalid", id, err)
			}
		})
	}
	after := actorTestView(t, actor)
	if !reflect.DeepEqual(before.Account, after.Account) || !reflect.DeepEqual(before.State, after.State) {
		t.Fatalf("reserved public IDs changed actor state:\nbefore=%+v\nafter=%+v", before, after)
	}
}

func TestActorInvalidBookInputDurablyInvalidatesReadiness(t *testing.T) {
	store, key, _, actor := actorTestReady(t, 0)
	ctx, cancel := actorTestContext(t)
	err := actor.Market(ctx, d.MarketEvent{
		Time:         time.Now().UnixNano(),
		Kind:         "book",
		Symbol:       "BTC/USD",
		ReceiptIndex: 2,
		Bids:         []d.Level{{Price: d.MustDecimal("0"), Quantity: d.MustDecimal("1")}},
	}, "invalid-book-input")
	cancel()
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("Market with invalid level error=%v, want ErrInvalid", err)
	}

	dbCtx, dbCancel := actorTestContext(t)
	defer dbCancel()
	account, err := store.GetAccount(dbCtx, key)
	if err != nil {
		t.Fatal(err)
	}
	if account.Ready || account.Market.Valid {
		t.Fatalf("invalid market was not durably quarantined: ready=%t market=%+v", account.Ready, account.Market)
	}
	view := actorTestView(t, actor)
	if view.Account.Ready || view.State.Book.Valid || !view.State.RecoveryRequired || view.State.Reason != "invalid_market_event" {
		t.Fatalf("actor view did not retain invalid-market quarantine: ready=%t book=%+v recovery=%t reason=%q", view.Account.Ready, view.State.Book, view.State.RecoveryRequired, view.State.Reason)
	}
}

func TestActorBackwardClockRequiresFreshBookAndAbortForActiveParent(t *testing.T) {
	store, key, _, actor := actorTestReady(t, 0)
	actorTestCreateAndStart(t, actor, "plan-clock-backward")
	closeCtx, closeCancel := actorTestContext(t)
	if err := actor.Close(closeCtx); err != nil {
		closeCancel()
		t.Fatalf("stop actor before deterministic clock injection: %v", err)
	}
	closeCancel()

	previous := time.Now()
	actor.lastClock = previous
	actor.now = func() time.Time { return previous.Add(-time.Second) }
	ctx, cancel := actorTestContext(t)
	err := actor.checkClock(ctx)
	cancel()
	if !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("backward clock check error=%v, want ErrRecoveryRequired", err)
	}
	if !actor.state.ClockDiscontinuity || actor.state.Book.Valid || actor.account.Ready {
		t.Fatalf("clock discontinuity did not quarantine account: state=%+v ready=%t", actor.state, actor.account.Ready)
	}
	dbCtx, dbCancel := actorTestContext(t)
	account, err := store.GetAccount(dbCtx, key)
	dbCancel()
	if err != nil {
		t.Fatal(err)
	}
	if account.Ready || account.Market.Valid {
		t.Fatalf("clock quarantine was not durable: ready=%t market=%+v", account.Ready, account.Market)
	}

	makeReconciliation := func(id string, abort bool) Reconciliation {
		return Reconciliation{
			ID:               id,
			ExpectedRevision: actor.account.Revision,
			Evidence:         "test-operator-clock-reconciliation",
			NoExternalOrders: true,
			Balances:         actor.account.Balances,
			AbortParent:      abort,
		}
	}
	ctx, cancel = actorTestContext(t)
	_, err = actor.reconcile(ctx, makeReconciliation("reconcile-clock-no-book", true))
	cancel()
	if !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("reconcile before fresh book error=%v, want ErrRecoveryRequired", err)
	}

	ctx, cancel = actorTestContext(t)
	err = actor.market(ctx, marketInput{
		SnapshotID: "snapshot-after-clock-backward",
		Event: d.MarketEvent{
			Time:         time.Now().UnixNano(),
			Kind:         "snapshot",
			Symbol:       "BTC/USD",
			ReceiptIndex: 1,
			Bids:         []d.Level{{Price: d.MustDecimal("99"), Quantity: d.MustDecimal("2")}},
			Asks:         []d.Level{{Price: d.MustDecimal("101"), Quantity: d.MustDecimal("2")}},
		},
	})
	cancel()
	if err != nil {
		t.Fatalf("apply fresh post-clock book: %v", err)
	}
	if !actor.state.Book.Valid || actor.account.Ready {
		t.Fatalf("fresh book should restore market validity but keep account quarantined: book=%+v ready=%t", actor.state.Book, actor.account.Ready)
	}

	ctx, cancel = actorTestContext(t)
	_, err = actor.reconcile(ctx, makeReconciliation("reconcile-clock-active-parent", false))
	cancel()
	if !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("active-parent reconcile without abort error=%v, want ErrRecoveryRequired", err)
	}

	ctx, cancel = actorTestContext(t)
	_, err = actor.reconcile(ctx, makeReconciliation("reconcile-clock-abort-parent", true))
	cancel()
	if err != nil {
		t.Fatalf("reconcile with explicit active-parent abort: %v", err)
	}
	if !actor.account.Ready || !actor.state.Book.Valid || actor.state.ClockDiscontinuity || actor.state.RecoveryRequired {
		t.Fatalf("explicit clock recovery did not restore ready state: account=%+v state=%+v", actor.account, actor.state)
	}
	if actor.state.Parent == nil || actor.state.Parent.Status != "cancelled" {
		t.Fatalf("explicit AbortParent did not terminate active parent: %+v", actor.state.Parent)
	}
	dbCtx, dbCancel = actorTestContext(t)
	account, err = store.GetAccount(dbCtx, key)
	dbCancel()
	if err != nil {
		t.Fatal(err)
	}
	if !account.Ready || !account.Market.Valid {
		t.Fatalf("reconciled state was not durably restored: ready=%t market=%+v", account.Ready, account.Market)
	}
}
