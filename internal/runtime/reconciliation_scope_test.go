package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	d "github.com/liquiditylab/liquiditylab/internal/domain"
	pg "github.com/liquiditylab/liquiditylab/internal/store/postgres"
)

func TestActorResolvedOldParentFactDoesNotForceAbortOfNewParent(t *testing.T) {
	store, key, _, actor := actorTestReady(t, 0)
	oldParentID := "plan-old-quarantined-fill"
	actorTestCreateAndStart(t, actor, oldParentID)
	queued := actorTestQueue(t, actor, d.PassiveNormal, "action-old-quarantined-fill")
	childID := queued.State.Child.ID
	if err := actorTestDispatch(t, actor, childID, "dispatch-old-quarantined-fill"); err != nil {
		t.Fatalf("dispatch old passive child: %v", err)
	}

	overfill := d.Fill{
		ExecutionID: "contradictory-old-parent-fill", OrderID: childID,
		Side: "buy", Quantity: d.MustDecimal("0.051"), Price: d.MustDecimal("99"),
		Fee: d.MustDecimal("0.005049"), FeeCurrency: "quote", Time: 1, Maker: true,
	}
	const factID = "quarantined-overfill-old-parent"
	ctx, cancel := actorTestContext(t)
	err := actor.ObserveFill(ctx, factID, childID, overfill)
	cancel()
	if !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("overfill should quarantine the durable fact: %v", err)
	}
	quarantined := actorTestView(t, actor)
	if quarantined.Account.Ready || !quarantined.State.RecoveryRequired || len(quarantined.State.UnresolvedFacts) != 1 {
		t.Fatalf("contradictory fill was not quarantined: %+v", quarantined)
	}
	unresolved := quarantined.State.UnresolvedFacts[0]
	if unresolved.ID != factID || unresolved.ParentID != oldParentID || unresolved.Resolved {
		t.Fatalf("quarantined fact lacks old-parent scope: %+v", unresolved)
	}
	rawFact, err := store.GetObservation(context.Background(), key, factID)
	if err != nil || rawFact.Kind != "fill" {
		t.Fatalf("raw fill inbox record not retained: observation=%+v err=%v", rawFact, err)
	}

	actorTestMarket(t, actor, "snapshot-reconcile-old-parent")
	beforeReconcile := actorTestView(t, actor)
	ctx, cancel = actorTestContext(t)
	_, err = actor.Reconcile(ctx, Reconciliation{
		ID: "reconcile-old-parent-quarantined-fill", ExpectedRevision: beforeReconcile.Account.Revision,
		Evidence: "operator-resolved-contradictory-old-parent-fill", NoExternalOrders: true,
		Balances: beforeReconcile.Account.Balances,
		Resolutions: []pg.Resolution{{
			IntentID: childID, State: "final", KnownFinal: true,
			Evidence: json.RawMessage(`{"evidence":"old-parent child is final"}`),
		}},
		ResolvedFactIDs: []string{factID}, AbortParent: true,
	})
	cancel()
	if err != nil {
		t.Fatalf("reconcile and explicitly abort old parent: %v", err)
	}
	oldResolved := actorTestView(t, actor)
	if oldResolved.State.Parent == nil || oldResolved.State.Parent.ID != oldParentID || oldResolved.State.Parent.Status != "cancelled" || oldResolved.State.Child != nil {
		t.Fatalf("old parent/child did not reach known final canceled state: parent=%+v child=%+v", oldResolved.State.Parent, oldResolved.State.Child)
	}
	if len(oldResolved.State.UnresolvedFacts) != 1 || !oldResolved.State.UnresolvedFacts[0].Resolved || oldResolved.State.UnresolvedFacts[0].ParentID != oldParentID {
		t.Fatalf("resolved old-parent fact lost its audit scope: %+v", oldResolved.State.UnresolvedFacts)
	}
	if oldResolved.Account.ProcessedSequence >= oldResolved.Account.ObservedSequence {
		t.Fatalf("reconciliation unexpectedly consumed source fact before the next control: observed=%d processed=%d", oldResolved.Account.ObservedSequence, oldResolved.Account.ProcessedSequence)
	}

	// This control first drains the now-resolved inbox fact. Its captured account
	// revision is consequently stale; the caller retries the same uncommitted
	// command identity with the refreshed revision.
	newPlan := actorTestPlan()
	create := Command{ID: "create-after-old-fact", ExpectedRevision: oldResolved.Account.Revision, Kind: "create", PlanID: "plan-new-after-old-fact", Plan: newPlan}
	ctx, cancel = actorTestContext(t)
	_, err = actor.Do(ctx, create)
	cancel()
	if !errors.Is(err, pg.ErrStaleRevision) {
		t.Fatalf("first control after resolving fact should drain then report stale revision, got %v", err)
	}
	consumed := actorTestView(t, actor)
	if consumed.Account.ProcessedSequence != consumed.Account.ObservedSequence {
		t.Fatalf("next control did not consume resolved source fact: observed=%d processed=%d", consumed.Account.ObservedSequence, consumed.Account.ProcessedSequence)
	}
	create.ExpectedRevision = consumed.Account.Revision
	ctx, cancel = actorTestContext(t)
	_, err = actor.Do(ctx, create)
	cancel()
	if err != nil {
		t.Fatalf("retry create with refreshed revision: %v", err)
	}
	actorTestDo(t, actor, Command{ID: "start-new-after-old-fact", Kind: "start"})
	newParent := actorTestView(t, actor)
	if newParent.State.Parent == nil || newParent.State.Parent.ID != "plan-new-after-old-fact" || newParent.State.Parent.Status != "active" {
		t.Fatalf("new parent failed to start after source drain: %+v", newParent.State.Parent)
	}

	ctx, cancel = actorTestContext(t)
	err = actor.Market(ctx, d.MarketEvent{Kind: "gap", Symbol: "BTC/USD", ReceiptIndex: 2, Invalid: true}, "gap-new-parent")
	cancel()
	if err != nil {
		t.Fatalf("invalidate new parent's market: %v", err)
	}
	actorTestMarket(t, actor, "snapshot-reconcile-new-parent")
	actorTestReconcile(t, actor, "reconcile-new-parent-no-abort")
	final := actorTestView(t, actor)
	if !final.Account.Ready || !final.State.Book.Valid || final.State.RecoveryRequired || final.State.Parent == nil ||
		final.State.Parent.ID != "plan-new-after-old-fact" || final.State.Parent.Status != "active" {
		t.Fatalf("old fact improperly forced abort of unrelated new parent: account=%+v state=%+v", final.Account, final.State)
	}
	if len(final.State.UnresolvedFacts) != 1 || !final.State.UnresolvedFacts[0].Resolved || final.State.UnresolvedFacts[0].ParentID != oldParentID {
		t.Fatalf("old fact audit record was not preserved after new-parent reconciliation: %+v", final.State.UnresolvedFacts)
	}
}
