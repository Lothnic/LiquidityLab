package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	d "github.com/liquiditylab/liquiditylab/internal/domain"
	pg "github.com/liquiditylab/liquiditylab/internal/store/postgres"
)

func actorTestMarketFromSession(t *testing.T, actor *Actor, event d.MarketEvent, snapshotID, sessionID string) error {
	t.Helper()
	ctx, cancel := actorTestContext(t)
	defer cancel()
	return actor.MarketFromSession(ctx, event, snapshotID, sessionID)
}

func actorTestTrade(tradeID string, quantity string, receiptIndex uint64, receivedAt int64) d.MarketEvent {
	return d.MarketEvent{
		Time:         receivedAt,
		Kind:         "trade",
		Symbol:       "BTC/USD",
		TradeID:      tradeID,
		ReceiptIndex: receiptIndex,
		Side:         "sell",
		Price:        d.MustDecimal("99"),
		Quantity:     d.MustDecimal(quantity),
	}
}

func TestActorPassiveTradesDepleteQueueFillAndDeduplicateVenueIdentity(t *testing.T) {
	store, key, _, actor := actorTestReady(t, 0)
	actorTestCreateAndStart(t, actor, "plan-passive-trades")
	queued := actorTestQueue(t, actor, d.PassiveNormal, "action-passive-trades")
	childID := queued.State.Child.ID
	if err := actorTestDispatch(t, actor, childID, "dispatch-passive-trades"); err != nil {
		t.Fatalf("dispatch passive child: %v", err)
	}
	initial := actorTestView(t, actor)
	if initial.State.Child == nil || !initial.State.Child.Order.QueueAhead.Equal(d.MustDecimal("2")) {
		t.Fatalf("passive child did not start behind visible queue: %#v", initial.State.Child)
	}
	initialReservation := initial.Account.Reservations[0].Amount

	// This trade only removes the displayed two-unit queue ahead of the child.
	queueTrade := actorTestTrade("venue-queue-1", "2", 5, 1_700_000_000_000_000_001)
	if err := actorTestMarketFromSession(t, actor, queueTrade, "snapshot-trade-a", "receipt-session-a"); err != nil {
		t.Fatalf("deplete passive queue: %v", err)
	}
	depleted := actorTestView(t, actor)
	if depleted.State.Child == nil || !depleted.State.Child.Order.QueueAhead.IsZero() || len(depleted.State.Fills) != 0 {
		t.Fatalf("queue-only trade caused fill or failed to deplete queue: child=%#v fills=%#v", depleted.State.Child, depleted.State.Fills)
	}
	if findBalance(depleted.Account.Balances, "BTC").Total.Cmp(findBalance(initial.Account.Balances, "BTC").Total) != 0 ||
		findBalance(depleted.Account.Balances, "USD").Total.Cmp(findBalance(initial.Account.Balances, "USD").Total) != 0 {
		t.Fatalf("queue depletion changed balances: before=%+v after=%+v", initial.Account.Balances, depleted.Account.Balances)
	}
	if !depleted.Account.Reservations[0].Amount.Equal(initialReservation) {
		t.Fatalf("queue depletion changed reservation: before=%s after=%s", initialReservation, depleted.Account.Reservations[0].Amount)
	}

	// Repeated receipts with the same venue TradeID are one economic event even
	// when local receipt metadata and the feed snapshot/session change.
	queueRetry := queueTrade
	queueRetry.Time += 9_000_000
	queueRetry.ReceiptIndex += 100
	if err := actorTestMarketFromSession(t, actor, queueRetry, "snapshot-trade-b", "receipt-session-b"); err != nil {
		t.Fatalf("retry queue trade with changed receipt metadata: %v", err)
	}
	retriedQueue := actorTestView(t, actor)
	if retriedQueue.Account.ObservedSequence != depleted.Account.ObservedSequence || retriedQueue.Account.ProcessedSequence != depleted.Account.ProcessedSequence {
		t.Fatalf("queue trade retry added another durable fact: before=%d/%d after=%d/%d", depleted.Account.ObservedSequence, depleted.Account.ProcessedSequence, retriedQueue.Account.ObservedSequence, retriedQueue.Account.ProcessedSequence)
	}

	partialTrade := actorTestTrade("venue-partial-1", "0.03", 6, 1_700_000_000_000_000_002)
	if err := actorTestMarketFromSession(t, actor, partialTrade, "snapshot-trade-c", "receipt-session-c"); err != nil {
		t.Fatalf("partial maker trade: %v", err)
	}
	partial := actorTestView(t, actor)
	if partial.State.Child == nil || !partial.State.Child.Order.Remaining.Equal(d.MustDecimal("0.02")) || len(partial.State.Fills) != 1 {
		t.Fatalf("partial maker fill mismatch: child=%#v fills=%#v", partial.State.Child, partial.State.Fills)
	}
	fill := partial.State.Fills[0]
	if fill.Quantity.Cmp(d.MustDecimal("0.03")) != 0 || fill.Price.Cmp(d.MustDecimal("99")) != 0 || fill.Fee.Cmp(d.MustDecimal("0.00297")) != 0 || fill.FeeCurrency != "quote" || !fill.Maker {
		t.Fatalf("unexpected exact maker fill: %+v", fill)
	}
	if findBalance(partial.Account.Balances, "BTC").Total.Cmp(d.MustDecimal("1.03")) != 0 || findBalance(partial.Account.Balances, "USD").Total.Cmp(d.MustDecimal("997.02703")) != 0 {
		t.Fatalf("partial trade financial projection mismatch: %+v", partial.Account.Balances)
	}
	partialReservation := d.MustDecimal("1.98396")
	if len(partial.Account.Reservations) != 1 || !partial.Account.Reservations[0].Amount.Equal(partialReservation) {
		t.Fatalf("partial fill did not reduce reservation to remaining child amount: %+v", partial.Account.Reservations)
	}

	partialRetry := partialTrade
	partialRetry.Time += 12_000_000
	partialRetry.ReceiptIndex += 300
	if err := actorTestMarketFromSession(t, actor, partialRetry, "snapshot-trade-d", "receipt-session-d"); err != nil {
		t.Fatalf("retry partial trade with changed receipt metadata: %v", err)
	}
	retriedPartial := actorTestView(t, actor)
	if len(retriedPartial.State.Fills) != 1 || retriedPartial.Account.ObservedSequence != partial.Account.ObservedSequence ||
		!findBalance(retriedPartial.Account.Balances, "BTC").Total.Equal(findBalance(partial.Account.Balances, "BTC").Total) ||
		!findBalance(retriedPartial.Account.Balances, "USD").Total.Equal(findBalance(partial.Account.Balances, "USD").Total) ||
		!retriedPartial.Account.Reservations[0].Amount.Equal(partial.Account.Reservations[0].Amount) {
		t.Fatalf("duplicate venue TradeID changed fill/projection: before=%+v after=%+v", partial, retriedPartial)
	}

	finalTrade := actorTestTrade("venue-final-1", "0.02", 7, 1_700_000_000_000_000_003)
	if err := actorTestMarketFromSession(t, actor, finalTrade, "snapshot-trade-e", "receipt-session-e"); err != nil {
		t.Fatalf("final maker trade: %v", err)
	}
	final := actorTestView(t, actor)
	if final.State.Child != nil || len(final.Account.Reservations) != 0 || len(final.State.Fills) != 2 {
		t.Fatalf("final trade failed to close child and release hold: child=%#v reservations=%#v fills=%#v", final.State.Child, final.Account.Reservations, final.State.Fills)
	}
	if final.State.Fills[1].Quantity.Cmp(d.MustDecimal("0.02")) != 0 || final.State.Fills[1].Fee.Cmp(d.MustDecimal("0.00198")) != 0 || !final.State.Fills[1].Maker {
		t.Fatalf("unexpected final exact maker fill: %+v", final.State.Fills[1])
	}
	if findBalance(final.Account.Balances, "BTC").Total.Cmp(d.MustDecimal("1.05")) != 0 || findBalance(final.Account.Balances, "USD").Total.Cmp(d.MustDecimal("995.04505")) != 0 {
		t.Fatalf("final trade financial projection mismatch: %+v", final.Account.Balances)
	}
	if final.Account.ObservedSequence != final.Account.ProcessedSequence {
		t.Fatalf("paper trade inbox not fully consumed: observed=%d processed=%d", final.Account.ObservedSequence, final.Account.ProcessedSequence)
	}
	queueFactID, _ := paperTradeIdentity("venue-trade-id", queueTrade)
	queueFact, err := store.GetObservation(context.Background(), key, queueFactID)
	if err != nil || queueFact.Kind != paperTradeKind {
		t.Fatalf("queue trade was not durably recorded: observation=%+v err=%v", queueFact, err)
	}

	// A conflicting payload under an already-used venue identity fails closed
	// without adding an observation or altering the recorded fills.
	conflict := partialTrade
	conflict.Quantity = d.MustDecimal("0.031")
	if err := actorTestMarketFromSession(t, actor, conflict, "snapshot-conflict", "receipt-session-conflict"); !errors.Is(err, pg.ErrConflict) {
		t.Fatalf("changed payload under stable TradeID error=%v, want conflict", err)
	}
	quarantined := actorTestView(t, actor)
	if quarantined.Account.Ready || quarantined.Account.Market.Valid || len(quarantined.State.Fills) != 2 || quarantined.Account.ObservedSequence != final.Account.ObservedSequence ||
		!findBalance(quarantined.Account.Balances, "BTC").Total.Equal(findBalance(final.Account.Balances, "BTC").Total) ||
		!findBalance(quarantined.Account.Balances, "USD").Total.Equal(findBalance(final.Account.Balances, "USD").Total) {
		t.Fatalf("conflicting TradeID did not fail closed without financial duplication: %+v", quarantined)
	}
}

