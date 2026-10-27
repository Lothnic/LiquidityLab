package postgres

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func paperAccount(t *testing.T, s *Store, key Key) Account {
	t.Helper()
	a, err := s.CreateAccount(context.Background(), key,
		[]Balance{{"USD", amount("100"), amount("100"), amount("0")}},
		raw(`{"version":"account-state-v1","profile":"paper","book":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestPaperOwnershipReturnsPostClaimStateAndRetainsAttemptedHolds(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	key, old, a := initialized(t, s)
	q := queuedChange(key, old, a.Revision)
	q.Snapshot = raw(`{"version":"account-state-v1","profile":"paper","parent":{"id":"plan-1"},"note":"retained"}`)
	queued, err := s.Apply(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	dispatch, err := s.AuthorizeDispatch(ctx, authorization(key, old, queued.Revision))
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Apply(ctx, Change{Key: key, Ownership: old,
		Command: Command{"another-queued", "queue", dispatch.Revision},
		Enqueue: []Intent{{ID: "unsent", PlanID: "plan-1", PlanRevision: 1,
			ExpiresAt: q.Plan.Deadline, InstrumentRevision: "rules-1", Payload: raw(`{}`)}},
		Reservations: []Reservation{{IntentID: "unsent", Currency: "USD", Amount: amount("5")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	watermark := recordObservation(t, s, key, "legacy-committed", "transport", `{"outcome":"committed before takeover"}`)
	before := mustAccount(t, s, key)
	claimed, err := s.ClaimPaperOwnership(ctx, key, "paper-owner-b", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.PaperTransportContract != PaperTransportContract || claimed.Ownership.Epoch != old.Epoch+1 || claimed.Ownership.OwnerID != "paper-owner-b" ||
		claimed.Revision != before.Revision+1 || claimed.Ready || claimed.Market.Valid || claimed.ObservedSequence != watermark ||
		claimed.ProcessedSequence != before.ProcessedSequence || !claimed.LeaseUntil.After(time.Now()) {
		t.Fatalf("unsafe paper ownership transition: %+v", claimed)
	}
	if !bytes.Equal(claimed.Snapshot, before.Snapshot) || !reflect.DeepEqual(claimed.Balances, before.Balances) ||
		len(claimed.Intents) != 1 || claimed.Intents[0].ID != "intent-1" || claimed.Intents[0].State != "attempted" ||
		len(claimed.Reservations) != 1 || claimed.Reservations[0].IntentID != "intent-1" {
		t.Fatalf("paper takeover changed retained financial state: %+v", claimed)
	}
	if fresh := mustAccount(t, s, key); !reflect.DeepEqual(fresh, claimed) {
		t.Fatalf("claim returned a projection from before its transaction: fresh=%+v claimed=%+v", fresh, claimed)
	}
	unsent, err := s.GetIntent(ctx, key, "unsent")
	if err != nil || unsent.State != "obsolete" {
		t.Fatalf("queued intent not obsoleted: %+v %v", unsent, err)
	}
	var detail []byte
	if err = s.pool.QueryRow(ctx, `SELECT detail FROM account_journal WHERE tenant_id=$1 AND account_id=$2 AND revision=$3 AND kind='paper_ownership'`, key.TenantID, key.AccountID, claimed.Revision).Scan(&detail); err != nil {
		t.Fatal(err)
	}
	var audit struct {
		Profile           string    `json:"profile"`
		TransportContract string    `json:"transport_contract"`
		PreviousOwnership Ownership `json:"previous_ownership"`
		Ownership         Ownership `json:"ownership"`
		RecoveryWatermark int64     `json:"recovery_watermark"`
		Fence             string    `json:"fence"`
		Reconciliation    string    `json:"reconciliation"`
	}
	if err = json.Unmarshal(detail, &audit); err != nil {
		t.Fatal(err)
	}
	if audit.Profile != "paper" || audit.TransportContract != PaperTransportContract || audit.PreviousOwnership != old || audit.Ownership != claimed.Ownership ||
		audit.RecoveryWatermark != watermark || !strings.Contains(audit.Fence, "database-only") || !strings.Contains(audit.Fence, "no external sender isolation asserted") ||
		audit.Reconciliation != "required after takeover" || strings.Contains(string(detail), "previous_sender_isolated") {
		t.Fatalf("paper audit misstates isolation or reconciliation: %s", detail)
	}
	if _, err = s.RecordObservation(ctx, key, Observation{ID: "legacy-late", Kind: "transport", Payload: raw(`{}`)}); !errors.Is(err, ErrOwnership) {
		t.Fatalf("unowned simulated venue append after takeover: %v", err)
	}
	if _, err = s.RecordPaperObservation(ctx, key, old, Observation{ID: "old-late", Kind: "paper_trade", Payload: raw(`{}`)}); !errors.Is(err, ErrOwnership) {
		t.Fatalf("old sender appended with its previously valid lease: %v", err)
	}
	if _, err = s.ClaimOwnership(ctx, key, "generic-bypass", time.Minute, RecoveryProof{true, true, "asserted-proof"}); !errors.Is(err, ErrIsolationRequired) {
		t.Fatalf("generic claim bypassed paper contract: %v", err)
	}
	idempotent, err := s.ClaimPaperOwnership(ctx, key, claimed.Ownership.OwnerID, time.Minute)
	if err != nil || !reflect.DeepEqual(idempotent, claimed) {
		t.Fatalf("same-owner retry changed the claim: %+v %v", idempotent, err)
	}
	if _, err = s.Apply(ctx, Change{Key: key, Ownership: old, Command: Command{"old-snapshot", "snapshot", claimed.Revision}, Snapshot: raw(`{}`)}); !errors.Is(err, ErrOwnership) {
		t.Fatalf("stale reducer changed the recovered snapshot: %v", err)
	}
}

func TestPaperObservationsKeepCanonicalIdentityAndExternalIngressSeparate(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	key := Key{"paper", "observations"}
	paperAccount(t, s, key)
	a, err := s.ClaimPaperOwnership(ctx, key, "paper-owner", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	first := Observation{ID: "submit", Kind: "transport", Payload: raw(` {"quantity":"0.1","price":"101"} `)}
	sequence, err := s.RecordPaperObservation(ctx, key, a.Ownership, first)
	if err != nil || sequence != 1 {
		t.Fatalf("owned append sequence=%d err=%v", sequence, err)
	}
	first.Payload = raw(`{"price":"101","quantity":"0.1"}`)
	if duplicate, err := s.RecordPaperObservation(ctx, key, a.Ownership, first); err != nil || duplicate != sequence {
		t.Fatalf("owned canonical retry sequence=%d err=%v", duplicate, err)
	}
	first.Payload = raw(`{"price":"102","quantity":"0.1"}`)
	if _, err = s.RecordPaperObservation(ctx, key, a.Ownership, first); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting paper identity: %v", err)
	}
	if _, err = s.RecordPaperObservation(ctx, key, a.Ownership, Observation{ID: "external", Kind: "fill", Payload: raw(`{}`)}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("paper API accepted external financial fact kind: %v", err)
	}
	if _, err = s.pool.Exec(ctx, `UPDATE accounts SET lease_until=clock_timestamp()-interval '1 second' WHERE tenant_id=$1 AND account_id=$2`, key.TenantID, key.AccountID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.RecordPaperObservation(ctx, key, a.Ownership, Observation{ID: "expired", Kind: "paper_trade", Payload: raw(`{}`)}); !errors.Is(err, ErrOwnership) {
		t.Fatalf("expired paper sender appended: %v", err)
	}
	if got := recordObservation(t, s, key, "external-after-expiry", "fill", `{"external":true}`); got != 2 {
		t.Fatalf("external fact incorrectly fenced by execution lease: %d", got)
	}
	other := Key{"other-tenant", key.AccountID}
	paperAccount(t, s, other)
	b, err := s.ClaimPaperOwnership(ctx, other, "other-owner", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.RecordPaperObservation(ctx, other, a.Ownership, Observation{ID: "cross-tenant", Kind: "transport", Payload: raw(`{}`)}); !errors.Is(err, ErrOwnership) {
		t.Fatalf("cross-tenant sender accepted: %v", err)
	}
	if got, err := s.RecordPaperObservation(ctx, other, b.Ownership, Observation{ID: "submit", Kind: "transport", Payload: raw(`{}`)}); err != nil || got != 1 {
		t.Fatalf("other tenant did not retain independent identities: %d %v", got, err)
	}
}

func TestPaperObservationTriggerFencesLegacySQLWithCallerSearchPath(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	key := Key{"paper", "trigger"}
	paperAccount(t, s, key)
	a, err := s.ClaimPaperOwnership(ctx, key, "current-owner", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	var schema string
	if err = s.pool.QueryRow(ctx, `SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	connection, err := s.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Release()
	for _, tc := range []struct {
		name, owner, epoch, kind string
		owned                    bool
	}{
		{"missing settings", "", "", "transport", false},
		{"invalid epoch", a.Ownership.OwnerID, "not-a-number", "transport", false},
		{"stale epoch", a.Ownership.OwnerID, "0", "paper_trade", false},
		{"stale owner", "old-owner", fmt.Sprint(a.Ownership.Epoch), "transport", false},
		{"current token", a.Ownership.OwnerID, fmt.Sprint(a.Ownership.Epoch), "transport", true},
		{"external fact", "", "", "fill", true},
		{"settings discarded after transaction", "", "", "paper_trade", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx, err := connection.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			if _, err = tx.Exec(ctx, `SET LOCAL search_path = pg_catalog`); err != nil {
				t.Fatal(err)
			}
			if tc.owner != "" || tc.epoch != "" {
				if _, err = tx.Exec(ctx, `SELECT set_config('liquiditylab.paper_owner_id',$1,true),set_config('liquiditylab.paper_owner_epoch',$2,true)`, tc.owner, tc.epoch); err != nil {
					t.Fatal(err)
				}
			}
			_, err = tx.Exec(ctx, `INSERT INTO `+pgx.Identifier{schema, "observations"}.Sanitize()+`(tenant_id,account_id,sequence,observation_id,kind,payload,payload_bytes,digest) VALUES($1,$2,1,'legacy-raw',$3,'{}',2,'\x00')`, key.TenantID, key.AccountID, tc.kind)
			if tc.owned {
				if err != nil {
					t.Fatalf("permitted raw observation failed: %v", err)
				}
			} else if normalized(err) != ErrOwnership {
				t.Fatalf("legacy trigger failure was not sanitized ErrOwnership: %v", err)
			}
		})
	}
	// Neither transaction-local credentials nor any unsuccessful legacy append
	// can escape its transaction into the next connection borrower.
	if a := mustAccount(t, s, key); a.ObservedSequence != 0 || a.PendingObservationBytes != 0 {
		t.Fatalf("raw trigger probes changed inbox state: %+v", a)
	}
	if got, err := s.RecordPaperObservation(ctx, key, a.Ownership, Observation{ID: "owned", Kind: "paper_trade", Payload: raw(`{}`)}); err != nil || got != 1 {
		t.Fatalf("new owned API failed with insert guard active: %d %v", got, err)
	}
}

func TestPaperClaimWaitsForLegacyEffectTransactionAndRetainsItsWatermark(t *testing.T) {
	s := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	key := Key{"legacy-paper", "in-flight"}
	paperAccount(t, s, key)
	if _, err := s.ClaimOwnership(ctx, key, "legacy-owner", time.Minute, RecoveryProof{}); err != nil {
		t.Fatal(err)
	}
	legacy, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer legacy.Rollback(ctx)
	if _, err = lockAccount(ctx, legacy, key); err != nil {
		t.Fatal(err)
	}
	type claimResult struct {
		account Account
		err     error
	}
	claimStarted := make(chan struct{})
	claimDone := make(chan claimResult, 1)
	go func() {
		close(claimStarted)
		a, err := s.ClaimPaperOwnership(ctx, key, "new-paper-owner", time.Minute)
		claimDone <- claimResult{a, err}
	}()
	<-claimStarted
	// Reproduce an old binary's transaction, including its unowned INSERT.
	// Its existing account lock makes the takeover wait for the entire effect.
	if _, err = legacy.Exec(ctx, `INSERT INTO observations(tenant_id,account_id,sequence,observation_id,kind,payload,payload_bytes,digest) VALUES($1,$2,1,'legacy-accepted','transport','{}',2,'\x00')`, key.TenantID, key.AccountID); err != nil {
		t.Fatal(err)
	}
	if _, err = legacy.Exec(ctx, `UPDATE accounts SET observed_sequence=1,pending_observation_bytes=2 WHERE tenant_id=$1 AND account_id=$2`, key.TenantID, key.AccountID); err != nil {
		t.Fatal(err)
	}
	if err = legacy.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	claim := <-claimDone
	if claim.err != nil || claim.account.ObservedSequence != 1 || claim.account.PendingObservationBytes != 2 {
		t.Fatalf("takeover lost a legacy transaction committed before it: %+v %v", claim.account, claim.err)
	}
	if fact, err := s.GetObservation(ctx, key, "legacy-accepted"); err != nil || fact.Sequence != claim.account.ObservedSequence {
		t.Fatalf("pre-takeover legacy effect was not retained: %+v %v", fact, err)
	}
}

func TestPaperClaimRacesWithSimulatedEffects(t *testing.T) {
	s := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for iteration := 0; iteration < 20; iteration++ {
		key := Key{"paper-race", fmt.Sprint(iteration)}
		paperAccount(t, s, key)
		old, err := s.ClaimPaperOwnership(ctx, key, "old-owner", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		type claimResult struct {
			account Account
			err     error
		}
		claimDone := make(chan claimResult, 1)
		recordDone := make(chan error, 1)
		go func() {
			<-start
			a, err := s.ClaimPaperOwnership(ctx, key, "new-owner", time.Minute)
			claimDone <- claimResult{a, err}
		}()
		go func() {
			<-start
			_, err := s.RecordPaperObservation(ctx, key, old.Ownership, Observation{ID: "racing-effect", Kind: "transport", Payload: raw(`{"accepted":true}`)})
			recordDone <- err
		}()
		close(start)
		claim := <-claimDone
		recordErr := <-recordDone
		if claim.err != nil {
			t.Fatal(claim.err)
		}
		if recordErr == nil {
			if claim.account.ObservedSequence != 1 {
				t.Fatalf("committed old effect missing from takeover watermark: %+v", claim.account)
			}
			if _, err := s.GetObservation(ctx, key, "racing-effect"); err != nil {
				t.Fatalf("committed old effect disappeared: %v", err)
			}
		} else if errors.Is(recordErr, ErrOwnership) {
			if claim.account.ObservedSequence != 0 {
				t.Fatalf("rejected old effect advanced the watermark: %+v", claim.account)
			}
		} else {
			t.Fatalf("unexpected racing sender failure: %v", recordErr)
		}
		if _, err := s.RecordPaperObservation(ctx, key, old.Ownership, Observation{ID: "resumed-stale-sender", Kind: "paper_trade", Payload: raw(`{}`)}); !errors.Is(err, ErrOwnership) {
			t.Fatalf("suspended old sender appended after takeover: %v", err)
		}
		got, err := s.RecordPaperObservation(ctx, key, claim.account.Ownership, Observation{ID: "new-effect", Kind: "transport", Payload: raw(`{}`)})
		if err != nil || got != claim.account.ObservedSequence+1 {
			t.Fatalf("new owner could not append after race: sequence=%d err=%v", got, err)
		}
	}
}

func TestPaperClaimRejectsInvalidProfilesAndArguments(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	for index, snapshot := range []string{`{"profile":"live"}`, `{"schema":"unclassified"}`} {
		key := Key{"paper-invalid", fmt.Sprint(index)}
		if _, err := s.CreateAccount(ctx, key, []Balance{{"USD", amount("1"), amount("1"), amount("0")}}, raw(snapshot)); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ClaimPaperOwnership(ctx, key, "owner", time.Minute); !errors.Is(err, ErrInvalid) {
			t.Fatalf("non-paper profile accepted: %s %v", snapshot, err)
		}
		if a := mustAccount(t, s, key); a.Revision != 0 || a.Ownership.OwnerID != "" || a.PaperTransportContract != "" {
			t.Fatalf("failed paper claim mutated account: %+v", a)
		}
	}
	key := Key{"paper-invalid", "virgin"}
	if _, err := s.CreateAccount(ctx, key, []Balance{{"USD", amount("1"), amount("1"), amount("0")}}, raw(`{}`)); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		key   Key
		owner string
		lease time.Duration
	}{{Key{}, "owner", time.Minute}, {key, "", time.Minute}, {key, "owner", 0}, {key, "owner", 6 * time.Minute}} {
		if _, err := s.ClaimPaperOwnership(ctx, tc.key, tc.owner, tc.lease); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid paper claim accepted: %+v %v", tc, err)
		}
	}
	if _, err := s.ClaimPaperOwnership(ctx, Key{"paper-invalid", "missing"}, "owner", time.Minute); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing account was created or misreported: %v", err)
	}
	if a, err := s.ClaimPaperOwnership(ctx, key, "owner", time.Minute); err != nil || a.Ownership.Epoch != 1 || a.Revision != 1 {
		t.Fatalf("virgin paper account claim: %+v %v", a, err)
	}
}

