package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	d "github.com/liquiditylab/liquiditylab/internal/domain"
	pg "github.com/liquiditylab/liquiditylab/internal/store/postgres"
)

// recoveryTestHandoff persists the same durable source fact that dispatch would
// receive, while deliberately leaving the actor's reducer behind that fact.
func recoveryTestHandoff(t *testing.T, a *Actor, childID string) (pg.Observation, transportFact) {
	t.Helper()
	ctx, cancel := actorTestContext(t)
	defer cancel()
	// Establish the crash checkpoint only after its owner loop has stopped.
	if err := a.Close(ctx); err != nil {
		t.Fatal(err)
	}
	child := a.state.Child
	if child == nil || child.ID != childID || child.State != "queued" {
		t.Fatalf("expected queued child %s, got %#v", childID, child)
	}
	authorized, err := a.config.Store.AuthorizeDispatch(ctx, pg.Authorization{
		Key: a.config.Key, Ownership: a.owner, IntentID: child.ID,
		ExpectedRevision: a.account.Revision, PlanRevision: a.state.Parent.Revision,
		SnapshotID: a.state.MarketID, InstrumentRevision: a.state.InstrumentRevision,
		AttemptID: "recovery-checkpoint-attempt",
	})
	if err != nil {
		t.Fatalf("AuthorizeDispatch: %v", err)
	}
	a.account.Revision = authorized.Revision
	book, err := cloneState(a.state)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := a.transport.Handoff(ctx, Submission{
		ID: child.ID, Intent: child.Order.Intent, Book: book.Book, Fees: a.state.Fees,
		Now: a.elapsed(a.state), OperationID: "recovery-dispatch-" + child.ID,
		InputDigest: "recovery-input-digest-" + child.ID,
	})
	if err != nil {
		t.Fatalf("persist paper outcome: %v", err)
	}
	if !outcome.KnownFinal || len(outcome.Fills) != 1 {
		t.Fatalf("expected one full IOC outcome, got %+v", outcome)
	}
	observation, err := a.config.Store.GetObservation(ctx, a.config.Key, "paper-submit-"+child.ID)
	if err != nil {
		t.Fatalf("read durable source outcome: %v", err)
	}
	var fact transportFact
	if err = json.Unmarshal(observation.Payload, &fact); err != nil {
		t.Fatal(err)
	}
	return observation, fact
}

func recoveryTestNew(t *testing.T, store *pg.Store, key pg.Key, owner string) *Actor {
	t.Helper()
	cfg := DefaultConfig(store, key)
	cfg.OwnerID = owner
	cfg.RecoveryProof = pg.RecoveryProof{PreviousSenderIsolated: true, Reconciled: true, Reference: "recovery-test-operator-proof"}
	ctx, cancel := actorTestContext(t)
	defer cancel()
	a, err := New(ctx, cfg)
	if err != nil {
		t.Fatalf("New recovery owner %s: %v", owner, err)
	}
	t.Cleanup(func() {
		ctx, cancel := actorTestContext(t)
		defer cancel()
		if err := a.Close(ctx); err != nil {
			t.Errorf("close recovery actor %s: %v", owner, err)
		}
	})
	return a
}

func recoveryTestDrain(t *testing.T, a *Actor) error {
	t.Helper()
	ctx, cancel := actorTestContext(t)
	defer cancel()
	_, err := a.submit(ctx, "facts", nil, true)
	return err
}

