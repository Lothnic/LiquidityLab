package runtime

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	d "github.com/liquiditylab/liquiditylab/internal/domain"
	x "github.com/liquiditylab/liquiditylab/internal/execution"
	"github.com/liquiditylab/liquiditylab/internal/telemetry"
)

func telemetryTestMarket(id string) (d.MarketEvent, string) {
	return d.MarketEvent{Time: time.Now().UnixNano(), Kind: "snapshot", Symbol: "BTC/USD", ReceiptIndex: 1,
		Bids: []d.Level{{Price: d.MustDecimal("99"), Quantity: d.MustDecimal("2")}},
		Asks: []d.Level{{Price: d.MustDecimal("101"), Quantity: d.MustDecimal("2")}},
	}, id
}

func waitTelemetry(t *testing.T, recorder *telemetry.Recorder, ready func(telemetry.Snapshot) bool) telemetry.Snapshot {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		s := recorder.Snapshot()
		if ready(s) {
			return s
		}
		time.Sleep(2 * time.Millisecond)
	}
	s := recorder.Snapshot()
	t.Fatalf("telemetry condition not reached: %+v", s)
	return telemetry.Snapshot{}
}

func TestActorQueueTelemetryBalancesPublicationRejectionCancellationAndClose(t *testing.T) {
	store, key, schema := actorTestDB(t)
	recorder := telemetry.New()
	cfg := DefaultConfig(store, key)
	cfg.OwnerID = "telemetry-queue-test"
	cfg.MailboxCapacity = 1
	cfg.ControlCapacity = 1
	cfg.Execution.MinMessageInterval = 0
	cfg.Telemetry = recorder
	ctx, cancel := actorTestContext(t)
	actor, err := New(ctx, cfg)
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer closeCancel()
		if err := actor.Close(closeCtx); err != nil {
			t.Errorf("close telemetry actor: %v", err)
		}
	})
	actorTestMarket(t, actor, "telemetry-ready-snapshot")
	actorTestReconcile(t, actor, "telemetry-ready-reconcile")
	actorTestCreateAndStart(t, actor, "telemetry-ready-plan")
	controlBaseline := recorder.Snapshot()
	actorTestDo(t, actor, Command{ID: "telemetry-control-cancel", Kind: "cancel"})
	afterControl := recorder.Snapshot()
	if afterControl.Distributions[telemetry.ControlAckNS].Count <= controlBaseline.Distributions[telemetry.ControlAckNS].Count {
		t.Fatal("successful priority control did not record acknowledgement latency")
	}
	baseline := afterControl

	blockCtx, blockCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer blockCancel()
	blocker, err := pgx.Connect(blockCtx, os.Getenv("LIQUIDITYLAB_TEST_POSTGRES_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close(blockCtx)
	lock, err := blocker.Begin(blockCtx)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback(blockCtx)
	query := "SELECT 1 FROM " + pgx.Identifier{schema, "accounts"}.Sanitize() + " WHERE tenant_id=$1 AND account_id=$2 FOR UPDATE"
	if _, err := lock.Exec(blockCtx, query, key.TenantID, key.AccountID); err != nil {
		t.Fatal(err)
	}

	firstResult := make(chan error, 1)
	firstEvent := d.MarketEvent{Time: time.Now().UnixNano(), Kind: "gap", Symbol: "BTC/USD", Invalid: true}
	firstID := "telemetry-blocking-gap"
	go func() { firstResult <- actor.Market(context.Background(), firstEvent, firstID) }()
	waitTelemetry(t, recorder, func(s telemetry.Snapshot) bool { return s.Gauges[telemetry.ActorHandling].Value == 1 })

	queuedCtx, queuedCancel := context.WithCancel(context.Background())
	queuedResult := make(chan error, 1)
	queuedEvent, queuedID := telemetryTestMarket("telemetry-cancelled-queued-market")
	go func() { queuedResult <- actor.Market(queuedCtx, queuedEvent, queuedID) }()
	waitTelemetry(t, recorder, func(s telemetry.Snapshot) bool { return s.Gauges[telemetry.ActorQueued].Value == 1 })
	fullEvent, fullID := telemetryTestMarket("telemetry-full-market")
	if err := actor.Market(context.Background(), fullEvent, fullID); !errors.Is(err, ErrMailboxFull) {
		t.Fatalf("full queue error = %v", err)
	}
	preCanceled, preCancel := context.WithCancel(context.Background())
	preCancel()
	preCanceledEvent, preCanceledID := telemetryTestMarket("telemetry-pre-cancelled-market")
	if err := actor.Market(preCanceled, preCanceledEvent, preCanceledID); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-cancelled caller error = %v", err)
	}
	queuedCancel()
	if err := <-queuedResult; !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("cancelled admitted caller error = %v", err)
	}

	closeResult := make(chan error, 1)
	go func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer closeCancel()
		closeResult <- actor.Close(closeCtx)
	}()
	deadline := time.Now().Add(time.Second)
	for {
		actor.admission.Lock()
		closing := actor.closing
		actor.admission.Unlock()
		if closing {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("actor did not enter closing state")
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := actor.View(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("post-close publication error = %v", err)
	}
	if err := lock.Commit(blockCtx); err != nil {
		t.Fatal(err)
	}
	if err := <-firstResult; err != nil && !errors.Is(err, ErrClosed) {
		t.Fatalf("first accepted market failed: %v", err)
	}
	if err := <-closeResult; err != nil {
		t.Fatalf("actor close failed: %v", err)
	}
	final := recorder.Snapshot()
	if final.Gauges[telemetry.ActorQueued].Value != 0 || final.Gauges[telemetry.ActorHandling].Value != 0 || final.Gauges[telemetry.ActorQueued].Overflow || final.Gauges[telemetry.ActorHandling].Overflow {
		t.Fatalf("queue/handling gauges did not settle cleanly: queued=%+v handling=%+v", final.Gauges[telemetry.ActorQueued], final.Gauges[telemetry.ActorHandling])
	}
	admitted := final.Counters[telemetry.ActorAdmitted].Value - baseline.Counters[telemetry.ActorAdmitted].Value
	completed := final.Counters[telemetry.ActorCompleted].Value - baseline.Counters[telemetry.ActorCompleted].Value
	failed := final.Counters[telemetry.ActorFailed].Value - baseline.Counters[telemetry.ActorFailed].Value
	rejected := final.Counters[telemetry.ActorRejected].Value - baseline.Counters[telemetry.ActorRejected].Value
	if admitted != 2 || completed+failed != admitted || rejected < 3 {
		t.Fatalf("admission accounting admitted=%d completed=%d failed=%d rejected=%d", admitted, completed, failed, rejected)
	}
	if final.Distributions[telemetry.ActorQueueWaitNS].Count-baseline.Distributions[telemetry.ActorQueueWaitNS].Count != admitted || final.Distributions[telemetry.ActorHandleNS].Count-baseline.Distributions[telemetry.ActorHandleNS].Count != admitted {
		t.Fatalf("elapsed samples missing for accepted work: queue=%+v handling=%+v", final.Distributions[telemetry.ActorQueueWaitNS], final.Distributions[telemetry.ActorHandleNS])
	}
}

func TestTelemetryDecisionCommitRecordsOnlyNewAcknowledgedDecision(t *testing.T) {
	store, key, _ := actorTestDB(t)
	recorder := telemetry.New()
	cfg := DefaultConfig(store, key)
	cfg.OwnerID = "telemetry-decision-test"
	cfg.Execution.MinMessageInterval = 0
	cfg.Telemetry = recorder
	ctx, cancel := actorTestContext(t)
	actor, err := New(ctx, cfg)
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer closeCancel()
		if err := actor.Close(closeCtx); err != nil {
			t.Errorf("close telemetry decision actor: %v", err)
		}
	})
	actorTestMarket(t, actor, "telemetry-decision-book")
	actorTestReconcile(t, actor, "telemetry-decision-reconcile")
	actorTestCreateAndStart(t, actor, "telemetry-decision-plan")
	before := actorTestView(t, actor)
	decision := Command{ID: "telemetry-decision-once", ExpectedRevision: before.Account.Revision, Kind: "decision", Policy: x.BaselineScheduled}
	actorTestDo(t, actor, decision)
	afterFirst := recorder.Snapshot()
	if got := afterFirst.Distributions[telemetry.DecisionCommitNS].Count; got != 1 {
		t.Fatalf("first acknowledged decision latency samples = %d, want 1", got)
	}
	ctx, cancel = actorTestContext(t)
	duplicate, err := actor.Do(ctx, decision)
	cancel()
	if err != nil || !duplicate.Duplicate {
		t.Fatalf("duplicate decision retry result=%+v err=%v", duplicate, err)
	}
	afterDuplicate := recorder.Snapshot()
	if got := afterDuplicate.Distributions[telemetry.DecisionCommitNS].Count; got != 1 {
		t.Fatalf("duplicate retry added decision latency sample: count=%d", got)
	}
}

