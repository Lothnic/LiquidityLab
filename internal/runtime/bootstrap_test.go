package runtime

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	d "github.com/liquiditylab/liquiditylab/internal/domain"
	x "github.com/liquiditylab/liquiditylab/internal/execution"
	pg "github.com/liquiditylab/liquiditylab/internal/store/postgres"
)

func bootstrapTestReady(t *testing.T) (*pg.Store, pg.Key, string, *Actor) {
	t.Helper()
	store, key, schema := actorTestDB(t)
	cfg := DefaultConfig(store, key)
	cfg.OwnerID = "bootstrap-previous-owner"
	cfg.Execution.DecisionInterval = int64(30 * time.Millisecond)
	cfg.Execution.MinMessageInterval = 0
	ctx, cancel := actorTestContext(t)
	actor, err := New(ctx, cfg)
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	bootstrapTestCleanup(t, actor)
	actorTestMarket(t, actor, "bootstrap-initial-book")
	actorTestReconcile(t, actor, "bootstrap-initial-reconciliation")
	return store, key, schema, actor
}

func bootstrapTestCleanup(t *testing.T, actor *Actor) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := actorTestContext(t)
		defer cancel()
		if err := actor.Close(ctx); err != nil {
			t.Error(err)
		}
	})
}

func bootstrapTestRecoveryConfig(old *Actor, owner string) Config {
	cfg := old.config
	cfg.OwnerID = owner
	cfg.RecoveryProof = pg.RecoveryProof{PreviousSenderIsolated: true, Reconciled: true, Reference: "bootstrap-test-stopped-owner"}
	return cfg
}

func TestBootstrapRejectsRetainedExecutionChangesBeforeClaim(t *testing.T) {
	store, key, _, old := bootstrapTestReady(t)
	ctx, cancel := actorTestContext(t)
	defer cancel()
	if err := old.Close(ctx); err != nil {
		t.Fatal(err)
	}
	before, err := store.GetAccount(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		change func(*Config)
	}{
		{"symbol", func(c *Config) { c.Rules.Symbol = "ETH/USD" }},
		{"tick", func(c *Config) { c.Rules.TickSize = d.MustDecimal("0.02") }},
		{"lot", func(c *Config) { c.Rules.LotSize = d.MustDecimal("0.002") }},
		{"minimum_quantity", func(c *Config) { c.Rules.MinQuantity = d.MustDecimal("0.002") }},
		{"minimum_notional", func(c *Config) { c.Rules.MinNotional = d.MustDecimal("0.02") }},
		{"maker_fee", func(c *Config) { c.Fees.Maker = d.MustDecimal("0.003") }},
		{"taker_fee", func(c *Config) { c.Fees.Taker = d.MustDecimal("0.004") }},
		{"fee_currency", func(c *Config) { c.Fees.Currency = "base" }},
		{"fee_source", func(c *Config) { c.Fees.Source = "another-fee-schedule" }},
		{"decision_interval", func(c *Config) { c.Execution.DecisionInterval++ }},
		{"submission_latency", func(c *Config) { c.Execution.SubmissionLatency++ }},
		{"cancel_latency", func(c *Config) { c.Execution.CancelLatency++ }},
		{"settlement_allowance", func(c *Config) { c.Execution.SettlementAllowance++ }},
		{"miss_penalty", func(c *Config) { c.Execution.LambdaMiss++ }},
		{"participation", func(c *Config) { c.Execution.MaxParticipation = d.MustDecimal("0.3") }},
		{"resting_age", func(c *Config) { c.Execution.MinRestingAge++ }},
		{"message_interval", func(c *Config) { c.Execution.MinMessageInterval++ }},
		{"fill_model", func(c *Config) { c.Execution.FillModel = "another-fill-model" }},
		{"base_currency", func(c *Config) { c.BaseCurrency = "ETH" }},
		{"quote_currency", func(c *Config) { c.QuoteCurrency = "EUR" }},
		{"instrument_revision", func(c *Config) { c.InstrumentRevision = "changed-rules-v2" }},
		{"data_source", func(c *Config) { c.DataSource = "synthetic" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := bootstrapTestRecoveryConfig(old, "rejected-bootstrap-owner")
			tc.change(&cfg)
			if actor, err := New(ctx, cfg); !errors.Is(err, ErrInvalid) || actor != nil {
				if actor != nil {
					_ = actor.Close(ctx)
				}
				t.Fatalf("changed immutable configuration was accepted: actor=%v err=%v", actor, err)
			}
			after, err := store.GetAccount(ctx, key)
			if err != nil {
				t.Fatal(err)
			}
			if after.Revision != before.Revision || after.Ownership != before.Ownership || inputDigest(after.Snapshot) != inputDigest(before.Snapshot) || inputDigest(after.Balances) != inputDigest(before.Balances) {
				t.Fatal("incompatible bootstrap claimed or changed the retained account")
			}
		})
	}
}

