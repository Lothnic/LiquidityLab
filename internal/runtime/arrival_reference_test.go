package runtime

import (
	"testing"
	"time"

	d "github.com/liquiditylab/liquiditylab/internal/domain"
	x "github.com/liquiditylab/liquiditylab/internal/execution"
)

func TestActorArrivalReferenceFrozenAcrossDecisionAndResume(t *testing.T) {
	_, _, _, actor := actorTestReady(t, 0)
	actorTestCreateAndStart(t, actor, "arrival-plan")

	started := actorTestView(t, actor)
	if started.State.Parent == nil || !started.State.Parent.ArrivalMid.Equal(d.MustDecimal("100")) {
		t.Fatalf("initial arrival midpoint = %v, want 100", started.State.Parent)
	}
	if !started.State.ReferenceMid.Equal(d.MustDecimal("100")) {
		t.Fatalf("initial reference midpoint = %s, want 100", started.State.ReferenceMid)
	}

	// A known paper IOC fill at 101 establishes exact executed cost against the
	// frozen arrival midpoint before the market moves.
	queued := actorTestQueue(t, actor, d.AggressiveNormal, "arrival-first-slice")
	if err := actorTestDispatch(t, actor, queued.State.Child.ID, "arrival-first-dispatch"); err != nil {
		t.Fatalf("dispatch initial IOC: %v", err)
	}
	filled := actorTestView(t, actor)
	if filled.State.Parent == nil || len(filled.State.Fills) != 1 {
		t.Fatalf("expected one durable IOC fill, parent=%+v fills=%+v", filled.State.Parent, filled.State.Fills)
	}

	ctx, cancel := actorTestContext(t)
	err := actor.Market(ctx, d.MarketEvent{
		Time: time.Now().UnixNano(), Kind: "snapshot", Symbol: "BTC/USD", ReceiptIndex: 2,
		Bids: []d.Level{{Price: d.MustDecimal("101"), Quantity: d.MustDecimal("2")}},
		Asks: []d.Level{{Price: d.MustDecimal("103"), Quantity: d.MustDecimal("2")}},
	}, "arrival-next-snapshot")
	cancel()
	if err != nil {
		t.Fatalf("apply changed book: %v", err)
	}

	view := actorTestView(t, actor)
	decision := Command{ID: "arrival-new-mid-decision", ExpectedRevision: view.Account.Revision, Kind: "decision", Policy: view.State.Parent.Baseline}
	ctx, cancel = actorTestContext(t)
	_, err = actor.Do(ctx, decision)
	cancel()
	if err != nil {
		t.Fatalf("decision after midpoint change: %v", err)
	}
	decided := actorTestView(t, actor)
	if !decided.State.ReferenceMid.Equal(d.MustDecimal("102")) {
		t.Fatalf("latest decision midpoint = %s, want 102", decided.State.ReferenceMid)
	}
	if !decided.State.Parent.ArrivalMid.Equal(d.MustDecimal("100")) {
		t.Fatalf("decision changed frozen arrival midpoint: %s", decided.State.Parent.ArrivalMid)
	}
	if len(decided.State.LastObservation) != x.ObservationSize || decided.State.DecisionCount < 1 {
		t.Fatalf("shared observation not recorded: count=%d features=%v", decided.State.DecisionCount, decided.State.LastObservation)
	}

	actorTestDo(t, actor, Command{ID: "arrival-pause", Kind: "pause"})
	paused := actorTestView(t, actor)
	if paused.State.Parent.Status != "paused" || !paused.State.Parent.ArrivalMid.Equal(d.MustDecimal("100")) {
		t.Fatalf("pause changed arrival reference: parent=%+v", paused.State.Parent)
	}
	actorTestDo(t, actor, Command{ID: "arrival-resume", Kind: "resume"})
	resumed := actorTestView(t, actor)
	if resumed.State.Parent.Status != "active" || !resumed.State.Parent.ArrivalMid.Equal(d.MustDecimal("100")) {
		t.Fatalf("resume changed arrival reference: parent=%+v", resumed.State.Parent)
	}

	decoded, err := DecodeSnapshot(resumed.Account.Snapshot)
	if err != nil {
		t.Fatalf("decode durable state snapshot: %v", err)
	}
	if decoded.Parent == nil || !decoded.Parent.ArrivalMid.Equal(d.MustDecimal("100")) || !decoded.ReferenceMid.Equal(d.MustDecimal("102")) {
		t.Fatalf("durable snapshot lost arrival/current reference distinction: arrival=%v reference=%s", decoded.Parent, decoded.ReferenceMid)
	}

	ledger := x.NewLedger(resumed.State.Parent.Spec)
	for _, fill := range resumed.State.Fills {
		if _, err := ledger.Apply(fill); err != nil {
			t.Fatalf("apply durable fill to shared ledger: %v", err)
		}
	}
	arrival := resumed.State.Parent.ArrivalMid
	if !ledger.Remaining().Equal(d.MustDecimal("0.05")) {
		t.Fatalf("remaining quantity = %s, want 0.05", ledger.Remaining())
	}
	if got, want := ledger.MarkedCost(arrival, arrival), d.MustDecimal("0.0601"); !got.Equal(want) {
		t.Fatalf("executed cost at arrival midpoint = %s, want %s", got, want)
	}
	if got, want := ledger.MarkedCost(arrival, d.MustDecimal("102")), d.MustDecimal("0.1601"); !got.Equal(want) {
		t.Fatalf("current residual-marked cost = %s, want %s", got, want)
	}
}
