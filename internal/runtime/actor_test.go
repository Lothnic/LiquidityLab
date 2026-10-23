package runtime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	d "github.com/liquiditylab/liquiditylab/internal/domain"
	pg "github.com/liquiditylab/liquiditylab/internal/store/postgres"
)

func actorTestDB(t *testing.T) (*pg.Store, pg.Key, string) {
	t.Helper()
	dsn := os.Getenv("LIQUIDITYLAB_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("LIQUIDITYLAB_TEST_POSTGRES_DSN is not set")
	}
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatal(err)
	}
	schema := "ll_actor_test_" + hex.EncodeToString(suffix[:])
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	store, err := pg.Open(ctx, dsn, schema)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Migrate(ctx); err != nil {
		store.Close()
		t.Fatal(err)
	}
	key := pg.Key{TenantID: "actor-test-tenant", AccountID: "actor-test-account"}
	_, err = store.CreateAccount(ctx, key, []pg.Balance{
		{Currency: "USD", Total: d.MustDecimal("1000"), Free: d.MustDecimal("1000")},
		{Currency: "BTC", Total: d.MustDecimal("1"), Free: d.MustDecimal("1")},
	}, json.RawMessage(`{}`))
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		store.Close()
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer dropCancel()
		conn, openErr := pgx.Connect(dropCtx, dsn)
		if openErr != nil {
			t.Errorf("connect to drop isolated test schema: %v", openErr)
			return
		}
		defer conn.Close(dropCtx)
		if _, dropErr := conn.Exec(dropCtx, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE"); dropErr != nil {
			t.Errorf("drop isolated test schema: %v", dropErr)
		}
	})
	return store, key, schema
}

func actorTestContext(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	return context.WithTimeout(context.Background(), 5*time.Second)
}

func actorTestNew(t *testing.T, store *pg.Store, key pg.Key, owner string, lease time.Duration) *Actor {
	t.Helper()
	cfg := DefaultConfig(store, key)
	cfg.OwnerID = owner
	if lease > 0 {
		cfg.Lease = lease
	}
	cfg.Execution.MinMessageInterval = 0
	ctx, cancel := actorTestContext(t)
	defer cancel()
	a, err := New(ctx, cfg)
	if err != nil {
		t.Fatalf("New(%s): %v", owner, err)
	}
	t.Cleanup(func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer closeCancel()
		if err := a.Close(closeCtx); err != nil {
			t.Errorf("close actor %s: %v", owner, err)
		}
	})
	return a
}

func actorTestView(t *testing.T, a *Actor) View {
	t.Helper()
	ctx, cancel := actorTestContext(t)
	defer cancel()
	v, err := a.View(ctx)
	if err != nil {
		t.Fatalf("View: %v", err)
	}
	return v
}

func actorTestDo(t *testing.T, a *Actor, c Command) pg.Result {
	t.Helper()
	if c.ExpectedRevision == 0 {
		c.ExpectedRevision = actorTestView(t, a).Account.Revision
	}
	ctx, cancel := actorTestContext(t)
	defer cancel()
	r, err := a.Do(ctx, c)
	if err != nil {
		t.Fatalf("Do(%s): %v", c.Kind, err)
	}
	return r
}

func actorTestMarket(t *testing.T, a *Actor, id string) {
	t.Helper()
	ctx, cancel := actorTestContext(t)
	defer cancel()
	err := a.Market(ctx, d.MarketEvent{Time: time.Now().UnixNano(), Kind: "snapshot", Symbol: "BTC/USD", ReceiptIndex: 1,
		Bids: []d.Level{{Price: d.MustDecimal("99"), Quantity: d.MustDecimal("2")}},
		Asks: []d.Level{{Price: d.MustDecimal("101"), Quantity: d.MustDecimal("2")}},
	}, id)
	if err != nil {
		t.Fatalf("Market(%s): %v", id, err)
	}
}

