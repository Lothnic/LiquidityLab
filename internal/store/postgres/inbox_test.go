package postgres

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"
)

func recordObservation(t *testing.T, s *Store, key Key, id, kind, payload string) int64 {
	t.Helper()
	sequence, err := s.RecordObservation(context.Background(), key, Observation{
		ID: id, Kind: kind, Payload: json.RawMessage(payload),
	})
	if err != nil {
		t.Fatal(err)
	}
	return sequence
}

func createInboxAccount(t *testing.T, s *Store, key Key) {
	t.Helper()
	_, err := s.CreateAccount(context.Background(), key,
		[]Balance{{"USD", amount("100"), amount("100"), amount("0")}}, raw(`{"schema":"paper-v1"}`))
	if err != nil {
		t.Fatal(err)
	}
}

func compactJSON(t *testing.T, value []byte) []byte {
	t.Helper()
	var compact bytes.Buffer
	if err := json.Compact(&compact, value); err != nil {
		t.Fatal(err)
	}
	return compact.Bytes()
}

func jsonObjectEqual(t *testing.T, value []byte, expected map[string]any) bool {
	t.Helper()
	var actual map[string]any
	if err := json.Unmarshal(value, &actual); err != nil {
		t.Fatal(err)
	}
	return reflect.DeepEqual(actual, expected)
}