func TestActorPaperTradeWithoutVenueIDRequiresReceiptSession(t *testing.T) {
	_, _, _, actor := actorTestReady(t, 0)
	actorTestCreateAndStart(t, actor, "plan-trade-no-session")
	before := actorTestView(t, actor)
	event := actorTestTrade("", "0.1", 3, time.Now().UnixNano())
	err := actorTestMarketFromSession(t, actor, event, "snapshot-no-session", "")
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("trade without TradeID or receipt session error=%v, want ErrInvalid", err)
	}
	view := actorTestView(t, actor)
	if view.Account.Ready || view.Account.Market.Valid || !view.State.RecoveryRequired {
		t.Fatalf("unidentifiable trade did not safely quarantine the account: %+v", view)
	}
	if view.Account.ObservedSequence != before.Account.ObservedSequence || view.Account.ProcessedSequence != before.Account.ProcessedSequence {
		t.Fatalf("unidentifiable trade created a source fact: before=%d/%d after=%d/%d", before.Account.ObservedSequence, before.Account.ProcessedSequence, view.Account.ObservedSequence, view.Account.ProcessedSequence)
	}
}

func TestActorPaperTradePendingCursorRecoversAfterFullFillCheckpoint(t *testing.T) {
	store, key, _, old := actorTestReady(t, 0)
	actorTestCreateAndStart(t, old, "plan-paper-trade-recovery")
	queued := actorTestQueue(t, old, d.PassiveNormal, "action-paper-trade-recovery")
	childID := queued.State.Child.ID
	if err := actorTestDispatch(t, old, childID, "dispatch-paper-trade-recovery"); err != nil {
		t.Fatalf("dispatch passive child: %v", err)
	}
	stopCtx, stopCancel := actorTestContext(t)
	if err := old.Close(stopCtx); err != nil {
		stopCancel()
		t.Fatal(err)
	}
	stopCancel()
	child := old.state.Child
	if child == nil || !child.Order.QueueAhead.Equal(d.MustDecimal("2")) {
		t.Fatalf("recovery setup child has unexpected queue: %#v", child)
	}
	event := actorTestTrade("venue-recovery-full", "2.05", 11, 1_700_000_000_000_000_123)
	ctx, cancel := actorTestContext(t)
	fact, err := old.transport.(*paperTransport).ObserveTrade(ctx, "recovery-receipt-session", event, child, old.state.Fees)
	cancel()
	if err != nil {
		t.Fatalf("persist full paper trade fact: %v", err)
	}
	if len(fact.Fills) != 1 || !fact.Fills[0].Quantity.Equal(child.Order.Remaining) {
		t.Fatalf("expected durable full child fill, fact=%+v child=%+v", fact, child)
	}
	ctx, cancel = actorTestContext(t)
	observation, err := store.GetObservation(ctx, key, fact.ID)
	cancel()
	if err != nil || observation.Kind != paperTradeKind {
		t.Fatalf("read durable paper trade source fact: observation=%+v err=%v", observation, err)
	}
	ctx, cancel = actorTestContext(t)
	err = old.Close(ctx)
	cancel()
	if err != nil {
		t.Fatalf("stop actor before fill checkpoint: %v", err)
	}

	checkpoint := Fact{ID: fact.ID + "/fill/0", IntentID: childID, Fill: fact.Fills[0]}
	ctx, cancel = actorTestContext(t)
	err = old.applyFill(ctx, observation, checkpoint, 0, false)
	cancel()
	if err != nil {
		t.Fatalf("commit financial fill before source cursor: %v", err)
	}
	checkpointAccount, err := store.GetAccount(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	if checkpointAccount.ProcessedSequence >= observation.Sequence || checkpointAccount.ObservedSequence != observation.Sequence {
		t.Fatalf("full-fill-before-cursor checkpoint missing: observed=%d processed=%d source=%d", checkpointAccount.ObservedSequence, checkpointAccount.ProcessedSequence, observation.Sequence)
	}
	if findBalance(checkpointAccount.Balances, "BTC").Total.Cmp(d.MustDecimal("1.05")) != 0 || findBalance(checkpointAccount.Balances, "USD").Total.Cmp(d.MustDecimal("995.04505")) != 0 {
		t.Fatalf("checkpoint financial projection mismatch: %+v", checkpointAccount.Balances)
	}

	recovered := recoveryTestNew(t, store, key, "paper-process-paper-trade-recovery")
	if recovered.state.Child == nil || !recovered.state.Child.Order.Remaining.IsZero() {
		t.Fatalf("recovery did not retain zero-remaining child for pending cursor: %#v", recovered.state.Child)
	}
	actorTestMarket(t, recovered, "snapshot-paper-trade-recovery")
	if err := recoveryTestDrain(t, recovered); err != nil {
		t.Fatalf("recover pending durable paper trade: %v", err)
	}
	after := actorTestView(t, recovered)
	if after.Account.ProcessedSequence != observation.Sequence || after.Account.ObservedSequence != observation.Sequence {
		t.Fatalf("recovered paper trade cursor not consumed: observed=%d processed=%d want=%d", after.Account.ObservedSequence, after.Account.ProcessedSequence, observation.Sequence)
	}
	if len(after.State.UnresolvedFacts) != 0 || len(after.State.Fills) != 1 || after.State.Child != nil {
		t.Fatalf("paper trade recovery duplicated/classified the valid fill incorrectly: unresolved=%#v fills=%#v child=%#v", after.State.UnresolvedFacts, after.State.Fills, after.State.Child)
	}
	if findBalance(after.Account.Balances, "BTC").Total.Cmp(d.MustDecimal("1.05")) != 0 || findBalance(after.Account.Balances, "USD").Total.Cmp(d.MustDecimal("995.04505")) != 0 {
		t.Fatalf("cursor recovery duplicated financial effect: %+v", after.Account.Balances)
	}
	var recoveredFact paperTradeFact
	if err := json.Unmarshal(observation.Payload, &recoveredFact); err != nil || recoveredFact.ID != fact.ID {
		t.Fatalf("paper trade fact changed across recovery: fact=%+v err=%v", recoveredFact, err)
	}
}
