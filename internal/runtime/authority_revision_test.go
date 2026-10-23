package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	d "github.com/liquiditylab/liquiditylab/internal/domain"
	x "github.com/liquiditylab/liquiditylab/internal/execution"
	pg "github.com/liquiditylab/liquiditylab/internal/store/postgres"
)

func authorityCommand(v View, id, kind string) Command {
	authority := v.AuthorityRevision()
	return Command{ID: id, Kind: kind, ExpectedRevision: v.Account.Revision, ExpectedAuthorityRevision: &authority}
}

func assertAuthorityStale(t *testing.T, actor *Actor, command Command) {
	t.Helper()
	ctx, cancel := actorTestContext(t)
	defer cancel()
	if _, err := actor.Do(ctx, command); !errors.Is(err, pg.ErrStaleRevision) {
		t.Fatalf("command %s error=%v, want stale revision", command.ID, err)
	}
}

func TestActorAuthorityRevisionAllowsCommandsAcrossSourceOnlyTrades(t *testing.T) {
	store, key, _, actor := actorTestReady(t, 0)
	preview := actorTestView(t, actor)
	if preview.AuthorityRevision() != preview.Account.Revision {
		t.Fatalf("ready state authority=%d, journal=%d", preview.AuthorityRevision(), preview.Account.Revision)
	}
	for i := range 5 {
		event := actorTestTrade(fmt.Sprintf("idle-source-%d", i), "0.1", uint64(i+2), time.Now().UnixNano())
		if err := actorTestMarketFromSession(t, actor, event, fmt.Sprintf("idle-market-%d", i), "idle-session"); err != nil {
			t.Fatal(err)
		}
	}
	current := actorTestView(t, actor)
	if current.Account.Revision <= preview.Account.Revision || current.Account.ProcessedSequence != preview.Account.ProcessedSequence+5 || current.AuthorityRevision() != preview.AuthorityRevision() {
		t.Fatalf("source receipts must advance durable cursor independently: before=%+v after=%+v", preview.Account, current.Account)
	}
	var persisted State
	if err := json.Unmarshal(current.Account.Snapshot, &persisted); err != nil || persisted.AuthorityRevision != preview.AuthorityRevision() {
		t.Fatalf("authority marker was not persisted: marker=%d err=%v", persisted.AuthorityRevision, err)
	}
	create := authorityCommand(preview, "source-tolerant-create", "create")
	create.PlanID, create.Plan = "source-tolerant-plan", actorTestPlan()
	legacy := create
	legacy.ID, legacy.ExpectedAuthorityRevision = "legacy-stale-create", nil
	assertAuthorityStale(t, actor, legacy)
	for _, revision := range []int64{preview.AuthorityRevision() - 1, current.Account.Revision + 1} {
		invalid := create
		invalid.ID, invalid.ExpectedRevision = fmt.Sprintf("invalid-revision-%d", revision), revision
		assertAuthorityStale(t, actor, invalid)
	}
	wrongToken := create
	wrong := preview.AuthorityRevision() - 1
	wrongToken.ID, wrongToken.ExpectedAuthorityRevision = "wrong-token-create", &wrong
	assertAuthorityStale(t, actor, wrongToken)

	committed := actorTestDo(t, actor, create)
	created := actorTestView(t, actor)
	if committed.Revision != current.Account.Revision+1 || created.AuthorityRevision() != committed.Revision {
		t.Fatalf("create did not advance authority: result=%+v view=%+v", committed, created)
	}
	// A caller that missed the successful response can retry its exact request
	// after unrelated source receipt commits, or after another control transition.
	event := actorTestTrade("idle-after-create", "0.1", 10, time.Now().UnixNano())
	if err := actorTestMarketFromSession(t, actor, event, "idle-after-create-market", "idle-session"); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		retry := actorTestDo(t, actor, create)
		if !retry.Duplicate || retry.Revision != committed.Revision {
			t.Fatalf("unknown outcome retry changed result: original=%+v retry=%+v", committed, retry)
		}
	}
	actorTestDo(t, actor, authorityCommand(actorTestView(t, actor), "source-tolerant-start", "start"))
	if retry := actorTestDo(t, actor, create); !retry.Duplicate || retry.Revision != committed.Revision {
		t.Fatalf("retry after control change=%+v", retry)
	}
	changed := create
	changed.ExpectedRevision++
	ctx, cancel := actorTestContext(t)
	defer cancel()
	if _, err := actor.Do(ctx, changed); !errors.Is(err, pg.ErrConflict) {
		t.Fatalf("changed original expected revision error=%v, want idempotency conflict", err)
	}
	changed = create
	changed.ExpectedAuthorityRevision = &wrong
	if _, err := actor.Do(ctx, changed); !errors.Is(err, pg.ErrConflict) {
		t.Fatalf("changed original authority token error=%v, want idempotency conflict", err)
	}
	// Store receipts still compare and record the actual journal revision.
	receipt, err := store.LookupCommand(context.Background(), key, create.ID)
	if err != nil || receipt.ExpectedRevision != current.Account.Revision {
		t.Fatalf("store receipt revision=%d want=%d err=%v", receipt.ExpectedRevision, current.Account.Revision, err)
	}
}