func TestRecoveryDrainsDurablePaperOutcomeExactlyOnce(t *testing.T) {
	store, key, _, old := actorTestReady(t, 0)
	actorTestCreateAndStart(t, old, "plan-recovery-outcome")
	queued := actorTestQueue(t, old, d.AggressiveNormal, "action-recovery-outcome")
	childID := queued.State.Child.ID
	observation, fact := recoveryTestHandoff(t, old, childID)
	if observation.Sequence != queued.Account.ObservedSequence+1 {
		t.Fatalf("source sequence did not advance exactly once: before=%d observation=%d", queued.Account.ObservedSequence, observation.Sequence)
	}
	ctx, cancel := actorTestContext(t)
	if err := old.Close(ctx); err != nil {
		cancel()
		t.Fatal(err)
	}
	cancel()

	recovered := recoveryTestNew(t, store, key, "paper-process-recovery-outcome")
	actorTestMarket(t, recovered, "recovery-outcome-snapshot")
	if err := recoveryTestDrain(t, recovered); err != nil {
		t.Fatalf("drain durable source outcome: %v", err)
	}
	state := actorTestView(t, recovered)
	if len(state.State.Fills) != 1 || !state.State.Fills[0].Equal(fact.Outcome.Fills[0]) {
		t.Fatalf("recovery did not apply exactly the durable fill: %#v", state.State.Fills)
	}
	if state.State.Child != nil || state.State.RecoveryRequired != true {
		t.Fatalf("unexpected recovered child/reconciliation state: child=%#v recovery=%v", state.State.Child, state.State.RecoveryRequired)
	}
	if findBalance(state.Account.Balances, "BTC").Total.Cmp(d.MustDecimal("1.05")) != 0 || findBalance(state.Account.Balances, "USD").Total.Cmp(d.MustDecimal("994.9399")) != 0 {
		t.Fatalf("recovery financial projection mismatch: %+v", state.Account.Balances)
	}
	if state.Account.ProcessedSequence != observation.Sequence || state.Account.ObservedSequence != observation.Sequence {
		t.Fatalf("source cursor did not advance through outcome: observed=%d processed=%d want=%d", state.Account.ObservedSequence, state.Account.ProcessedSequence, observation.Sequence)
	}
	if _, err := store.GetExecution(context.Background(), key, "paper", fact.Outcome.Fills[0].ExecutionID); err != nil {
		t.Fatalf("durable execution after recovery: %v", err)
	}
	// Re-draining an already consumed inbox must not duplicate either the fill or
	// its balance effect.
	if err := recoveryTestDrain(t, recovered); err != nil {
		t.Fatalf("empty second drain: %v", err)
	}
	after := actorTestView(t, recovered)
	if len(after.State.Fills) != 1 || findBalance(after.Account.Balances, "BTC").Total.Cmp(d.MustDecimal("1.05")) != 0 {
		t.Fatalf("second drain duplicated financial effect: fills=%d balances=%+v", len(after.State.Fills), after.Account.Balances)
	}
}

