package runtime

import (
	"context"
	"errors"
	"math"
	"reflect"
	"testing"
	"time"

	d "github.com/liquiditylab/liquiditylab/internal/domain"
	x "github.com/liquiditylab/liquiditylab/internal/execution"
	pg "github.com/liquiditylab/liquiditylab/internal/store/postgres"
)

func actorTestReadyAuto(t *testing.T, interval time.Duration) (*pg.Store, pg.Key, string, *Actor) {
	t.Helper()
	store, key, schema := actorTestDB(t)
	cfg := DefaultConfig(store, key)
	cfg.OwnerID = "paper-process-auto-scheduler"
	cfg.AutoExecute = true
	cfg.Execution.DecisionInterval = int64(interval)
	cfg.Execution.MinMessageInterval = 0
	ctx, cancel := actorTestContext(t)
	actor, err := New(ctx, cfg)
	cancel()
	if err != nil {
		t.Fatalf("New automatic scheduler actor: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := actorTestContext(t)
		defer cancel()
		if err := actor.Close(ctx); err != nil {
			t.Errorf("close automatic scheduler actor: %v", err)
		}
	})
	actorTestMarket(t, actor, "auto-snapshot-1")
	actorTestReconcile(t, actor, "auto-reconcile-1")
	return store, key, schema, actor
}

func actorTestWaitView(t *testing.T, actor *Actor, timeout time.Duration, ready func(View) bool) View {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
		view, err := actor.View(ctx)
		cancel()
		if err != nil {
			t.Fatalf("View while waiting: %v", err)
		}
		if ready(view) {
			return view
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("actor condition not reached within %s", timeout)
	return View{}
}

func actorTestAssertStable(t *testing.T, actor *Actor, duration time.Duration, stable func(View) bool) View {
	t.Helper()
	deadline := time.Now().Add(duration)
	var last View
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
		view, err := actor.View(ctx)
		cancel()
		if err != nil {
			t.Fatalf("View while checking stable state: %v", err)
		}
		if !stable(view) {
			t.Fatalf("actor state changed during bounded stability window: %+v", view)
		}
		last = view
		time.Sleep(10 * time.Millisecond)
	}
	return last
}