func actorTestReconcile(t *testing.T, a *Actor, id string) {
	t.Helper()
	v := actorTestView(t, a)
	ctx, cancel := actorTestContext(t)
	defer cancel()
	_, err := a.Reconcile(ctx, Reconciliation{ID: id, ExpectedRevision: v.Account.Revision,
		Evidence: "test-operator-account-reconciliation", NoExternalOrders: true, Balances: v.Account.Balances})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
}

func actorTestReady(t *testing.T, lease time.Duration) (*pg.Store, pg.Key, string, *Actor) {
	t.Helper()
	store, key, schema := actorTestDB(t)
	a := actorTestNew(t, store, key, "paper-process-a", lease)
	actorTestMarket(t, a, "snapshot-1")
	actorTestReconcile(t, a, "reconcile-1")
	return store, key, schema, a
}

func actorTestPlan() *d.Plan {
	p := d.DefaultPlan()
	p.Deadline = 20 * d.Second
	return &p
}

func actorTestCreateAndStart(t *testing.T, a *Actor, id string) {
	t.Helper()
	actorTestDo(t, a, Command{ID: id + "-create", Kind: "create", PlanID: id, Plan: actorTestPlan()})
	actorTestDo(t, a, Command{ID: id + "-start", Kind: "start"})
}

func actorTestQueue(t *testing.T, a *Actor, action d.Action, id string) View {
	t.Helper()
	actorTestDo(t, a, Command{ID: id, Kind: "action", Action: action})
	v := actorTestView(t, a)
	if v.State.Child == nil || v.State.Child.State != "queued" {
		t.Fatalf("expected queued child, got %#v", v.State.Child)
	}
	return v
}

func actorTestDispatch(t *testing.T, a *Actor, childID, commandID string) error {
	t.Helper()
	v := actorTestView(t, a)
	ctx, cancel := actorTestContext(t)
	defer cancel()
	_, err := a.Do(ctx, Command{ID: commandID, ExpectedRevision: v.Account.Revision, Kind: "dispatch", IntentID: childID})
	return err
}

func TestActorPaperIOCProjectionDurabilityAndCommandIdentity(t *testing.T) {
	store, key, _, a := actorTestReady(t, 0)
	actorTestCreateAndStart(t, a, "plan-ioc")
	queued := actorTestQueue(t, a, d.AggressiveNormal, "action-ioc")
	childID := queued.State.Child.ID
	if queued.State.Child.Order.Intent.Quantity.Cmp(d.MustDecimal("0.05")) != 0 || queued.State.Parent.Spec.Quantity.Cmp(d.MustDecimal("0.1")) != 0 {
		t.Fatalf("queued or parent quantity changed: child=%s parent=%s", queued.State.Child.Order.Intent.Quantity, queued.State.Parent.Spec.Quantity)
	}
	if err := actorTestDispatch(t, a, childID, "dispatch-ioc"); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	v := actorTestView(t, a)
	if v.State.Child != nil || v.State.Parent.Status != "active" || len(v.State.Fills) != 1 {
		t.Fatalf("IOC did not reach a durable final state: parent=%#v child=%#v fills=%#v", v.State.Parent, v.State.Child, v.State.Fills)
	}
	f := v.State.Fills[0]
	if f.Quantity.Cmp(d.MustDecimal("0.05")) != 0 || f.Price.Cmp(d.MustDecimal("101")) != 0 || f.Fee.Cmp(d.MustDecimal("0.0101")) != 0 || f.FeeCurrency != "quote" {
		t.Fatalf("unexpected exact paper fill: %+v", f)
	}
	if findBalance(v.Account.Balances, "BTC").Total.Cmp(d.MustDecimal("1.05")) != 0 || findBalance(v.Account.Balances, "USD").Total.Cmp(d.MustDecimal("994.9399")) != 0 {
		t.Fatalf("unexpected exact financial projection: %+v", v.Account.Balances)
	}
	for _, currency := range []string{"BTC", "USD"} {
		balance := findBalance(v.Account.Balances, currency)
		if !balance.Free.Equal(balance.Total) || !balance.Locked.IsZero() {
			t.Fatalf("known-final IOC retained a reservation for %s: %+v", currency, balance)
		}
	}
	obs, err := store.GetObservation(context.Background(), key, "paper-submit-"+childID)
	if err != nil {
		t.Fatalf("durable paper submission fact: %v", err)
	}
	var fact transportFact
	if err := json.Unmarshal(obs.Payload, &fact); err != nil {
		t.Fatal(err)
	}
	if obs.Kind != "transport" || fact.IntentID != childID || fact.OperationID != "dispatch-ioc" || len(fact.Outcome.Fills) != 1 || !fact.Outcome.KnownFinal {
		t.Fatalf("unexpected durable source outcome: kind=%s fact=%+v", obs.Kind, fact)
	}
	// Duplicate lookup must return the original action result even after later
	// transitions have advanced the account revision.
	ctx, cancel := actorTestContext(t)
	defer cancel()
	dup, err := a.Do(ctx, Command{ID: "action-ioc", ExpectedRevision: queued.Account.Revision - 1, Kind: "action", Action: d.AggressiveNormal})
	if err != nil || !dup.Duplicate {
		t.Fatalf("idempotent public command retry: result=%+v err=%v", dup, err)
	}
	_, err = a.Do(ctx, Command{ID: "action-ioc", ExpectedRevision: queued.Account.Revision - 1, Kind: "action", Action: d.PassiveNormal})
	if !errors.Is(err, pg.ErrConflict) {
		t.Fatalf("mismatched command identity error=%v, want conflict", err)
	}
}

