package runtime

import (
	"errors"
	"reflect"
	"testing"

	d "github.com/liquiditylab/liquiditylab/internal/domain"
	pg "github.com/liquiditylab/liquiditylab/internal/store/postgres"
)

func TestRecoverPaperCancelsRetainedChildAndWaitsForFreshBook(t *testing.T) {
	store, key, _, old := actorTestReady(t, 0)
	actorTestCreateAndStart(t, old, "recover-paper-passive")
	queued := actorTestQueue(t, old, d.PassiveNormal, "recover-paper-queue")
	if err := actorTestDispatch(t, old, queued.State.Child.ID, "recover-paper-dispatch"); err != nil {
		t.Fatal(err)
	}
	before := actorTestView(t, old)
	ctx, cancel := actorTestContext(t)
	defer cancel()
	if err := old.Close(ctx); err != nil {
		t.Fatal(err)
	}
	next := recoveryTestNew(t, store, key, "recover-paper-owner")
	if err := next.RecoverPaper(ctx); !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("recovery without fresh book: %v", err)
	}
	settled := actorTestView(t, next)
	if settled.Account.Ready || settled.State.Child != nil || len(settled.Account.Intents) != 0 || len(settled.Account.Reservations) != 0 || settled.State.Parent.Status != "paused" || !settled.State.Parent.Deadline.Equal(before.State.Parent.Deadline) || !reflect.DeepEqual(before.Account.Balances, settled.Account.Balances) {
		t.Fatalf("unsafe pre-book recovery: %+v", settled)
	}
	actorTestMarket(t, next, "recover-paper-fresh-book")
	if err := next.RecoverPaper(ctx); err != nil {
		t.Fatal(err)
	}
	after := actorTestView(t, next)
	if !after.Account.Ready || after.State.RecoveryRequired || after.State.Parent.Status != "paused" || len(after.State.Fills) != 0 || after.Account.ObservedSequence != after.Account.ProcessedSequence {
		t.Fatalf("paper recovery did not settle safely: %+v", after)
	}
	if err := next.RecoverPaper(ctx); err != nil {
		t.Fatal(err)
	}
	retry := actorTestView(t, next)
	if retry.Account.Revision != after.Account.Revision {
		t.Fatal("ready recovery retry appended a transition")
	}
	// The old simulator cannot append an effect even after the new account is ready.
	if _, err := old.transport.Cancel(ctx, "late-old-child"); !errors.Is(err, pg.ErrOwnership) {
		t.Fatalf("stale simulator resumed: %v", err)
	}
}

func TestRecoverPaperReducesCommittedIOCOutcomeExactlyOnce(t *testing.T) {
	store, key, _, old := actorTestReady(t, 0)
	actorTestCreateAndStart(t, old, "recover-paper-ioc")
	queued := actorTestQueue(t, old, d.AggressiveNormal, "recover-paper-ioc-queue")
	_, fact := recoveryTestHandoff(t, old, queued.State.Child.ID)
	next := recoveryTestNew(t, store, key, "recover-paper-ioc-owner")
	ctx, cancel := actorTestContext(t)
	defer cancel()
	if err := next.RecoverPaper(ctx); !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("recovery without fresh book: %v", err)
	}
	actorTestMarket(t, next, "recover-paper-ioc-fresh-book")
	if err := next.RecoverPaper(ctx); err != nil {
		t.Fatal(err)
	}
	after := actorTestView(t, next)
	if len(after.State.Fills) != 1 || !after.State.Fills[0].Equal(fact.Outcome.Fills[0]) || !after.Account.Ready || len(after.Account.Intents) != 0 || len(after.Account.Reservations) != 0 || after.State.Parent.Status != "paused" {
		t.Fatalf("committed effect was lost or resubmitted: %+v", after)
	}
	if err := next.RecoverPaper(ctx); err != nil {
		t.Fatal(err)
	}
	retry := actorTestView(t, next)
	if !reflect.DeepEqual(after.Account.Balances, retry.Account.Balances) || len(retry.State.Fills) != 1 {
		t.Fatal("retry duplicated a fill")
	}
}

func TestRecoverPaperCannotClearContradictoryFacts(t *testing.T) {
	store, key, _, old := actorTestReady(t, 0)
	actorTestCreateAndStart(t, old, "recover-paper-quarantine")
	ctx, cancel := actorTestContext(t)
	defer cancel()
	if err := old.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordObservation(ctx, key, pg.Observation{ID: "contradictory-fill", Kind: "fill", Payload: jsonBytes(Fact{ID: "contradictory-fill", IntentID: "unknown-order", Fill: d.Fill{ExecutionID: "unknown-execution", OrderID: "unknown-order", Side: "buy", Quantity: d.MustDecimal("0.01"), Price: d.MustDecimal("100"), FeeCurrency: "quote"}})}); err != nil {
		t.Fatal(err)
	}
	next := recoveryTestNew(t, store, key, "recover-paper-quarantine-owner")
	actorTestMarket(t, next, "recover-paper-quarantine-book")
	if err := next.RecoverPaper(ctx); !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("unexplained fact was cleared: %v", err)
	}
	after := actorTestView(t, next)
	if after.Account.Ready || !after.State.RecoveryRequired || len(after.State.UnresolvedFacts) == 0 {
		t.Fatal("contradictory fact did not keep recovery required")
	}
}
