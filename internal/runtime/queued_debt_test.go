package runtime

import (
	"context"
	"errors"
	"testing"
	"time"

	d "github.com/liquiditylab/liquiditylab/internal/domain"
	pg "github.com/liquiditylab/liquiditylab/internal/store/postgres"
)

func TestQueuedObligationsSurviveSourceDebtUntilExplicitCaughtUpDisposal(t *testing.T) {
	for _, control := range []string{"pause", "cancel", "gap"} {
		t.Run(control, func(t *testing.T) {
			store, key, _, a := actorTestReady(t, 0)
			actorTestCreateAndStart(t, a, "queued-debt-plan")
			queued := actorTestQueue(t, a, d.PassiveNormal, "queued-debt-action")
			ctx := context.Background()
			if err := a.Close(ctx); err != nil {
				t.Fatal(err)
			}
			childID := queued.State.Child.ID
			obs := pg.Observation{ID: "queued-debt-source", Kind: paperTradeKind}
			fact := paperTradeFact{ID: obs.ID, InputDigest: "queued-debt-input", IntentID: childID, Event: actorTestTrade("queued-debt-trade", "0.01", 1, 1), QueueAhead: d.Zero, Fills: []d.Fill{}}
			obs.Payload = jsonBytes(fact)
			sequence, err := store.RecordPaperObservation(ctx, key, a.owner, obs)
			if err != nil {
				t.Fatal(err)
			}
			if control == "gap" {
				err = a.market(ctx, marketInput{Event: d.MarketEvent{Kind: "gap", Symbol: "BTC/USD", Time: 2}, SnapshotID: "queued-debt-gap"})
			} else {
				_, err = a.command(ctx, Command{ID: "queued-debt-control", Kind: control, ExpectedRevision: a.account.Revision})
			}
			if err != nil {
				t.Fatal(err)
			}
			before, err := store.GetAccount(ctx, key)
			if err != nil || len(before.Intents) != 1 || before.Intents[0].State != "queued" || len(before.Reservations) != 1 || before.ProcessedSequence >= sequence || a.state.Child == nil || a.state.Child.Order.State != "submitting" {
				t.Fatalf("control released unsent obligation or made it matchable: account=%+v child=%+v err=%v", before, a.state.Child, err)
			}
			a.progressCancellation()
			if a.state.Child == nil || len(a.account.Reservations) != 1 {
				t.Fatal("cleanup crossed accepted source debt")
			}
			// A parked market operation can reduce its predecessor and freeze
			// its own trade before the next cancellation-maintenance turn.
			// The unsent order must still be ineligible to match that trade.
			if control != "gap" {
				event := actorTestTrade("queued-debt-later-trade", "100", 2, 2)
				op := &actorOperation{request: quantumTestRequest("market", marketInput{Event: event, SnapshotID: "queued-debt-later", SessionID: "queued-debt-session"})}
				a.currentOperation = op
				err = a.market(ctx, op.request.input.(marketInput))
				a.currentOperation = nil
				if !errors.Is(err, errFactsPending) {
					t.Fatalf("later trade did not retain its reduction obligation: %v", err)
				}
				id, _ := paperTradeIdentity("queued-debt-session", event)
				later, err := store.GetObservation(ctx, key, id)
				var frozen paperTradeFact
				if err != nil || decodeInternal(later.Payload, &frozen) != nil || len(frozen.Fills) != 0 || later.Sequence != sequence+1 {
					t.Fatalf("unsent order matched a later trade: %+v err=%v", frozen, err)
				}
			}
			for i := 0; i < 8; i++ {
				err = a.drainFacts(ctx)
				if err == nil {
					break
				}
				if !errors.Is(err, errFactsPending) {
					t.Fatal(err)
				}
			}
			if err != nil || a.account.ProcessedSequence != a.account.ObservedSequence || len(a.account.Reservations) != 1 || a.state.Child == nil {
				t.Fatalf("last trailer released hold or failed reduction: account=%+v child=%+v err=%v", a.account, a.state.Child, err)
			}
			wantBTC, wantUSD, wantFills := "1", "1000", 0
			a.progressCancellation()
			a.finishControl()
			intent, err := store.GetIntent(ctx, key, childID)
			if err != nil || intent.State != "obsolete" || a.state.Child != nil || len(a.account.Reservations) != 0 || len(a.account.Intents) != 0 || len(a.state.Fills) != wantFills || !findBalance(a.account.Balances, "BTC").Total.Equal(d.MustDecimal(wantBTC)) || !findBalance(a.account.Balances, "USD").Total.Equal(d.MustDecimal(wantUSD)) || a.account.ObservedSequence != a.account.ProcessedSequence {
				t.Fatalf("caught-up disposal changed economics/finality: account=%+v state=%+v intent=%+v err=%v", a.account, a.state, intent, err)
			}
			if control == "cancel" && a.state.Parent.Status != "cancelled" || control == "pause" && a.state.Parent.Status != "paused" || control == "gap" && (a.state.Book.Valid || a.account.Ready || !a.state.RecoveryRequired) {
				t.Fatalf("incorrect control/readiness status: %+v", a.state)
			}
		})
	}
}