func TestActorAuthorityRevisionRejectsStaleFinancialAndReadinessCommands(t *testing.T) {
	for _, effect := range []string{"reservation", "queue", "fill", "readiness", "control"} {
		t.Run(effect, func(t *testing.T) {
			_, _, _, actor := actorTestReady(t, 0)
			actorTestCreateAndStart(t, actor, "authority-effects")
			if effect == "queue" || effect == "fill" {
				queued := actorTestQueue(t, actor, d.PassiveNormal, "authority-passive")
				if err := actorTestDispatch(t, actor, queued.State.Child.ID, "authority-dispatch"); err != nil {
					t.Fatal(err)
				}
				if effect == "fill" {
					if err := actorTestMarketFromSession(t, actor, actorTestTrade("authority-deplete", "2", 2, time.Now().UnixNano()), "authority-deplete-market", "session"); err != nil {
						t.Fatal(err)
					}
				}
			}
			before := actorTestView(t, actor)
			stale := authorityCommand(before, "stale-after-"+effect, "pause")
			switch effect {
			case "reservation":
				actorTestQueue(t, actor, d.PassiveNormal, "authority-reserve")
			case "queue", "fill":
				quantity := "1"
				if effect == "fill" {
					quantity = "0.01"
				}
				if err := actorTestMarketFromSession(t, actor, actorTestTrade("authority-change", quantity, 3, time.Now().UnixNano()), "authority-change-market", "session"); err != nil {
					t.Fatal(err)
				}
			case "readiness":
				ctx, cancel := actorTestContext(t)
				err := actor.Market(ctx, d.MarketEvent{Kind: "gap", Symbol: "BTC/USD", Invalid: true, ReceiptIndex: 2}, "authority-gap")
				cancel()
				if err != nil {
					t.Fatal(err)
				}
			case "control":
				actorTestDo(t, actor, authorityCommand(before, "authority-control-pause", "pause"))
			}
			after := actorTestView(t, actor)
			if after.AuthorityRevision() <= before.AuthorityRevision() {
				t.Fatalf("%s failed to advance authority: before=%d after=%d", effect, before.AuthorityRevision(), after.AuthorityRevision())
			}
			assertAuthorityStale(t, actor, stale)
		})
	}
}