func TestBootstrapStatePreservesLegacyDefaultsAndRechecksBundleSettlement(t *testing.T) {
	cfg := DefaultConfig(nil, pg.Key{})
	cfg.DataSource = "manual"
	state := State{Version: StateVersion, Profile: ProfilePaper, Rules: cfg.Rules, Fees: cfg.Fees, Execution: cfg.Execution, BaseCurrency: cfg.BaseCurrency, QuoteCurrency: cfg.QuoteCurrency, InstrumentRevision: cfg.InstrumentRevision, Book: x.NewBook(), Parent: &Parent{Status: "paused"}}
	account := pg.Account{Snapshot: jsonBytes(state)}
	decoded, err := bootstrapState(cfg, account, nil)
	if err != nil || decoded.DataSource != "manual" || decoded.Parent.Baseline != x.BaselineScheduled {
		t.Fatalf("legacy defaults did not load: state=%+v err=%v", decoded, err)
	}
	state.Parent.Baseline = x.BaselineLiquidityAware
	account.Snapshot = jsonBytes(state)
	decoded, err = bootstrapState(cfg, account, nil)
	if err != nil || decoded.Parent.Baseline != x.BaselineLiquidityAware {
		t.Fatal("configured default replaced an existing parent's baseline")
	}
	state.Parent.Status = "cancelled"
	state.PolicyBundle = &PolicyBundle{PolicySHA256: strings.Repeat("a", 64)}
	state.Parent.PolicyBundle = state.PolicyBundle
	account.Snapshot = jsonBytes(state)
	next := &PolicyBundle{PolicySHA256: strings.Repeat("b", 64)}
	decoded, err = bootstrapState(cfg, account, next)
	if err != nil || decoded.PolicyBundle.PolicySHA256 != next.PolicySHA256 || decoded.Parent.PolicyBundle.PolicySHA256 != state.PolicyBundle.PolicySHA256 {
		t.Fatal("settled terminal account could not pin its next bundle independently")
	}
	account.ObservedSequence = 1
	if _, err := bootstrapState(cfg, account, next); !errors.Is(err, ErrInvalid) {
		t.Fatalf("fresh pending fact did not block bundle replacement: %v", err)
	}
}