func TestPaperClaimDoesNotReclassifyPopulatedBlankSnapshots(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	for _, owned := range []bool{true, false} {
		key := Key{"unclassified", fmt.Sprint(owned)}
		if _, err := s.CreateAccount(ctx, key, []Balance{{"USD", amount("10"), amount("10"), amount("0")}}, raw(`{}`)); err != nil {
			t.Fatal(err)
		}
		if owned {
			if _, err := s.ClaimOwnership(ctx, key, "unknown-sender", time.Minute, RecoveryProof{}); err != nil {
				t.Fatal(err)
			}
		} else {
			recordObservation(t, s, key, "external-fact", "fill", `{"external":true}`)
		}
		before := mustAccount(t, s, key)
		if _, err := s.ClaimPaperOwnership(ctx, key, "paper-sender", time.Minute); !errors.Is(err, ErrInvalid) {
			t.Fatalf("blank populated snapshot incorrectly reclassified as paper: owned=%t err=%v", owned, err)
		}
		if after := mustAccount(t, s, key); !reflect.DeepEqual(before, after) {
			t.Fatalf("rejected classification changed account: before=%+v after=%+v", before, after)
		}
	}
}

func TestPaperClaimRecoversCrashBeforeFirstRuntimeSnapshot(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	key := Key{"paper-bootstrap", "crashed"}
	if _, err := s.CreateAccount(ctx, key, []Balance{{"USD", amount("10"), amount("10"), amount("0")}}, raw(`{}`)); err != nil {
		t.Fatal(err)
	}
	first, err := s.ClaimPaperOwnership(ctx, key, "first-paper-owner", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if same, err := s.ClaimPaperOwnership(ctx, key, first.Ownership.OwnerID, time.Minute); err != nil || !reflect.DeepEqual(first, same) {
		t.Fatalf("retry before runtime bootstrap was not idempotent: %+v %v", same, err)
	}
	sequence, err := s.RecordPaperObservation(ctx, key, first.Ownership, Observation{ID: "before-bootstrap", Kind: "paper_trade", Payload: raw(`{"retained":true}`)})
	if err != nil {
		t.Fatal(err)
	}
	before := mustAccount(t, s, key)
	recovered, err := s.ClaimPaperOwnership(ctx, key, "second-paper-owner", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Ownership.Epoch != first.Ownership.Epoch+1 || recovered.PaperTransportContract != PaperTransportContract ||
		string(recovered.Snapshot) != "{}" || recovered.ObservedSequence != sequence || recovered.ProcessedSequence != 0 ||
		recovered.PendingObservationBytes != before.PendingObservationBytes || !reflect.DeepEqual(recovered.Balances, before.Balances) {
		t.Fatalf("crash before first runtime snapshot lost committed state: %+v", recovered)
	}
	if _, err := s.RecordPaperObservation(ctx, key, first.Ownership, Observation{ID: "late-old-bootstrap", Kind: "transport", Payload: raw(`{}`)}); !errors.Is(err, ErrOwnership) {
		t.Fatalf("old pre-bootstrap sender was not fenced: %v", err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE accounts SET paper_transport_contract='unknown-paper-contract' WHERE tenant_id=$1 AND account_id=$2`, key.TenantID, key.AccountID); err != nil {
		t.Fatal(err)
	}
	unknown := mustAccount(t, s, key)
	if _, err := s.ClaimPaperOwnership(ctx, key, "unsupported-owner", time.Minute); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown paper contract was accepted: %v", err)
	}
	if after := mustAccount(t, s, key); !reflect.DeepEqual(unknown, after) {
		t.Fatalf("unsupported paper claim changed durable state: %+v", after)
	}
}