func findBalance(balances []pg.Balance, currency string) pg.Balance {
	for _, b := range balances {
		if b.Currency == currency {
			return b
		}
	}
	return pg.Balance{Currency: currency}
}

func TestActorPauseObsoletesQueuedIntentAndReleasesReservation(t *testing.T) {
	_, _, _, a := actorTestReady(t, 0)
	actorTestCreateAndStart(t, a, "plan-pause-queued")
	queued := actorTestQueue(t, a, d.AggressiveNormal, "action-pause-queued")
	actorTestDo(t, a, Command{ID: "pause-queued", Kind: "pause"})
	v := actorTestView(t, a)
	if v.State.Child != nil || v.State.Parent.Status != "paused" || len(v.Account.Intents) != 0 || len(v.Account.Reservations) != 0 {
		t.Fatalf("pause left queued exposure behind: child=%#v intents=%#v reservations=%#v", v.State.Child, v.Account.Intents, v.Account.Reservations)
	}
	if err := actorTestDispatch(t, a, queued.State.Child.ID, "dispatch-obsolete"); !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("dispatch obsolete queued intent error=%v", err)
	}
}

func TestActorPassiveFillDeduplicationAndKnownFinalCancel(t *testing.T) {
	_, _, _, a := actorTestReady(t, 0)
	actorTestCreateAndStart(t, a, "plan-passive-cancel")
	queued := actorTestQueue(t, a, d.PassiveNormal, "action-passive-cancel")
	childID := queued.State.Child.ID
	if err := actorTestDispatch(t, a, childID, "dispatch-passive-cancel"); err != nil {
		t.Fatalf("passive dispatch: %v", err)
	}
	open := actorTestView(t, a)
	if open.State.Child == nil || open.State.Child.State != "open" || open.State.Child.KnownFinal || open.State.Child.Order.QueueAhead.Cmp(d.MustDecimal("2")) != 0 {
		t.Fatalf("passive paper order not retained with queue ahead: %#v", open.State.Child)
	}
	fill := d.Fill{ExecutionID: "manual-fill-1", OrderID: childID, Side: "buy", Quantity: d.MustDecimal("0.025"), Price: d.MustDecimal("99"), Fee: d.MustDecimal("0.002475"), FeeCurrency: "quote", Time: time.Now().UnixNano(), Maker: true}
	ctx, cancel := actorTestContext(t)
	if err := a.ObserveFill(ctx, "exchange-fill-event-1", childID, fill); err != nil {
		cancel()
		t.Fatalf("ObserveFill: %v", err)
	}
	cancel()
	ctx, cancel = actorTestContext(t)
	err := a.ObserveFill(ctx, "exchange-fill-event-1", childID, fill)
	cancel()
	if err != nil {
		t.Fatalf("duplicate ObserveFill: %v", err)
	}
	filled := actorTestView(t, a)
	if len(filled.State.Fills) != 1 || filled.State.Fills[0].Quantity.Cmp(d.MustDecimal("0.025")) != 0 {
		t.Fatalf("duplicate fill changed ledger: %#v", filled.State.Fills)
	}
	if findBalance(filled.Account.Balances, "BTC").Total.Cmp(d.MustDecimal("1.025")) != 0 || findBalance(filled.Account.Balances, "USD").Total.Cmp(d.MustDecimal("997.522525")) != 0 {
		t.Fatalf("partial fill was not projected exactly: %+v", filled.Account.Balances)
	}
	conflict := fill
	conflict.Quantity = d.MustDecimal("0.04")
	ctx, cancel = actorTestContext(t)
	err = a.ObserveFill(ctx, "exchange-fill-event-1", childID, conflict)
	cancel()
	if !errors.Is(err, pg.ErrConflict) {
		t.Fatalf("conflicting fill identity error=%v, want conflict", err)
	}
	actorTestDo(t, a, Command{ID: "cancel-open-passive", Kind: "action", Action: d.CancelChild})
	closed := actorTestView(t, a)
	if closed.State.Child != nil || len(closed.Account.Reservations) != 0 || len(closed.State.Fills) != 1 {
		t.Fatalf("paper cancel failed to finalize retained order: child=%#v reservations=%#v fills=%#v", closed.State.Child, closed.Account.Reservations, closed.State.Fills)
	}
	obs, err := a.config.Store.GetObservation(context.Background(), a.config.Key, "paper-cancel-"+childID)
	if err != nil {
		t.Fatalf("durable known-final cancel: %v", err)
	}
	var fact transportFact
	if err = json.Unmarshal(obs.Payload, &fact); err != nil || !fact.Outcome.KnownFinal || !fact.Outcome.Accepted {
		t.Fatalf("cancel fact=%+v err=%v", fact, err)
	}
}