func TestPolicyTelemetryLateOutcomeDoesNotCountTwice(t *testing.T) {
	w := newControlledPolicy()
	recorder := telemetry.New()
	actor, _, _ := policyTestActor(t, w, "pause", 150*time.Millisecond, func(cfg *Config) { cfg.Telemetry = recorder })
	policyTestStart(t, actor)
	request := policyTestCall(t, w)
	actorTestDo(t, actor, Command{ID: "telemetry-policy-cancel", Kind: "cancel"})
	resolved := actorTestWaitView(t, actor, time.Second, func(v View) bool { return v.State.PolicyPending == nil && v.State.LastPolicyDecision != nil })
	if resolved.State.LastPolicyDecision.RequestID != request.Binding.RequestId {
		t.Fatalf("resolved decision request=%q want=%q", resolved.State.LastPolicyDecision.RequestID, request.Binding.RequestId)
	}
	beforeLate := recorder.Snapshot()
	if beforeLate.Counters[telemetry.PolicyDecisions].Value != 1 || beforeLate.Counters[telemetry.PolicyFallbacks].Value != 1 || beforeLate.Distributions[telemetry.DecisionCommitNS].Count != 1 {
		t.Fatalf("policy resolution telemetry = counters %+v decision latency %+v", beforeLate.Counters, beforeLate.Distributions[telemetry.DecisionCommitNS])
	}
	w.release <- d.AggressiveNormal
	actorTestWaitView(t, actor, time.Second, func(v View) bool { return v.State.PolicyLateOutcome != nil })
	afterLate := recorder.Snapshot()
	if afterLate.Counters[telemetry.PolicyDecisions].Value != beforeLate.Counters[telemetry.PolicyDecisions].Value || afterLate.Counters[telemetry.PolicyFallbacks].Value != beforeLate.Counters[telemetry.PolicyFallbacks].Value || afterLate.Distributions[telemetry.DecisionCommitNS].Count != beforeLate.Distributions[telemetry.DecisionCommitNS].Count {
		t.Fatalf("late worker response changed decision telemetry: before=%+v after=%+v", beforeLate.Counters, afterLate.Counters)
	}
}

func TestPolicyTelemetryDoesNotFabricateRestartLatency(t *testing.T) {
	w := newControlledPolicy()
	recorder := telemetry.New()
	actor, _, _ := policyTestActor(t, w, "baseline", 150*time.Millisecond, func(cfg *Config) { cfg.Telemetry = recorder })
	policyTestStart(t, actor)
	policyTestCall(t, w)
	if got := recorder.Snapshot().Distributions[telemetry.DecisionCommitNS].Count; got != 0 {
		t.Fatalf("pending worker call recorded unacknowledged latency: %d", got)
	}
	ctx, cancel := actorTestContext(t)
	if err := actor.Close(ctx); err != nil {
		cancel()
		t.Fatal(err)
	}
	cancel()
	cfg := actor.config
	cfg.OwnerID = "telemetry-restart-owner"
	restarted, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer closeCancel()
		if err := restarted.Close(closeCtx); err != nil {
			t.Errorf("close restarted telemetry actor: %v", err)
		}
	}()
	if got := recorder.Snapshot().Distributions[telemetry.DecisionCommitNS].Count; got != 0 {
		t.Fatalf("recovery fabricated process-local decision latency: %d", got)
	}
	w.release <- d.Hold
}
