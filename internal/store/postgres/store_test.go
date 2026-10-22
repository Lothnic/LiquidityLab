package postgres

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	d "github.com/liquiditylab/liquiditylab/internal/domain"
)

func amount(s string) d.Decimal    { return d.MustDecimal(s) }
func raw(s string) json.RawMessage { return json.RawMessage(s) }
func testStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("LIQUIDITYLAB_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set LIQUIDITYLAB_TEST_POSTGRES_DSN for isolated PostgreSQL integration checks")
	}
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatal(err)
	}
	schema := "ll_test_" + hex.EncodeToString(suffix[:])
	ctx := context.Background()
	s, err := Open(ctx, dsn, schema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := s.pool.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Error(err)
		}
		s.Close()
	})
	if err = s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return s
}
func initialized(t *testing.T, s *Store) (Key, Ownership, Account) {
	t.Helper()
	ctx := context.Background()
	key := Key{"tenant-a", "account-a"}
	_, err := s.CreateAccount(ctx, key, []Balance{{"USD", amount("1000"), amount("1000"), d.Zero}, {"BTC", amount("1"), amount("1"), d.Zero}}, raw(`{"schema":"paper-v1"}`))
	if err != nil {
		t.Fatal(err)
	}
	owner, err := s.ClaimOwnership(ctx, key, "worker-a", time.Minute, RecoveryProof{})
	if err != nil {
		t.Fatal(err)
	}
	ready := true
	_, err = s.Apply(ctx, Change{Key: key, Ownership: owner, Command: Command{"ready", "reconcile", 1}, Ready: &ready, Market: &Market{true, "book-1", "rules-1"}, Snapshot: raw(`{"schema":"paper-v1","ready":true}`)})
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.GetAccount(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	return key, owner, a
}
func queuedChange(key Key, owner Ownership, revision int64) Change {
	deadline := time.Now().UTC().Add(time.Minute)
	return Change{Key: key, Ownership: owner, Command: Command{"queue", "start", revision}, Snapshot: raw(`{"schema":"paper-v1","parent":"plan-1"}`), Plan: &Plan{ID: "plan-1", Revision: 1, Status: "active", Deadline: deadline, Detail: raw(`{"side":"buy","quantity":"0.1"}`)}, Enqueue: []Intent{{ID: "intent-1", PlanID: "plan-1", PlanRevision: 1, ExpiresAt: deadline, InstrumentRevision: "rules-1", Payload: raw(`{"side":"buy","quantity":"0.1","price":"101"}`)}}, Reservations: []Reservation{{IntentID: "intent-1", Currency: "USD", Amount: amount("20")}}}
}
func queue(t *testing.T, s *Store, key Key, owner Ownership, revision int64) Result {
	t.Helper()
	r, err := s.Apply(context.Background(), queuedChange(key, owner, revision))
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func authorization(key Key, owner Ownership, revision int64) Authorization {
	return Authorization{Key: key, Ownership: owner, IntentID: "intent-1", ExpectedRevision: revision, PlanRevision: 1, SnapshotID: "book-1", InstrumentRevision: "rules-1", AttemptID: "attempt-1"}
}
func mustAccount(t *testing.T, s *Store, key Key) Account {
	t.Helper()
	a, err := s.GetAccount(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestPostgresMigrationAndExactAmounts(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.Migrate(ctx); err != nil {
		t.Fatal("repeat migration", err)
	}
	x, err := d.ParseInternalDecimal(strings.Repeat("9", 412) + "." + strings.Repeat("1", 100))
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.CreateAccount(ctx, Key{"tenant", "large"}, []Balance{{"USD", x, x, d.Zero}}, raw(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if !a.Balances[0].Free.Equal(x) {
		t.Fatal("numeric roundtrip lost precision")
	}
	for _, value := range []string{"NaN", "Infinity", "-Infinity", "-1", "0." + strings.Repeat("0", 145), "1." + strings.Repeat("0", 145), "0." + strings.Repeat("0", 144) + "1", strings.Repeat("9", 513)} {
		_, err := s.pool.Exec(ctx, "UPDATE balances SET total=$1::numeric,free=$1::numeric WHERE tenant_id='tenant' AND account_id='large'", value)
		if err == nil {
			t.Fatalf("database accepted unsupported amount %q", value)
		}
	}
	if _, err = s.pool.Exec(ctx, "UPDATE schema_migrations SET checksum='tampered' WHERE version=1"); err != nil {
		t.Fatal(err)
	}
	if err = s.Migrate(ctx); !errors.Is(err, ErrSchema) {
		t.Fatal("failed to detect migration drift", err)
	}
}

func TestPostgresAtomicRollbackAndCommandIdentity(t *testing.T) {
	s := testStore(t)
	key, owner, a := initialized(t, s)
	ctx := context.Background()
	change := queuedChange(key, owner, a.Revision)
	change.Reservations[0].Amount = amount("2000")
	if _, err := s.Apply(ctx, change); !errors.Is(err, ErrInsufficientFunds) {
		t.Fatal(err)
	}
	a = mustAccount(t, s, key)
	if a.Revision != 2 || len(a.Plans) != 0 || len(a.Intents) != 0 || len(a.Reservations) != 0 {
		t.Fatal("partial transaction persisted", a)
	}
	change.Reservations[0].Amount = amount("20")
	first, err := s.Apply(ctx, change)
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.Apply(ctx, change)
	if err != nil || !again.Duplicate || again.Revision != first.Revision {
		t.Fatal("command retry", again, err)
	}
	change.Reservations[0].Amount = amount("21")
	if _, err = s.Apply(ctx, change); !errors.Is(err, ErrConflict) {
		t.Fatal("changed duplicate", err)
	}
	other := Change{Key: key, Ownership: owner, Command: Command{"second-parent", "start", 3}, Plan: &Plan{ID: "plan-2", Revision: 1, Status: "active", Deadline: time.Now().Add(time.Minute), Detail: raw(`{}`)}}
	if _, err = s.Apply(ctx, other); !errors.Is(err, ErrConflict) {
		t.Fatal("allowed second active parent", err)
	}
	if _, err = s.GetAccount(ctx, Key{"different-tenant", key.AccountID}); !errors.Is(err, ErrNotFound) {
		t.Fatal("tenant boundary", err)
	}
}

func TestPostgresDispatchUnknownFinalAndExecutionIdentity(t *testing.T) {
	s := testStore(t)
	key, owner, a := initialized(t, s)
	ctx := context.Background()
	q := queue(t, s, key, owner, a.Revision)
	dispatch, err := s.AuthorizeDispatch(ctx, authorization(key, owner, q.Revision))
	if err != nil {
		t.Fatal(err)
	}
	if dispatch.Intent.State != "attempted" || dispatch.Intent.AuthorizationSnapshot != "book-1" {
		t.Fatal(dispatch)
	}
	if _, err = s.AuthorizeDispatch(ctx, authorization(key, owner, dispatch.Revision)); !errors.Is(err, ErrConflict) {
		t.Fatal("attempt blindly authorized twice", err)
	}
	r, err := s.Apply(ctx, Change{Key: key, Ownership: owner, Command: Command{"unknown", "transport_ambiguous", dispatch.Revision}, Resolutions: []Resolution{{IntentID: "intent-1", State: "unknown", Evidence: raw(`{"reason":"connection lost"}`)}}})
	if err != nil {
		t.Fatal(err)
	}
	a = mustAccount(t, s, key)
	if len(a.Reservations) != 1 || a.Intents[0].State != "unknown" {
		t.Fatal("unknown reservation released", a)
	}
	bad := Change{Key: key, Ownership: owner, Command: Command{"bad-final", "resolve", r.Revision}, Resolutions: []Resolution{{IntentID: "intent-1", State: "final", Evidence: raw(`{}`)}}}
	if _, err = s.Apply(ctx, bad); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	fill := Change{Key: key, Ownership: owner, Command: Command{"fill", "execution", r.Revision}, Snapshot: raw(`{"filled":"0.1"}`), Balances: []Balance{{"USD", amount("989.8798"), amount("989.8798"), d.Zero}, {"BTC", amount("1.1"), amount("1.1"), d.Zero}}, Resolutions: []Resolution{{IntentID: "intent-1", State: "final", KnownFinal: true, Evidence: raw(`{"venue_status":"filled"}`)}}, Execution: &Execution{ID: "trade-1", IntentID: "intent-1", Venue: "paper", Detail: raw(`{"quantity":"0.1","price":"101","fee":"0.0202"}`)}}
	first, err := s.Apply(ctx, fill)
	if err != nil {
		t.Fatal(err)
	}
	a = mustAccount(t, s, key)
	if len(a.Reservations) != 0 || len(a.Intents) != 0 {
		t.Fatal("known final still retained", a)
	}
	fill.Command = Command{"repeat-fill", "execution", 0}
	again, err := s.Apply(ctx, fill)
	if err != nil || !again.Duplicate || again.Revision != first.Revision {
		t.Fatal("execution deduplication", again, err)
	}
	fill.Command.ID = "changed-fill"
	fill.Execution.Detail = raw(`{"quantity":"0.2"}`)
	if _, err = s.Apply(ctx, fill); !errors.Is(err, ErrConflict) {
		t.Fatal("mismatched execution identity", err)
	}
	if mustAccount(t, s, key).Revision != first.Revision {
		t.Fatal("duplicate execution changed balances")
	}
}

func TestPostgresOwnershipNeedsIsolationAndRetainsUnknown(t *testing.T) {
	s := testStore(t)
	key, owner, a := initialized(t, s)
	ctx := context.Background()
	q := queue(t, s, key, owner, a.Revision)
	dispatch, err := s.AuthorizeDispatch(ctx, authorization(key, owner, q.Revision))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ClaimOwnership(ctx, key, "worker-b", time.Minute, RecoveryProof{}); !errors.Is(err, ErrIsolationRequired) {
		t.Fatal("lease mistaken for exchange fencing", err)
	}
	// Even an expired lease does not establish old-sender isolation.
	if _, err = s.pool.Exec(ctx, "UPDATE accounts SET lease_until=clock_timestamp()-interval '1 second' WHERE tenant_id=$1 AND account_id=$2", key.TenantID, key.AccountID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ClaimOwnership(ctx, key, "worker-b", time.Minute, RecoveryProof{}); !errors.Is(err, ErrIsolationRequired) {
		t.Fatal(err)
	}
	newOwner, err := s.ClaimOwnership(ctx, key, "worker-b", time.Minute, RecoveryProof{true, true, "isolated-process-123-reconciled"})
	if err != nil {
		t.Fatal(err)
	}
	a = mustAccount(t, s, key)
	if newOwner.Epoch != owner.Epoch+1 || a.Ready || a.Market.Valid || len(a.Reservations) != 1 || a.Intents[0].State != "attempted" {
		t.Fatal("unsafe ownership transfer", a)
	}
	if _, err = s.Apply(ctx, Change{Key: key, Ownership: owner, Command: Command{"old-writer", "snapshot", a.Revision}, Snapshot: raw(`{}`)}); !errors.Is(err, ErrOwnership) {
		t.Fatal("stale owner wrote", err)
	}
	if err = s.RenewOwnership(ctx, key, newOwner, time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AuthorizeDispatch(ctx, authorization(key, owner, dispatch.Revision)); !errors.Is(err, ErrOwnership) {
		t.Fatal(err)
	}
}

func TestPostgresPauseDispatchRace(t *testing.T) {
	s := testStore(t)
	key, owner, a := initialized(t, s)
	ctx := context.Background()
	q := queue(t, s, key, owner, a.Revision)
	plan := mustAccount(t, s, key).Plans[0]
	plan.Revision++
	plan.Status = "paused"
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	var dispatchErr, pauseErr error
	go func() {
		defer wg.Done()
		<-start
		_, dispatchErr = s.AuthorizeDispatch(ctx, authorization(key, owner, q.Revision))
	}()
	go func() {
		defer wg.Done()
		<-start
		_, pauseErr = s.Apply(ctx, Change{Key: key, Ownership: owner, Command: Command{"pause", "pause", q.Revision}, Plan: &plan})
	}()
	close(start)
	wg.Wait()
	if dispatchErr == nil {
		if !errors.Is(pauseErr, ErrStaleRevision) {
			t.Fatal("dispatch winner did not reject stale pause", pauseErr)
		}
		latest := mustAccount(t, s, key)
		_, err := s.Apply(ctx, Change{Key: key, Ownership: owner, Command: Command{"pause-retry", "pause", latest.Revision}, Plan: &plan})
		if err != nil {
			t.Fatal(err)
		}
		latest = mustAccount(t, s, key)
		if latest.Plans[0].Status != "paused" || latest.Intents[0].State != "attempted" || len(latest.Reservations) != 1 {
			t.Fatal("pause lost in-flight uncertainty", latest)
		}
	} else {
		if pauseErr != nil || !errors.Is(dispatchErr, ErrConflict) {
			t.Fatal("race outcomes", dispatchErr, pauseErr)
		}
		latest := mustAccount(t, s, key)
		if latest.Plans[0].Status != "paused" || len(latest.Reservations) != 0 || len(latest.Intents) != 0 {
			t.Fatal("unsent paused order retained", latest)
		}
	}
}

func TestPostgresQueuedExpiryMetadataAndFunds(t *testing.T) {
	t.Run("expiry", func(t *testing.T) {
		s := testStore(t)
		key, owner, a := initialized(t, s)
		q := queue(t, s, key, owner, a.Revision)
		if _, err := s.pool.Exec(context.Background(), "UPDATE outbox SET expires_at=clock_timestamp()-interval '1 second' WHERE tenant_id=$1 AND account_id=$2", key.TenantID, key.AccountID); err != nil {
			t.Fatal(err)
		}
		_, err := s.AuthorizeDispatch(context.Background(), authorization(key, owner, q.Revision))
		if !errors.Is(err, ErrExpired) {
			t.Fatal(err)
		}
		a = mustAccount(t, s, key)
		if len(a.Intents) != 0 || len(a.Reservations) != 0 || a.Revision != q.Revision+1 {
			t.Fatal("expired queued intent not invalidated", a)
		}
	})
	t.Run("metadata", func(t *testing.T) {
		s := testStore(t)
		key, owner, a := initialized(t, s)
		q := queue(t, s, key, owner, a.Revision)
		_, err := s.Apply(context.Background(), Change{Key: key, Ownership: owner, Command: Command{"metadata", "market", q.Revision}, Market: &Market{true, "book-2", "rules-2"}})
		if err != nil {
			t.Fatal(err)
		}
		a = mustAccount(t, s, key)
		if len(a.Intents) != 0 || len(a.Reservations) != 0 {
			t.Fatal("metadata update retained stale queued order", a)
		}
	})
	t.Run("reflected_hold", func(t *testing.T) {
		s := testStore(t)
		key, owner, a := initialized(t, s)
		q := queue(t, s, key, owner, a.Revision)
		dispatch, err := s.AuthorizeDispatch(context.Background(), authorization(key, owner, q.Revision))
		if err != nil {
			t.Fatal(err)
		}
		reflected := true
		_, err = s.Apply(context.Background(), Change{Key: key, Ownership: owner, Command: Command{"venue-hold", "reconcile", dispatch.Revision}, Balances: []Balance{{"USD", amount("1000"), amount("980"), amount("20")}}, Resolutions: []Resolution{{IntentID: "intent-1", State: "acknowledged", Reflected: &reflected, Evidence: raw(`{"locked":"20"}`)}}})
		if err != nil {
			t.Fatal("double-subtracted venue hold", err)
		}
		a = mustAccount(t, s, key)
		if !a.Reservations[0].Reflected {
			t.Fatal(a)
		}
	})
}

func TestPostgresConcurrentCommandDedup(t *testing.T) {
	s := testStore(t)
	key, owner, a := initialized(t, s)
	change := queuedChange(key, owner, a.Revision)
	var wg sync.WaitGroup
	var count int
	var mu sync.Mutex
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := s.Apply(context.Background(), change)
			if err != nil {
				t.Error(err)
				return
			}
			if !r.Duplicate {
				mu.Lock()
				count++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if count != 1 {
		t.Fatal("expected exactly one durable transition", count)
	}
	a = mustAccount(t, s, key)
	if a.Revision != 3 || len(a.Reservations) != 1 || len(a.Intents) != 1 {
		t.Fatal(a)
	}
}