func TestActorManualOverfillDoesNotChangeProjection(t *testing.T) {
	_, _, _, a := actorTestReady(t, 0)
	actorTestCreateAndStart(t, a, "plan-overfill")
	queued := actorTestQueue(t, a, d.PassiveNormal, "action-overfill")
	childID := queued.State.Child.ID
	if err := actorTestDispatch(t, a, childID, "dispatch-overfill"); err != nil {
		t.Fatalf("passive dispatch: %v", err)
	}
	before := actorTestView(t, a)
	fill := d.Fill{ExecutionID: "overfill-1", OrderID: childID, Side: "buy", Quantity: d.MustDecimal("0.101"), Price: d.MustDecimal("99"), Fee: d.MustDecimal("0.009999"), FeeCurrency: "quote", Time: time.Now().UnixNano(), Maker: true}
	ctx, cancel := actorTestContext(t)
	err := a.ObserveFill(ctx, "exchange-overfill-event", childID, fill)
	cancel()
	if err == nil {
		t.Fatal("overfill was accepted")
	}
	after := actorTestView(t, a)
	if len(after.State.Fills) != 0 || findBalance(after.Account.Balances, "BTC").Total.Cmp(findBalance(before.Account.Balances, "BTC").Total) != 0 || findBalance(after.Account.Balances, "USD").Total.Cmp(findBalance(before.Account.Balances, "USD").Total) != 0 {
		t.Fatalf("overfill changed financial projection: before=%+v after=%+v", before.Account.Balances, after.Account.Balances)
	}
}