func TestRecoveryConsumesTransportAfterFullFillCheckpoint(t *testing.T) {
	store, key, _, old := actorTestReady(t, 0)
	actorTestCreateAndStart(t, old, "plan-recovery-cursor")
	queued := actorTestQueue(t, old, d.AggressiveNormal, "action-recovery-cursor")
	childID := queued.State.Child.ID
	observation, fact := recoveryTestHandoff(t, old, childID)
	ctx, cancel := actorTestContext(t)
	if err := old.Close(ctx); err != nil {
		cancel()
		t.Fatal(err)
	}
	cancel()
	// Inject the crash checkpoint after the fill's financial transaction has
	// committed, but before the containing transport observation is consumed.
	checkpoint := Fact{ID: fmt.Sprintf("%s/fill/%d", observation.ID, 0), IntentID: childID, Fill: fact.Outcome.Fills[0]}
	ctx, cancel = actorTestContext(t)
	err := old.applyFill(ctx, observation, checkpoint, 0, false)
	cancel()
	if err != nil {
		t.Fatalf("establish full-fill pre-cursor checkpoint: %v", err)
	}
	before, err := store.GetAccount(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	intent, err := store.GetIntent(context.Background(), key, childID)
	if err != nil {
		t.Fatal(err)
	}
	if intent.State != "final" || before.ProcessedSequence >= observation.Sequence || before.ObservedSequence != observation.Sequence {
		t.Fatalf("checkpoint not established: intent=%s observed=%d processed=%d source=%d", intent.State, before.ObservedSequence, before.ProcessedSequence, observation.Sequence)
	}
	if findBalance(before.Balances, "BTC").Total.Cmp(d.MustDecimal("1.05")) != 0 || findBalance(before.Balances, "USD").Total.Cmp(d.MustDecimal("994.9399")) != 0 {
		t.Fatalf("full fill checkpoint has wrong projection: %+v", before.Balances)
	}

	recovered := recoveryTestNew(t, store, key, "paper-process-recovery-cursor")
	if recovered.state.Child == nil || !recovered.state.Child.Order.Remaining.IsZero() {
		t.Fatalf("restart did not retain the zero-remaining child needed for cursor recovery: %#v", recovered.state.Child)
	}
	if err := recoveryTestDrain(t, recovered); err != nil {
		t.Fatalf("consume pending full-fill transport: %v", err)
	}
	after := actorTestView(t, recovered)
	if after.Account.ProcessedSequence != observation.Sequence || after.Account.ObservedSequence != observation.Sequence {
		t.Fatalf("cursor did not finish pending transport: observed=%d processed=%d source=%d", after.Account.ObservedSequence, after.Account.ProcessedSequence, observation.Sequence)
	}
	if len(after.State.UnresolvedFacts) != 0 || len(after.State.Fills) != 1 || after.State.Child != nil {
		t.Fatalf("valid recovery transport was classified unresolved or duplicated: unresolved=%#v fills=%#v child=%#v", after.State.UnresolvedFacts, after.State.Fills, after.State.Child)
	}
	if findBalance(after.Account.Balances, "BTC").Total.Cmp(d.MustDecimal("1.05")) != 0 || findBalance(after.Account.Balances, "USD").Total.Cmp(d.MustDecimal("994.9399")) != 0 {
		t.Fatalf("cursor recovery changed already committed balances: %+v", after.Account.Balances)
	}
}

func TestActorViewReturnsDeepCopies(t *testing.T) {
	_, _, _, a := actorTestReady(t, 0)
	actorTestCreateAndStart(t, a, "plan-view-copy")
	actorTestQueue(t, a, d.AggressiveNormal, "action-view-copy")
	first := actorTestView(t, a)
	if len(first.Account.Reservations) == 0 || first.State.Book == nil || first.State.Parent == nil || len(first.Account.Snapshot) == 0 {
		t.Fatalf("test setup missing mutable view fields: account=%#v state=%#v", first.Account, first.State)
	}
	first.Account.Balances[0].Total = d.Zero
	first.Account.Reservations[0].Amount = d.Zero
	first.Account.Snapshot[0] = '!'
	first.State.Book.Bids["100000"] = d.Level{Price: d.MustDecimal("100000"), Quantity: d.One}
	first.State.Parent.Spec.Quantity = d.Zero
	first.State.Reason = "caller-mutated-view"
	second := actorTestView(t, a)
	if findBalance(second.Account.Balances, "USD").Total.IsZero() || second.Account.Reservations[0].Amount.IsZero() || second.Account.Snapshot[0] == '!' {
		t.Fatalf("account view mutation reached actor state: account=%#v", second.Account)
	}
	if second.State.Book.BestBid().Cmp(d.MustDecimal("99")) != 0 || second.State.Parent.Spec.Quantity.Cmp(d.MustDecimal("0.1")) != 0 || second.State.Reason == "caller-mutated-view" {
		t.Fatalf("state view mutation reached actor state: %#v", second.State)
	}
}

func TestActorRejectsConcurrentSecondInstanceWithSameOwner(t *testing.T) {
	store, key, _, first := actorTestReady(t, 0)
	_ = first
	cfg := DefaultConfig(store, key)
	cfg.OwnerID = "paper-process-a"
	ctx, cancel := actorTestContext(t)
	defer cancel()
	if _, err := New(ctx, cfg); !errors.Is(err, pg.ErrOwnership) {
		t.Fatalf("second actor with same owner error=%v, want ownership rejection", err)
	}
}
