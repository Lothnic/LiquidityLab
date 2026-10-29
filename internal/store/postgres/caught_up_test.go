package postgres

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRequireCaughtUpRejectsDebtAndPreservesOriginalReceipt(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	key, owner, account := initialized(t, s)
	change := Change{Key: key, Ownership: owner, Command: Command{ID: "caught-up-create", Kind: "create", ExpectedRevision: account.Revision}, Snapshot: raw(`{"guarded":true}`), RequireCaughtUp: true}
	first, err := s.Apply(ctx, change)
	if err != nil {
		t.Fatal(err)
	}
	recordObservation(t, s, key, "after-receipt", "balance", `{"free":"900"}`)
	duplicate, err := s.Apply(ctx, change)
	if err != nil || !duplicate.Duplicate || duplicate.Revision != first.Revision {
		t.Fatalf("debt hid immutable original receipt: %+v %v", duplicate, err)
	}
	before := mustAccount(t, s, key)
	change.Command = Command{ID: "create-over-debt", Kind: "create", ExpectedRevision: before.Revision}
	change.Snapshot = raw(`{"must_not_replace_parent":true}`)
	if _, err := s.Apply(ctx, change); !errors.Is(err, ErrNotReady) {
		t.Fatalf("debt guard error=%v", err)
	}
	after := mustAccount(t, s, key)
	if after.Revision != before.Revision || string(after.Snapshot) != string(before.Snapshot) || after.ProcessedSequence != before.ProcessedSequence {
		t.Fatalf("rejected create changed projection: before=%+v after=%+v", before, after)
	}
}

func TestPaperTradeFirstAcceptanceRequiresCaughtUpRevisionButRetryRemainsFrozen(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	key := Key{TenantID: "trade-guard-tenant", AccountID: "trade-guard-account"}
	paperAccount(t, s, key)
	account, err := s.ClaimPaperOwnership(ctx, key, "trade-guard-owner", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	trade := Observation{ID: "first-trade", Kind: "paper_trade", Payload: raw(`{"intent_id":"original-child","queue_ahead":"0.3"}`)}
	if _, err := s.RecordPaperTradeObservation(ctx, key, account.Ownership, trade, account.Revision-1); !errors.Is(err, ErrStaleRevision) {
		t.Fatalf("stale matching state accepted: %v", err)
	}
	sequence, err := s.RecordPaperTradeObservation(ctx, key, account.Ownership, trade, account.Revision)
	if err != nil || sequence != 1 {
		t.Fatalf("first acceptance sequence=%d err=%v", sequence, err)
	}
	next := trade
	next.ID = "new-trade-before-reduction"
	if _, err := s.RecordPaperTradeObservation(ctx, key, account.Ownership, next, account.Revision); !errors.Is(err, ErrNotReady) {
		t.Fatalf("new identity overtook predecessor: %v", err)
	}
	// Both stale revision and source debt are irrelevant to an exact retry: the
	// already accepted outcome must remain discoverable, without rematching.
	if repeated, err := s.RecordPaperTradeObservation(ctx, key, account.Ownership, trade, account.Revision-1); err != nil || repeated != sequence {
		t.Fatalf("frozen retry sequence=%d err=%v", repeated, err)
	}
	changed := trade
	changed.Payload = raw(`{"intent_id":"replacement-child","queue_ahead":"0"}`)
	if _, err := s.RecordPaperTradeObservation(ctx, key, account.Ownership, changed, account.Revision); !errors.Is(err, ErrConflict) {
		t.Fatalf("rematched identity accepted: %v", err)
	}
	_, err = s.Apply(ctx, Change{Key: key, Ownership: account.Ownership, Command: Command{ID: "consume-first-trade", Kind: "reduce", ExpectedRevision: account.Revision}, Snapshot: account.Snapshot, ConsumeObservationThrough: sequence})
	if err != nil {
		t.Fatal(err)
	}
	current := mustAccount(t, s, key)
	if _, err := s.RecordPaperTradeObservation(ctx, key, current.Ownership, next, account.Revision); !errors.Is(err, ErrStaleRevision) {
		t.Fatalf("caught-up but stale snapshot accepted: %v", err)
	}
	if sequence, err := s.RecordPaperTradeObservation(ctx, key, current.Ownership, next, current.Revision); err != nil || sequence != 2 {
		t.Fatalf("fresh next trade sequence=%d err=%v", sequence, err)
	}
}