func TestPostgresInboxIdentitySequencesAndLeaseIndependence(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	key, _, _ := initialized(t, s)
	first := recordObservation(t, s, key, "fact-1", "trade", ` { "price": 101.25, "quantity": 0.1 } `)
	if first != 1 {
		t.Fatalf("first sequence %d, want 1", first)
	}
	duplicate, err := s.RecordObservation(ctx, key, Observation{
		ID: "fact-1", Kind: "trade", Payload: raw(`{"quantity":0.1,"price":101.25}`),
	})
	if err != nil || duplicate != first {
		t.Fatalf("canonical retry sequence=%d err=%v", duplicate, err)
	}
	if _, err = s.RecordObservation(ctx, key, Observation{
		ID: "fact-1", Kind: "trade", Payload: raw(`{"price":101.25,"quantity":0.2}`),
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed fact with reused ID: got %v, want ErrConflict", err)
	}
	if _, err = s.RecordObservation(ctx, key, Observation{
		ID: "fact-1", Kind: "book", Payload: raw(`{"price":101.25,"quantity":0.1}`),
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed kind with reused ID: got %v, want ErrConflict", err)
	}
	if got := recordObservation(t, s, key, "fact-2", "trade", `{"price":102}`); got != 2 {
		t.Fatalf("next account sequence %d, want 2", got)
	}

	otherAccount := Key{TenantID: key.TenantID, AccountID: "account-b"}
	otherTenant := Key{TenantID: "tenant-b", AccountID: key.AccountID}
	createInboxAccount(t, s, otherAccount)
	createInboxAccount(t, s, otherTenant)
	if got := recordObservation(t, s, otherAccount, "fact-1", "trade", `{"isolated":true}`); got != 1 {
		t.Fatalf("other account sequence %d, want 1", got)
	}
	if got := recordObservation(t, s, otherTenant, "fact-1", "trade", `{"isolated":true}`); got != 1 {
		t.Fatalf("other tenant sequence %d, want 1", got)
	}
	mainPending, err := s.PendingObservations(ctx, key, 10)
	if err != nil || len(mainPending) != 2 || mainPending[0].Sequence != 1 || mainPending[1].Sequence != 2 {
		t.Fatalf("main account pending observations=%+v err=%v", mainPending, err)
	}
	otherPending, err := s.PendingObservations(ctx, otherAccount, 10)
	if err != nil || len(otherPending) != 1 || !bytes.Equal(compactJSON(t, otherPending[0].Payload), []byte(`{"isolated":true}`)) {
		t.Fatalf("isolated account pending observations=%+v err=%v", otherPending, err)
	}

	if _, err = s.pool.Exec(ctx, `UPDATE accounts SET lease_until=clock_timestamp()-interval '1 second' WHERE tenant_id=$1 AND account_id=$2`, key.TenantID, key.AccountID); err != nil {
		t.Fatal(err)
	}
	if got := recordObservation(t, s, key, "fact-after-lease", "balance", `{"free":"900"}`); got != 3 {
		t.Fatalf("post-lease observation sequence %d, want 3", got)
	}
	a := mustAccount(t, s, key)
	if a.ObservedSequence != 3 || a.ProcessedSequence != 0 {
		t.Fatalf("observation cursor after expired lease: %+v", a)
	}
}

func TestPostgresInboxBlocksDispatchUntilCursorAndProjectionCommit(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	key, owner, initial := initialized(t, s)
	queued := queue(t, s, key, owner, initial.Revision)
	sequence := recordObservation(t, s, key, "pending-fact", "balance", `{"free":"900"}`)
	if _, err := s.AuthorizeDispatch(ctx, authorization(key, owner, queued.Revision)); !errors.Is(err, ErrNotReady) {
		t.Fatalf("dispatch with pending observation: got %v, want ErrNotReady", err)
	}

	consumed, err := s.Apply(ctx, Change{
		Key: key, Ownership: owner,
		Command:                   Command{ID: "consume-fact", Kind: "reduce", ExpectedRevision: queued.Revision},
		Snapshot:                  raw(`{"schema":"paper-v1","reduced":"pending-fact"}`),
		ConsumeObservationThrough: sequence,
	})
	if err != nil {
		t.Fatal(err)
	}
	a := mustAccount(t, s, key)
	if a.ProcessedSequence != sequence || a.ObservedSequence != sequence || a.PendingObservationBytes != 0 {
		t.Fatalf("consumed account cursor/bytes: %+v", a)
	}
	if !jsonObjectEqual(t, a.Snapshot, map[string]any{"schema": "paper-v1", "reduced": "pending-fact"}) {
		t.Fatalf("projection snapshot not committed with cursor: %s", a.Snapshot)
	}
	if _, err = s.AuthorizeDispatch(ctx, authorization(key, owner, consumed.Revision)); err != nil {
		t.Fatalf("dispatch after cursor and projection commit: %v", err)
	}
}

func TestPostgresInboxCursorProjectionRollbackIsAtomic(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	key, owner, initial := initialized(t, s)
	sequence := recordObservation(t, s, key, "rollback-fact", "trade", `{"price":"101"}`)
	payloadBytes := int64(len([]byte(`{"price":"101"}`)))
	_, err := s.Apply(ctx, Change{
		Key: key, Ownership: owner,
		Command:                   Command{ID: "rollback-reduction", Kind: "reduce", ExpectedRevision: initial.Revision},
		Snapshot:                  raw(`{"schema":"paper-v1","reduced":true}`),
		ConsumeObservationThrough: sequence,
		ObsoleteQueued:            []string{"missing-intent"},
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("rollback transition got %v, want ErrConflict", err)
	}
	a := mustAccount(t, s, key)
	if a.Revision != initial.Revision || a.ProcessedSequence != 0 || a.ObservedSequence != sequence || a.PendingObservationBytes != payloadBytes {
		t.Fatalf("failed transition partially advanced account or cursor: %+v", a)
	}
	if string(a.Snapshot) != string(initial.Snapshot) {
		t.Fatalf("failed transition changed projection snapshot: %s", a.Snapshot)
	}
	if _, err = s.LookupCommand(ctx, key, "rollback-reduction"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("failed transition recorded command: %v", err)
	}
}

func TestPostgresLookupCommandKeepsOriginalSnapshot(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	key, owner, current := initialized(t, s)
	record, err := s.LookupCommand(ctx, key, "ready")
	if err != nil {
		t.Fatal(err)
	}
	var snapshot map[string]any
	if err = json.Unmarshal(record.Snapshot, &snapshot); err != nil {
		t.Fatal(err)
	}
	if record.Kind != "reconcile" || record.ExpectedRevision != 1 || !reflect.DeepEqual(snapshot, map[string]any{"schema": "paper-v1", "ready": true}) {
		t.Fatalf("original command record: %+v", record)
	}
	_, err = s.Apply(ctx, Change{
		Key: key, Ownership: owner,
		Command:  Command{ID: "later-state", Kind: "project", ExpectedRevision: current.Revision},
		Snapshot: raw(`{"schema":"paper-v1","generation":2}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	current = mustAccount(t, s, key)
	if !jsonObjectEqual(t, current.Snapshot, map[string]any{"schema": "paper-v1", "generation": float64(2)}) {
		t.Fatalf("later projection missing: %s", current.Snapshot)
	}
	reloaded, err := s.LookupCommand(ctx, key, "ready")
	if err != nil {
		t.Fatal(err)
	}
	if string(reloaded.Snapshot) != string(record.Snapshot) || !reloaded.Result.Duplicate {
		t.Fatalf("lookup did not retain immutable original snapshot: %+v", reloaded)
	}
}

func TestPostgresObsoleteQueuedReleasesOnlyUnsentReservation(t *testing.T) {
	t.Run("queued intent releases", func(t *testing.T) {
		s := testStore(t)
		ctx := context.Background()
		key, owner, initial := initialized(t, s)
		queued := queue(t, s, key, owner, initial.Revision)
		result, err := s.Apply(ctx, Change{
			Key: key, Ownership: owner,
			Command:        Command{ID: "obsolete-unsent", Kind: "reduce", ExpectedRevision: queued.Revision},
			Snapshot:       raw(`{"schema":"paper-v1","intent":"obsolete"}`),
			ObsoleteQueued: []string{"intent-1"},
		})
		if err != nil {
			t.Fatal(err)
		}
		var state, reservationState string
		if err = s.pool.QueryRow(ctx, `SELECT state FROM outbox WHERE tenant_id=$1 AND account_id=$2 AND intent_id='intent-1'`, key.TenantID, key.AccountID).Scan(&state); err != nil {
			t.Fatal(err)
		}
		if err = s.pool.QueryRow(ctx, `SELECT state FROM reservations WHERE tenant_id=$1 AND account_id=$2 AND intent_id='intent-1' AND currency='USD'`, key.TenantID, key.AccountID).Scan(&reservationState); err != nil {
			t.Fatal(err)
		}
		if state != "obsolete" || reservationState != "released" {
			t.Fatalf("queued intent state=%s reservation=%s", state, reservationState)
		}
		if _, err = s.AuthorizeDispatch(ctx, authorization(key, owner, result.Revision)); !errors.Is(err, ErrConflict) {
			t.Fatalf("obsolete intent authorized: %v", err)
		}
	})

	t.Run("attempted intent refuses obsoletion", func(t *testing.T) {
		s := testStore(t)
		ctx := context.Background()
		key, owner, initial := initialized(t, s)
		queued := queue(t, s, key, owner, initial.Revision)
		dispatch, err := s.AuthorizeDispatch(ctx, authorization(key, owner, queued.Revision))
		if err != nil {
			t.Fatal(err)
		}
		_, err = s.Apply(ctx, Change{
			Key: key, Ownership: owner,
			Command:        Command{ID: "obsolete-attempted", Kind: "reduce", ExpectedRevision: dispatch.Revision},
			Snapshot:       raw(`{"schema":"paper-v1","intent":"retain-inflight"}`),
			ObsoleteQueued: []string{"intent-1"},
		})
		if !errors.Is(err, ErrConflict) {
			t.Fatalf("attempted intent obsoletion got %v, want ErrConflict", err)
		}
		var state, reservationState string
		if err = s.pool.QueryRow(ctx, `SELECT state FROM outbox WHERE tenant_id=$1 AND account_id=$2 AND intent_id='intent-1'`, key.TenantID, key.AccountID).Scan(&state); err != nil {
			t.Fatal(err)
		}
		if err = s.pool.QueryRow(ctx, `SELECT state FROM reservations WHERE tenant_id=$1 AND account_id=$2 AND intent_id='intent-1' AND currency='USD'`, key.TenantID, key.AccountID).Scan(&reservationState); err != nil {
			t.Fatal(err)
		}
		if state != "attempted" || reservationState != "held" {
			t.Fatalf("attempted intent state=%s reservation=%s", state, reservationState)
		}
		if !time.Now().Before(dispatch.Intent.ExpiresAt) {
			t.Fatal("test dispatch expired unexpectedly")
		}
	})
}