func TestActorRecoveryRequiresPaperProfileProofNewBookAndReconciliation(t *testing.T) {
	store, key, _, a := actorTestReady(t, 0)
	actorTestCreateAndStart(t, a, "plan-restart-first")
	queued := actorTestQueue(t, a, d.AggressiveNormal, "action-restart-first")
	if err := actorTestDispatch(t, a, queued.State.Child.ID, "dispatch-restart-first"); err != nil {
		t.Fatalf("dispatch before restart: %v", err)
	}
	queued = actorTestQueue(t, a, d.AggressiveNormal, "action-restart-second")
	if err := actorTestDispatch(t, a, queued.State.Child.ID, "dispatch-restart-second"); err != nil {
		t.Fatalf("finish plan before restart: %v", err)
	}
	ctx, cancel := actorTestContext(t)
	if err := a.Close(ctx); err != nil {
		cancel()
		t.Fatal(err)
	}
	cancel()
	for _, profile := range []Profile{ProfileLive, ProfileReplay} {
		cfg := DefaultConfig(store, key)
		cfg.Profile = profile
		cfg.OwnerID = "profile-" + string(profile)
		ctx, cancel = actorTestContext(t)
		_, err := New(ctx, cfg)
		cancel()
		if !errors.Is(err, ErrProfile) {
			t.Fatalf("New profile %s error=%v, want ErrProfile", profile, err)
		}
	}
	cfg := DefaultConfig(store, key)
	cfg.OwnerID = "paper-process-without-proof"
	ctx, cancel = actorTestContext(t)
	_, err := New(ctx, cfg)
	cancel()
	if !errors.Is(err, pg.ErrIsolationRequired) {
		t.Fatalf("restart without proof error=%v, want isolation proof", err)
	}
	cfg.OwnerID = "paper-process-recovered"
	cfg.RecoveryProof = pg.RecoveryProof{PreviousSenderIsolated: true, Reconciled: true, Reference: "operator-proof-test"}
	ctx, cancel = actorTestContext(t)
	recovered, err := New(ctx, cfg)
	cancel()
	if err != nil {
		t.Fatalf("New with explicit recovery proof: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := actorTestContext(t)
		defer cancel()
		if err := recovered.Close(ctx); err != nil {
			t.Errorf("close recovered actor: %v", err)
		}
	})
	recoveryPlan := actorTestPlan()
	recoveryPlan.Quantity = d.MustDecimal("0.01")
	recoveryPlan.MaxChildQuantity = d.MustDecimal("0.01")
	recoveryPlan.QuoteBudget = d.MustDecimal("10")
	recoveryPlan.BaseBalance = d.MustDecimal("1.1")
	actorTestDo(t, recovered, Command{ID: "plan-after-restart-create", Kind: "create", PlanID: "plan-after-restart", Plan: recoveryPlan})
	if _, err = func() (pg.Result, error) {
		ctx, cancel := actorTestContext(t)
		defer cancel()
		return recovered.Do(ctx, Command{ID: "plan-after-restart-start-early", ExpectedRevision: actorTestView(t, recovered).Account.Revision, Kind: "start"})
	}(); !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("start before fresh market/reconciliation error=%v", err)
	}
	actorTestMarket(t, recovered, "snapshot-after-restart")
	actorTestReconcile(t, recovered, "reconcile-after-restart")
	actorTestDo(t, recovered, Command{ID: "plan-after-restart-start", Kind: "start"})
	if v := actorTestView(t, recovered); v.State.Parent == nil || v.State.Parent.Status != "active" {
		t.Fatalf("reconciled plan failed to start: %#v", v.State.Parent)
	}
}