func TestActorAuthorityRevisionBootstrapsLegacySnapshotAndFencesPreviousOwner(t *testing.T) {
	store, key, schema, old := actorTestReady(t, 0)
	before := actorTestView(t, old)
	ctx, cancel := actorTestContext(t)
	defer cancel()
	if err := old.Close(ctx); err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(ctx, os.Getenv("LIQUIDITYLAB_TEST_POSTGRES_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	_, err = conn.Exec(ctx, "UPDATE "+pgx.Identifier{schema, "accounts"}.Sanitize()+" SET snapshot=snapshot-'authority_revision' WHERE tenant_id=$1 AND account_id=$2", key.TenantID, key.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig(store, key)
	cfg.OwnerID = "authority-new-owner"
	cfg.RecoveryProof = pg.RecoveryProof{PreviousSenderIsolated: true, Reconciled: true, Reference: "authority-owner-proof"}
	recovered, err := New(ctx, cfg)
	if err != nil {
		t.Fatalf("load pre-authority snapshot: %v", err)
	}
	t.Cleanup(func() {
		closeCtx, closeCancel := actorTestContext(t)
		defer closeCancel()
		if err := recovered.Close(closeCtx); err != nil {
			t.Error(err)
		}
	})
	after := actorTestView(t, recovered)
	if after.AuthorityRevision() != after.Account.Revision || after.AuthorityRevision() <= before.Account.Revision {
		t.Fatalf("new ownership did not reset authority: before=%d after=%d journal=%d", before.AuthorityRevision(), after.AuthorityRevision(), after.Account.Revision)
	}
	command := authorityCommand(before, "old-owner-create", "create")
	command.PlanID, command.Plan = "old-owner-plan", actorTestPlan()
	assertAuthorityStale(t, recovered, command)
}

func TestActorAuthorityRevisionPreservesCursorOnlyConsumption(t *testing.T) {
	store, key, _, actor := actorTestReady(t, 0)
	ctx, cancel := actorTestContext(t)
	defer cancel()
	if err := actor.Close(ctx); err != nil {
		t.Fatal(err)
	}
	before := actor.account.Revision
	authority := actor.state.AuthorityRevision
	observation := pg.Observation{ID: "resolved-source-only", Kind: "fill", Payload: json.RawMessage(`{"resolved":true}`)}
	sequence, err := store.RecordObservation(ctx, key, observation)
	if err != nil {
		t.Fatal(err)
	}
	observation.Sequence = sequence
	if err := actor.consumeOnly(ctx, observation); err != nil {
		t.Fatal(err)
	}
	if actor.account.Revision != before+1 || actor.account.ProcessedSequence != sequence || actor.state.AuthorityRevision != authority {
		t.Fatalf("cursor-only consumption changed authority: revision=%d authority=%d sequence=%d", actor.account.Revision, actor.state.AuthorityRevision, actor.account.ProcessedSequence)
	}
}

func TestActorAuthorityRevisionPreservesAutomaticHoldDecisions(t *testing.T) {
	store, key, _, actor := actorTestReadyAuto(t, 20*time.Millisecond)
	plan := actorTestPlan()
	plan.Quantity = d.MustDecimal("0.2")
	plan.MaxChildQuantity = d.MustDecimal("0.1")
	actorTestDo(t, actor, Command{ID: "authority-auto-create", Kind: "create", PlanID: "authority-auto-plan", Plan: plan, Policy: x.BaselineScheduled})
	actorTestDo(t, actor, Command{ID: "authority-auto-start", Kind: "start"})
	before := actorTestWaitView(t, actor, time.Second, func(v View) bool {
		return len(v.State.Fills) == 1 && v.State.Child == nil && v.State.DecisionCount >= 1
	})
	pause := authorityCommand(before, "authority-auto-pause", "pause")
	after := actorTestWaitView(t, actor, time.Second, func(v View) bool {
		return v.State.DecisionCount >= before.State.DecisionCount+3
	})
	if after.Account.Revision <= before.Account.Revision || after.AuthorityRevision() != before.AuthorityRevision() || after.State.Reason != "hold" || after.State.NextDecision <= before.State.NextDecision || len(after.State.LastObservation) != x.ObservationSize {
		t.Fatalf("HOLD bookkeeping changed authority or failed to advance cadence: before=%+v after=%+v", before, after)
	}
	var persisted State
	if err := json.Unmarshal(after.Account.Snapshot, &persisted); err != nil || persisted.DecisionCount < before.State.DecisionCount+3 || persisted.AuthorityRevision != before.AuthorityRevision() {
		t.Fatalf("HOLD feature/cadence checkpoint not durable: state=%+v err=%v", persisted, err)
	}
	journal, err := store.Journal(context.Background(), key, before.Account.Revision, 100)
	if err != nil {
		t.Fatal(err)
	}
	decisions := 0
	for _, entry := range journal {
		if entry.Kind == "decision" {
			decisions++
		}
	}
	if decisions < 3 {
		t.Fatalf("HOLD journal entries=%d want >=3", decisions)
	}
	result := actorTestDo(t, actor, pause)
	paused := actorTestView(t, actor)
	if paused.State.Parent.Status != "paused" || paused.AuthorityRevision() != result.Revision || result.Revision <= after.Account.Revision {
		t.Fatalf("snapshot-bound pause failed across HOLD decisions: result=%+v view=%+v", result, paused)
	}
}

func TestActorAuthorityRevisionDecisionEnqueueAndCancelInvalidate(t *testing.T) {
	for _, effect := range []string{"enqueue", "cancel"} {
		t.Run(effect, func(t *testing.T) {
			store, key, _ := actorTestDB(t)
			cfg := DefaultConfig(store, key)
			cfg.OwnerID = "decision-authority-owner"
			cfg.Execution.MinRestingAge = 0
			cfg.Execution.MinMessageInterval = 0
			ctx, cancel := actorTestContext(t)
			actor, err := New(ctx, cfg)
			cancel()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				ctx, cancel := actorTestContext(t)
				defer cancel()
				if err := actor.Close(ctx); err != nil {
					t.Error(err)
				}
			})
			actorTestMarket(t, actor, "decision-authority-market")
			actorTestReconcile(t, actor, "decision-authority-reconcile")
			actorTestCreateAndStart(t, actor, "decision-authority-plan")
			if effect == "cancel" {
				queued := actorTestQueue(t, actor, d.PassiveNormal, "decision-authority-passive")
				if err := actorTestDispatch(t, actor, queued.State.Child.ID, "decision-authority-dispatch"); err != nil {
					t.Fatal(err)
				}
			}
			before := actorTestView(t, actor)
			decision := authorityCommand(before, "decision-authority-effect", "decision")
			decision.Policy = x.BaselineScheduled
			actorTestDo(t, actor, decision)
			after := actorTestView(t, actor)
			if after.AuthorityRevision() <= before.AuthorityRevision() {
				t.Fatalf("decision %s failed to advance authority", effect)
			}
			if effect == "enqueue" && (after.State.Child == nil || len(after.Account.Reservations) != 1) {
				t.Fatalf("decision did not enqueue and reserve: %+v", after)
			}
			if effect == "cancel" && (after.State.Child != nil || len(after.Account.Reservations) != 0) {
				t.Fatalf("decision did not cancel/finalize and release hold: %+v", after)
			}
			assertAuthorityStale(t, actor, authorityCommand(before, "decision-authority-stale-pause", "pause"))
		})
	}
}