func TestAttemptedPartialFillDebtPrecedesCancellationFinality(t *testing.T) {
	for _, control := range []string{"pause", "cancel"} {
		t.Run(control, func(t *testing.T) {
			store, key, _, a := actorTestReady(t, 0)
			actorTestCreateAndStart(t, a, "attempted-debt-plan")
			queued := actorTestQueue(t, a, d.PassiveNormal, "attempted-debt-queue")
			childID := queued.State.Child.ID
			if err := actorTestDispatch(t, a, childID, "attempted-debt-dispatch"); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			if err := a.Close(ctx); err != nil {
				t.Fatal(err)
			}
			fact := paperTradeFact{ID: "attempted-debt-source", InputDigest: "attempted-debt-input", IntentID: childID, Event: actorTestTrade("attempted-debt-trade", "0.01", 1, 1), QueueAhead: d.Zero, Fills: []d.Fill{{ExecutionID: "attempted-debt-fill", OrderID: childID, Side: "buy", Quantity: d.MustDecimal("0.01"), Price: d.MustDecimal("99"), Fee: d.MustDecimal("0.00099"), FeeCurrency: "quote", Maker: true, Time: 1}}}
			sequence, err := store.RecordPaperObservation(ctx, key, a.owner, pg.Observation{ID: fact.ID, Kind: paperTradeKind, Payload: jsonBytes(fact)})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := a.command(ctx, Command{ID: "attempted-debt-control", Kind: control, ExpectedRevision: a.account.Revision}); err != nil {
				t.Fatal(err)
			}
			if a.state.Child == nil || len(a.account.Reservations) != 1 || a.state.Child.Order.State != "cancel_requested" {
				t.Fatal("attempted obligation lost before source reduction")
			}
			a.progressCancellation()
			progress, err := a.reduceFactsQuantum(ctx, factBudget{MaxSteps: 1, MaxElapsed: time.Second})
			if err != nil || !progress.More || a.account.ProcessedSequence >= sequence || len(a.state.Fills) != 1 || a.state.Child == nil || len(a.account.Reservations) != 1 {
				t.Fatalf("partial-fill checkpoint lost obligation: progress=%+v state=%+v account=%+v err=%v", progress, a.state, a.account, err)
			}
			for i := 0; i < 8; i++ {
				err = a.drainFacts(ctx)
				if err == nil {
					break
				}
				if !errors.Is(err, errFactsPending) {
					t.Fatal(err)
				}
			}
			a.finishControl()
			status := "paused"
			if control == "cancel" {
				status = "cancelled"
			}
			if err != nil || a.state.Parent.Status != status || a.state.Child != nil || len(a.account.Reservations) != 0 || len(a.account.Intents) != 0 || len(a.state.Fills) != 1 || !a.state.Fills[0].Equal(fact.Fills[0]) || !findBalance(a.account.Balances, "BTC").Total.Equal(d.MustDecimal("1.01")) || !findBalance(a.account.Balances, "USD").Total.Equal(d.MustDecimal("999.00901")) || a.account.ProcessedSequence != a.account.ObservedSequence {
				t.Fatalf("known-final cancellation lost partial fill: state=%+v account=%+v err=%v", a.state, a.account, err)
			}
		})
	}
}

func TestUncertainReloadSurvivesRejectionAndLatePolicyBookkeeping(t *testing.T) {
	store, key, _, a := actorTestReady(t, 0)
	actorTestCreateAndStart(t, a, "reload-plan")
	queued := actorTestQueue(t, a, d.PassiveNormal, "reload-queue")
	if err := actorTestDispatch(t, a, queued.State.Child.ID, "reload-dispatch"); err != nil {
		t.Fatal(err)
	}
	stale := actorTestView(t, a)
	fill := d.Fill{ExecutionID: "reload-execution", OrderID: queued.State.Child.ID, Side: "buy", Quantity: d.MustDecimal("0.01"), Price: d.MustDecimal("99"), Fee: d.MustDecimal("0.00099"), FeeCurrency: "quote", Maker: true, Time: 1}
	ctx := context.Background()
	if err := a.ObserveFill(ctx, "reload-observation", fill.OrderID, fill); err != nil {
		t.Fatal(err)
	}
	actorTestDo(t, a, Command{ID: "reload-pause", Kind: "pause"})
	actorTestWaitView(t, a, 2*time.Second, func(v View) bool { return v.State.Child == nil && len(v.Account.Reservations) == 0 })
	if err := a.Close(ctx); err != nil {
		t.Fatal(err)
	}
	// Model a successful commit whose acknowledgement/reload was lost: the DB
	// is newer while the in-memory projection still predates the financial fact.
	a.state, a.account, a.needsReload = stale.State, stale.Account, true
	_, err := a.persistRevision(ctx, stale.State, pg.Command{ID: "reload-stale-write", Kind: "policy_late", ExpectedRevision: stale.Account.Revision}, pg.Change{}, true)
	if !errors.Is(err, pg.ErrStaleRevision) || !a.needsReload {
		t.Fatalf("definite rejection cleared prior uncertainty: reload=%v err=%v", a.needsReload, err)
	}
	a.receivePolicy(policyResult{audit: PolicyDecision{RequestID: "reload-late-policy"}, err: context.DeadlineExceeded})
	account, err := store.GetAccount(ctx, key)
	state, decodeErr := DecodeSnapshot(account.Snapshot)
	if err != nil || decodeErr != nil || a.needsReload || len(state.Fills) != 1 || !state.Fills[0].Equal(fill) || state.Parent.Status != "paused" || state.Child != nil || len(account.Reservations) != 0 || state.PolicyLateOutcome == nil || state.PolicyLateOutcome.RequestID != "reload-late-policy" || !findBalance(account.Balances, "BTC").Total.Equal(d.MustDecimal("1.01")) || !findBalance(account.Balances, "USD").Total.Equal(d.MustDecimal("999.00901")) {
		t.Fatalf("late policy overwrote durable financial/control state: account=%+v state=%+v err=%v decode=%v", account, state, err, decodeErr)
	}
}