func TestActorUnreconciledAndInvalidMarketBlockDispatch(t *testing.T) {
	store, key, _ := actorTestDB(t)
	a := actorTestNew(t, store, key, "paper-unreconciled", 0)
	actorTestDo(t, a, Command{ID: "plan-before-reconcile-create", Kind: "create", PlanID: "plan-before-reconcile", Plan: actorTestPlan()})
	ctx, cancel := actorTestContext(t)
	_, err := a.Do(ctx, Command{ID: "plan-before-reconcile-start", ExpectedRevision: actorTestView(t, a).Account.Revision, Kind: "start"})
	cancel()
	if !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("start without reconciliation error=%v", err)
	}
	actorTestMarket(t, a, "snapshot-before-reconcile")
	actorTestReconcile(t, a, "reconcile-before-action")
	actorTestDo(t, a, Command{ID: "plan-before-reconcile-start2", Kind: "start"})
	queued := actorTestQueue(t, a, d.AggressiveNormal, "action-invalid-market")
	ctx, cancel = actorTestContext(t)
	err = a.Market(ctx, d.MarketEvent{Kind: "gap", Symbol: "BTC/USD", ReceiptIndex: 2, Invalid: true}, "gap-2")
	cancel()
	if err != nil {
		t.Fatalf("gap fact rejected before invalidating market: %v", err)
	}
	if err := actorTestDispatch(t, a, queued.State.Child.ID, "dispatch-invalid-market"); !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("dispatch after invalid market error=%v", err)
	}
	if _, err := store.GetObservation(context.Background(), key, "paper-submit-"+queued.State.Child.ID); !errors.Is(err, pg.ErrNotFound) {
		t.Fatalf("invalid market caused paper handoff, observation err=%v", err)
	}
}

func TestActorLostLeaseCannotMakeNewPaperHandoff(t *testing.T) {
	store, key, schema, a := actorTestReady(t, 90*time.Millisecond)
	actorTestCreateAndStart(t, a, "plan-lease")
	queued := actorTestQueue(t, a, d.AggressiveNormal, "action-lease")
	conn, err := pgx.Connect(context.Background(), os.Getenv("LIQUIDITYLAB_TEST_POSTGRES_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	qualified := pgx.Identifier{schema, "accounts"}.Sanitize()
	if _, err := conn.Exec(context.Background(), "UPDATE "+qualified+" SET lease_until=clock_timestamp()-interval '1 second'"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(180 * time.Millisecond) // allow the actor's 30ms lease renewal loop to observe expiration
	if err := actorTestDispatch(t, a, queued.State.Child.ID, "dispatch-after-lease-loss"); !errors.Is(err, ErrRecoveryRequired) && !errors.Is(err, pg.ErrOwnership) {
		t.Fatalf("dispatch after lease loss error=%v", err)
	}
	if _, err := store.GetObservation(context.Background(), key, "paper-submit-"+queued.State.Child.ID); !errors.Is(err, pg.ErrNotFound) {
		t.Fatalf("expired owner made a paper handoff, observation err=%v", err)
	}
}

func TestActorPublicCommandIdentityRejectsChangedPayload(t *testing.T) {
	_, _, _, a := actorTestReady(t, 0)
	cmd := Command{ID: "stable-create-command", Kind: "create", PlanID: "stable-plan", Plan: actorTestPlan()}
	first := actorTestDo(t, a, cmd)
	actorTestDo(t, a, Command{ID: "stable-start-command", Kind: "start"})
	retry := cmd
	retry.ExpectedRevision = first.Revision - 1
	ctx, cancel := actorTestContext(t)
	result, err := a.Do(ctx, retry)
	cancel()
	if err != nil || !result.Duplicate || result.Revision != first.Revision {
		t.Fatalf("retry did not return original result: result=%+v err=%v", result, err)
	}
	changed := retry
	changed.PlanID = "different-plan"
	ctx, cancel = actorTestContext(t)
	_, err = a.Do(ctx, changed)
	cancel()
	if !errors.Is(err, pg.ErrConflict) {
		t.Fatalf("changed payload with same command ID error=%v", err)
	}
}