func TestBootstrapPausesActiveBaselineUntilExplicitResume(t *testing.T) {
	store, key, _, old := bootstrapTestReady(t)
	actorTestCreateAndStart(t, old, "bootstrap-baseline-plan")
	before := actorTestView(t, old)
	ctx, cancel := actorTestContext(t)
	defer cancel()
	if err := old.Close(ctx); err != nil {
		t.Fatal(err)
	}
	cfg := bootstrapTestRecoveryConfig(old, "bootstrap-baseline-recovered")
	cfg.AutoExecute = true
	recovered, err := New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	bootstrapTestCleanup(t, recovered)
	after := actorTestView(t, recovered)
	if after.State.Parent.Status != "paused" || after.State.Parent.Revision != before.State.Parent.Revision+1 || after.Account.Ready || after.State.Book.Valid {
		t.Fatalf("active baseline was not paused at bootstrap: %+v", after.State.Parent)
	}
	durable, err := store.GetAccount(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeSnapshot(durable.Snapshot)
	if err != nil || decoded.Parent.Status != "paused" || len(durable.Plans) != 1 || durable.Plans[0].Status != "paused" || durable.Plans[0].Revision != after.State.Parent.Revision {
		t.Fatalf("baseline pause did not persist with its plan row: err=%v plans=%+v", err, durable.Plans)
	}
	actorTestMarket(t, recovered, "bootstrap-recovered-book")
	actorTestReconcile(t, recovered, "bootstrap-recovered-reconciliation")
	actorTestAssertStable(t, recovered, 150*time.Millisecond, func(v View) bool {
		return v.Account.Ready && v.State.Parent.Status == "paused" && v.State.DecisionCount == 0 && len(v.State.Fills) == 0 && v.State.Child == nil
	})
	actorTestDo(t, recovered, Command{ID: "bootstrap-explicit-resume", Kind: "resume"})
	actorTestWaitView(t, recovered, time.Second, func(v View) bool { return len(v.State.Fills) == 1 })
}

// The row lock queues a real old-owner reducer before the new ownership claim.
// New can read the committed snapshot while those writers wait; releasing the
// lock makes the fill commit between New's first read and its ownership claim.
func TestBootstrapReconstructsFreshPostClaimFillState(t *testing.T) {
	store, key, schema, old := bootstrapTestReady(t)
	actorTestCreateAndStart(t, old, "bootstrap-fill-plan")
	queued := actorTestQueue(t, old, d.PassiveNormal, "bootstrap-passive-action")
	if err := actorTestDispatch(t, old, queued.State.Child.ID, "bootstrap-passive-dispatch"); err != nil {
		t.Fatal(err)
	}
	before := actorTestView(t, old)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := old.Close(ctx); err != nil {
		t.Fatal(err)
	}
	cfg := bootstrapTestRecoveryConfig(old, "bootstrap-fill-recovered")
	fill := d.Fill{ExecutionID: "bootstrap-concurrent-fill", OrderID: before.State.Child.ID, Side: "buy", Quantity: d.MustDecimal("0.01"), Price: before.State.Child.Order.Intent.Price, FeeCurrency: "quote", Maker: true}
	fill.Fee = fill.Price.Mul(fill.Quantity).Mul(before.State.Fees.Maker)
	fact := Fact{ID: "bootstrap-concurrent-fill-observation", IntentID: fill.OrderID, Fill: fill}
	sequence, err := store.RecordObservation(ctx, key, pg.Observation{ID: fact.ID, Kind: "fill", Payload: jsonBytes(fact)})
	if err != nil {
		t.Fatal(err)
	}
	blocker, err := pgx.Connect(ctx, os.Getenv("LIQUIDITYLAB_TEST_POSTGRES_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close(ctx)
	lock, err := blocker.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback(ctx)
	if _, err := lock.Exec(ctx, "SELECT 1 FROM "+pgx.Identifier{schema, "accounts"}.Sanitize()+" WHERE tenant_id=$1 AND account_id=$2 FOR UPDATE", key.TenantID, key.AccountID); err != nil {
		t.Fatal(err)
	}
	observer, err := pgx.Connect(ctx, os.Getenv("LIQUIDITYLAB_TEST_POSTGRES_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	defer observer.Close(ctx)
	drained := make(chan error, 1)
	go func() { drained <- old.drainFacts(ctx) }() // Old loop is stopped; its reducer is serialized here.
	bootstrapTestWaitBlocked(t, ctx, observer, blocker.PgConn().PID(), 1)
	type newResult struct {
		actor *Actor
		err   error
	}
	started := make(chan newResult, 1)
	go func() { actor, err := New(ctx, cfg); started <- newResult{actor, err} }()
	bootstrapTestWaitBlocked(t, ctx, observer, blocker.PgConn().PID(), 2)
	if err := lock.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-drained; err != nil {
		t.Fatalf("old reducer could not commit the accepted fill: %v", err)
	}
	result := <-started
	if result.err != nil {
		t.Fatal(result.err)
	}
	bootstrapTestCleanup(t, result.actor)
	after := actorTestView(t, result.actor)
	if len(after.State.Fills) != 1 || !after.State.Fills[0].Equal(fill) || after.State.Parent.Status != "paused" || !after.State.RecoveryRequired || after.State.Child == nil || after.State.Child.State != "unknown" || !after.State.Child.Order.Remaining.Equal(before.State.Child.Order.Remaining.Sub(fill.Quantity)) || after.Account.ProcessedSequence != sequence || after.Account.ObservedSequence != sequence {
		t.Fatalf("bootstrap lost the fill committed after its initial read: fills=%+v child=%+v parent=%+v recovery=%t observed=%d processed=%d", after.State.Fills, after.State.Child, after.State.Parent, after.State.RecoveryRequired, after.Account.ObservedSequence, after.Account.ProcessedSequence)
	}
	ledger, err := result.actor.ledger(after.State)
	if err != nil {
		t.Fatal(err)
	}
	if !findBalance(after.Account.Balances, "BTC").Total.Equal(ledger.Base) || !findBalance(after.Account.Balances, "USD").Total.Equal(ledger.Quote) || len(after.Account.Intents) != 1 || len(after.Account.Reservations) != 1 {
		t.Fatalf("recovered ledger and retained financial obligations disagree: balances=%+v intents=%+v reservations=%+v", after.Account.Balances, after.Account.Intents, after.Account.Reservations)
	}
	durable, err := store.GetAccount(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeSnapshot(durable.Snapshot)
	if err != nil || len(decoded.Fills) != 1 || !decoded.Fills[0].Equal(fill) || decoded.Parent.Status != "paused" {
		t.Fatalf("fresh recovered fill was not durably checkpointed: err=%v fills=%+v", err, decoded.Fills)
	}
}

func bootstrapTestWaitBlocked(t *testing.T, ctx context.Context, observer *pgx.Conn, blocker uint32, count int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var blocked int
		err := observer.QueryRow(ctx, `WITH RECURSIVE waiters(pid) AS (
 SELECT $1::integer
 UNION
 SELECT a.pid FROM pg_stat_activity a JOIN waiters w ON w.pid=ANY(pg_blocking_pids(a.pid))
) SELECT count(*)-1 FROM waiters`, int64(blocker)).Scan(&blocked)
		if err != nil {
			t.Fatal(err)
		}
		if blocked >= count {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("expected %d writers waiting behind account row lock", count)
}