func actorTestAutoPause(t *testing.T, actor *Actor, id string) {
	t.Helper()
	for attempt := 0; attempt < 100; attempt++ {
		view := actorTestView(t, actor)
		ctx, cancel := actorTestContext(t)
		_, err := actor.Do(ctx, Command{ID: id, ExpectedRevision: view.Account.Revision, Kind: "pause"})
		cancel()
		if err == nil {
			return
		}
		if !errors.Is(err, pg.ErrStaleRevision) {
			t.Fatalf("pause auto-scheduled parent: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("pause could not obtain a current account revision while auto decisions were running")
}

func actorTestAutoReconcile(t *testing.T, actor *Actor, id string) {
	t.Helper()
	for attempt := 0; attempt < 100; attempt++ {
		view := actorTestView(t, actor)
		ctx, cancel := actorTestContext(t)
		_, err := actor.Reconcile(ctx, Reconciliation{
			ID: id, ExpectedRevision: view.Account.Revision,
			Evidence: "test-operator-account-reconciliation", NoExternalOrders: true,
			Balances: view.Account.Balances,
		})
		cancel()
		if err == nil {
			return
		}
		if !errors.Is(err, pg.ErrStaleRevision) {
			t.Fatalf("reconcile after automatic scheduler gap: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("reconciliation could not obtain a current account revision while auto decisions were running")
}

func TestActorExplicitDecisionUsesPinnedBaselineAndIsIdempotent(t *testing.T) {
	_, _, _, actor := actorTestReady(t, 0)
	plan := actorTestPlan()
	actorTestDo(t, actor, Command{ID: "decision-plan-create", Kind: "create", PlanID: "plan-explicit-decision", Plan: plan, Policy: x.BaselineScheduled})
	actorTestDo(t, actor, Command{ID: "decision-plan-start", Kind: "start"})
	beforeWrong := actorTestView(t, actor)
	if beforeWrong.State.Parent == nil || beforeWrong.State.Parent.Baseline != x.BaselineScheduled {
		t.Fatalf("plan did not pin requested baseline: %+v", beforeWrong.State.Parent)
	}
	ctx, cancel := actorTestContext(t)
	_, err := actor.Do(ctx, Command{
		ID: "decision-wrong-baseline", ExpectedRevision: beforeWrong.Account.Revision,
		Kind: "decision", Policy: x.BaselineLiquidityAware,
	})
	cancel()
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("decision with non-pinned baseline error=%v, want ErrInvalid", err)
	}
	afterWrong := actorTestView(t, actor)
	if beforeWrong.Account.Revision != afterWrong.Account.Revision || beforeWrong.State.DecisionCount != afterWrong.State.DecisionCount ||
		beforeWrong.State.LastInputID != afterWrong.State.LastInputID || !reflect.DeepEqual(beforeWrong.State.LastObservation, afterWrong.State.LastObservation) {
		t.Fatalf("rejected baseline changed decision state: before=%+v after=%+v", beforeWrong, afterWrong)
	}

	decision := Command{
		ID: "decision-explicit-1", ExpectedRevision: afterWrong.Account.Revision,
		Kind: "decision", Policy: x.BaselineScheduled,
	}
	ctx, cancel = actorTestContext(t)
	result, err := actor.Do(ctx, decision)
	cancel()
	if err != nil || result.Duplicate {
		t.Fatalf("first explicit decision: result=%+v err=%v", result, err)
	}
	first := actorTestView(t, actor)
	if first.State.DecisionCount != 1 || first.State.NextSlice != 15*d.Second || len(first.State.LastObservation) != x.ObservationSize {
		t.Fatalf("decision schedule/observation not recorded: count=%d nextSlice=%d observation=%v", first.State.DecisionCount, first.State.NextSlice, first.State.LastObservation)
	}
	for i, value := range first.State.LastObservation {
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			t.Fatalf("shared feature %d is not finite: %v", i, value)
		}
	}
	if first.State.LastObservation[16] != 0 || first.State.LastObservation[17] != 1 || first.State.LastObservation[18] != 1 || first.State.LastObservation[19] != 0 {
		t.Fatalf("observation flags don't match the pre-action account/book/child state: %v", first.State.LastObservation)
	}
	if first.State.Child == nil || first.State.Child.Action != d.AggressiveNormal {
		t.Fatalf("pinned scheduled baseline did not queue its initial aggressive slice: %#v", first.State.Child)
	}
	if err := actorTestDispatch(t, actor, first.State.Child.ID, "decision-explicit-dispatch"); err != nil {
		t.Fatalf("dispatch explicit decision child: %v", err)
	}
	ctx, cancel = actorTestContext(t)
	duplicate, err := actor.Do(ctx, decision)
	cancel()
	if err != nil || !duplicate.Duplicate {
		t.Fatalf("identical decision retry: result=%+v err=%v", duplicate, err)
	}
	afterRetry := actorTestView(t, actor)
	if afterRetry.State.DecisionCount != first.State.DecisionCount || afterRetry.State.NextSlice != first.State.NextSlice ||
		!reflect.DeepEqual(afterRetry.State.LastObservation, first.State.LastObservation) || len(afterRetry.State.Fills) != 1 {
		t.Fatalf("decision retry advanced or changed schedule: before=%+v after=%+v", first.State, afterRetry.State)
	}
}

func TestActorAutomaticSchedulerRespectsReadinessPauseAndDeadline(t *testing.T) {
	store, key, _, actor := actorTestReadyAuto(t, 20*time.Millisecond)
	plan := actorTestPlan()
	plan.Quantity = d.MustDecimal("0.2")
	plan.MaxChildQuantity = d.MustDecimal("0.1")
	plan.Deadline = int64(1800 * time.Millisecond)
	actorTestDo(t, actor, Command{ID: "auto-plan-create", Kind: "create", PlanID: "plan-auto-scheduler", Plan: plan})
	actorTestDo(t, actor, Command{ID: "auto-plan-start", Kind: "start"})
	started := actorTestWaitView(t, actor, time.Second, func(v View) bool {
		return v.State.DecisionCount > 0 && len(v.State.Fills) == 1 && v.Account.ProcessedSequence == v.Account.ObservedSequence
	})
	if started.State.Parent == nil || started.State.Parent.Status != "active" || started.State.Child != nil || started.State.DecisionCount == 0 {
		t.Fatalf("automatic baseline failed to dispatch and fill paper IOC: %+v", started)
	}
	if len(started.State.Fills) == 0 {
		t.Fatal("automatic scheduler reported completion without a durable fill")
	}
	source, err := store.GetObservation(context.Background(), key, "paper-submit-"+started.State.Fills[0].OrderID)
	if err != nil || source.Kind != "transport" {
		t.Fatalf("automatic IOC source outcome not durably recorded: observation=%+v err=%v", source, err)
	}

	ctx, cancel := actorTestContext(t)
	err = actor.Market(ctx, d.MarketEvent{Kind: "gap", Symbol: "BTC/USD", ReceiptIndex: 2, Invalid: true}, "auto-gap")
	cancel()
	if err != nil {
		t.Fatalf("invalidate market for recovery gate: %v", err)
	}
	unready := actorTestView(t, actor)
	if unready.Account.Ready || unready.State.Book.Valid || !unready.State.RecoveryRequired {
		t.Fatalf("gap did not put scheduler behind readiness/recovery gate: %+v", unready)
	}
	stableUnready := actorTestAssertStable(t, actor, 100*time.Millisecond, func(v View) bool {
		return v.State.DecisionCount == unready.State.DecisionCount && !v.Account.Ready && !v.State.Book.Valid
	})

	actorTestMarket(t, actor, "auto-snapshot-after-gap")
	actorTestAutoReconcile(t, actor, "auto-reconcile-after-gap")
	readyAgain := actorTestView(t, actor)
	if !readyAgain.Account.Ready || !readyAgain.State.Book.Valid || readyAgain.State.RecoveryRequired {
		t.Fatalf("fresh book and reconciliation did not clear scheduler gate: %+v", readyAgain)
	}
	actorTestAutoPause(t, actor, "auto-plan-pause")
	paused := actorTestView(t, actor)
	if paused.State.Parent == nil || paused.State.Parent.Status != "paused" {
		t.Fatalf("pause did not stop active auto-scheduled parent: %+v", paused.State.Parent)
	}
	pausedStable := actorTestAssertStable(t, actor, 100*time.Millisecond, func(v View) bool {
		return v.State.DecisionCount == paused.State.DecisionCount && v.State.Parent != nil && v.State.Parent.Status == "paused"
	})
	if pausedStable.State.Child != nil {
		t.Fatalf("paused scheduler left a live child after IOC fill: %#v", pausedStable.State.Child)
	}

	expired := actorTestWaitView(t, actor, 2*time.Second, func(v View) bool {
		return v.State.Parent != nil && v.State.Parent.Status == "expired"
	})
	expiredCount := expired.State.DecisionCount
	actorTestAssertStable(t, actor, 100*time.Millisecond, func(v View) bool {
		return v.State.DecisionCount == expiredCount && v.State.Parent != nil && v.State.Parent.Status == "expired"
	})
	if expiredCount != paused.State.DecisionCount {
		t.Fatalf("automatic decisions advanced after pause/deadline: unready=%d ready=%d paused=%d expired=%d", unready.State.DecisionCount, readyAgain.State.DecisionCount, paused.State.DecisionCount, expiredCount)
	}
	if stableUnready.State.DecisionCount != unready.State.DecisionCount {
		t.Fatalf("automatic decisions advanced while not ready: before=%d after=%d", unready.State.DecisionCount, stableUnready.State.DecisionCount)
	}
}
