package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	d "github.com/liquiditylab/liquiditylab/internal/domain"
)

func TestActorCancelControlPersistsThroughQuarantinedInbox(t *testing.T) {
	store, key, _, actor := actorTestReady(t, 0)
	actorTestCreateAndStart(t, actor, "plan-cancel-during-quarantine")
	queued := actorTestQueue(t, actor, d.PassiveNormal, "action-cancel-during-quarantine")
	childID := queued.State.Child.ID
	if err := actorTestDispatch(t, actor, childID, "dispatch-cancel-during-quarantine"); err != nil {
		t.Fatalf("dispatch passive child: %v", err)
	}
	open := actorTestView(t, actor)
	if open.State.Child == nil || open.State.Child.State != "open" || len(open.Account.Reservations) != 1 {
		t.Fatalf("passive child not open before quarantine: child=%#v reservations=%#v", open.State.Child, open.Account.Reservations)
	}
	reservation := open.Account.Reservations[0].Amount

	badFill := d.Fill{
		ExecutionID: "overfill-blocks-cancel", OrderID: childID, Side: "buy",
		Quantity: d.MustDecimal("0.051"), Price: d.MustDecimal("99"),
		Fee: d.MustDecimal("0.005049"), FeeCurrency: "quote", Time: 1, Maker: true,
	}
	const badFactID = "quarantining-overfill-before-cancel"
	ctx, cancel := actorTestContext(t)
	err := actor.ObserveFill(ctx, badFactID, childID, badFill)
	cancel()
	if !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("overfill should remain quarantined in inbox: %v", err)
	}
	quarantined := actorTestView(t, actor)
	if quarantined.Account.Ready || len(quarantined.State.UnresolvedFacts) != 1 || quarantined.State.UnresolvedFacts[0].ID != badFactID {
		t.Fatalf("bad source fact did not quarantine account: %+v", quarantined)
	}
	badObservation, err := store.GetObservation(context.Background(), key, badFactID)
	if err != nil || badObservation.Kind != "fill" {
		t.Fatalf("raw quarantined fill is not durable: observation=%+v err=%v", badObservation, err)
	}
	var rawFill Fact
	if err := json.Unmarshal(badObservation.Payload, &rawFill); err != nil || rawFill.Fill.ExecutionID != badFill.ExecutionID {
		t.Fatalf("raw fill inbox payload changed or unavailable: fact=%+v err=%v", rawFill, err)
	}

	ctx, cancel = actorTestContext(t)
	cancelResult, cancelErr := actor.Do(ctx, Command{
		ID: "cancel-open-child-during-quarantine", ExpectedRevision: quarantined.Account.Revision,
		Kind: "cancel",
	})
	cancel()
	if cancelErr != nil || cancelResult.Revision == 0 {
		t.Fatalf("cancel should acknowledge its durable local state despite blocked reduction: result=%+v err=%v", cancelResult, cancelErr)
	}
	view := actorTestView(t, actor)
	if view.State.Parent == nil || view.State.Parent.Status != "cancel_requested" || view.State.Child == nil ||
		view.State.Child.State != "cancel_requested" || view.State.Child.Order.State != "cancel_requested" {
		t.Fatalf("cancel control state was not committed durably: parent=%+v child=%+v", view.State.Parent, view.State.Child)
	}
	if view.Account.Ready || view.Account.ObservedSequence <= view.Account.ProcessedSequence {
		t.Fatalf("account should remain quarantined with pending observations: ready=%t observed=%d processed=%d", view.Account.Ready, view.Account.ObservedSequence, view.Account.ProcessedSequence)
	}

	cancelObservation, err := store.GetObservation(context.Background(), key, "paper-cancel-"+childID)
	if err != nil || cancelObservation.Kind != paperTransportKind {
		t.Fatalf("paper cancel handoff fact not durable: observation=%+v err=%v", cancelObservation, err)
	}
	var cancelFact transportFact
	if err := json.Unmarshal(cancelObservation.Payload, &cancelFact); err != nil {
		t.Fatal(err)
	}
	if cancelFact.IntentID != childID || cancelFact.OperationID != "paper-cancel-"+childID || !cancelFact.Outcome.Accepted || !cancelFact.Outcome.KnownFinal {
		t.Fatalf("unexpected durable cancel outcome: %+v", cancelFact)
	}
	if cancelObservation.Sequence <= badObservation.Sequence {
		t.Fatalf("cancel fact should remain behind blocked raw fill: bad=%d cancel=%d", badObservation.Sequence, cancelObservation.Sequence)
	}

	dbCtx, dbCancel := actorTestContext(t)
	durable, err := store.GetAccount(dbCtx, key)
	dbCancel()
	if err != nil {
		t.Fatal(err)
	}
	intent, err := store.GetIntent(context.Background(), key, childID)
	if err != nil {
		t.Fatal(err)
	}
	if durable.Ready || intent.State != "acknowledged" || len(durable.Reservations) != 1 ||
		!durable.Reservations[0].Amount.Equal(reservation) {
		t.Fatalf("blocked cancellation acknowledgement released hold or restored readiness: ready=%t intent=%+v reservations=%+v", durable.Ready, intent, durable.Reservations)
	}
	journal, err := store.Journal(context.Background(), key, quarantined.Account.Revision, 16)
	if err != nil {
		t.Fatal(err)
	}
	foundCancel := false
	for _, entry := range journal {
		if entry.Kind == "cancel" && entry.Revision == cancelResult.Revision {
			foundCancel = true
		}
	}
	if !foundCancel {
		t.Fatalf("committed cancellation missing from durable journal after revision %d: %+v", quarantined.Account.Revision, journal)
	}
	if _, err := store.GetObservation(context.Background(), key, badFactID); err != nil {
		t.Fatalf("cancel path dropped quarantined raw source fact: %v", err)
	}

	beforeBlockedRequests := actorTestView(t, actor)
	ctx, cancel = actorTestContext(t)
	_, actionErr := actor.Do(ctx, Command{
		ID: "action-blocked-by-quarantine", ExpectedRevision: beforeBlockedRequests.Account.Revision,
		Kind: "action", Action: d.AggressiveNormal,
	})
	cancel()
	if !errors.Is(actionErr, ErrRecoveryRequired) {
		t.Fatalf("new action after quarantined cancellation error=%v, want recovery-required", actionErr)
	}
	ctx, cancel = actorTestContext(t)
	_, dispatchErr := actor.Do(ctx, Command{
		ID: "dispatch-blocked-by-quarantine", ExpectedRevision: beforeBlockedRequests.Account.Revision,
		Kind: "dispatch", IntentID: childID,
	})
	cancel()
	if !errors.Is(dispatchErr, ErrRecoveryRequired) {
		t.Fatalf("new dispatch after quarantined cancellation error=%v, want recovery-required", dispatchErr)
	}
	afterBlockedRequests := actorTestView(t, actor)
	if afterBlockedRequests.Account.ObservedSequence != beforeBlockedRequests.Account.ObservedSequence ||
		afterBlockedRequests.Account.ProcessedSequence != beforeBlockedRequests.Account.ProcessedSequence ||
		afterBlockedRequests.State.Parent == nil || afterBlockedRequests.State.Parent.Status != "cancel_requested" ||
		afterBlockedRequests.State.Child == nil || afterBlockedRequests.State.Child.State != "cancel_requested" {
		t.Fatalf("blocked proposal/dispatch changed durable cancellation or source cursors: before=%+v after=%+v", beforeBlockedRequests, afterBlockedRequests)
	}
	afterAccount, err := store.GetAccount(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	if len(afterAccount.Reservations) != 1 || !afterAccount.Reservations[0].Amount.Equal(reservation) || afterAccount.Ready {
		t.Fatalf("failed proposal/dispatch changed hold or readiness: %+v", afterAccount)
	}
	if _, err := store.GetObservation(context.Background(), key, "paper-cancel-"+childID); err != nil {
		t.Fatalf("blocked reductions deleted durable cancel request: %v", err)
	}
	if _, err := store.GetObservation(context.Background(), key, badFactID); err != nil {
		t.Fatalf("blocked reductions deleted quarantined input: %v", err)
	}
}
